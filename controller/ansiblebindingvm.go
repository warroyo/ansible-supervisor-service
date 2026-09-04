package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// vmDetailsFieldManager owns everything in an AnsibleBindingVM's status
// except the generic state/message/ready/lastUpdated the engine writes,
// for the same reason detailsFieldManager exists on the binding: two
// field managers server-side-applying disjoint field sets merge, where
// one manager writing both would drop whichever half it left out.
const vmDetailsFieldManager = "ansible-supervisor-vm-details"

// applyAnsibleBindingVM reconciles one VM's inventory host and run.
//
// This is the body of what used to be the per-VM loop inside
// applyAnsibleBinding, with one difference that matters: it operates on
// one VM, so the work is bounded no matter how many VMs a binding
// selects, and the workqueue can run several of these at once.
func applyAnsibleBindingVM(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) (Result, error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return Result{}, err
	}
	child, err := convertAnsibleBindingVM(u)
	if err != nil {
		return Result{}, fmt.Errorf("decoding AnsibleBindingVM: %w", err)
	}
	if child.Spec == nil {
		return Result{}, fmt.Errorf("spec is required")
	}
	if child.Spec.VMName == "" {
		return Result{}, fmt.Errorf("spec.vmName is required")
	}
	if child.Spec.BindingName == "" {
		return Result{}, fmt.Errorf("spec.bindingName is required")
	}
	if child.Spec.AWXConnectionRef == "" {
		return Result{}, fmt.Errorf("spec.awxConnectionRef is required")
	}
	if err := checkOwnedByItsVM(&child); err != nil {
		return Result{}, err
	}

	prior, adopted, err := priorVMState(&child)
	if err != nil {
		return Result{}, err
	}

	// st is what this pass will write. It starts as what the object
	// already says, so a field this pass has no opinion about is carried
	// forward rather than blanked.
	st := prior
	st.History = append([]VMRunHistoryEntry(nil), prior.History...)

	var firstErr error
	recordErr := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	// requeueAfter is what this pass asks the engine for on its way out.
	// The resync wakes every child anyway; this is what lets the host
	// check run on its own period rather than the resync's.
	var requeueAfter time.Duration

	// finish persists whatever this pass worked out and returns the
	// first error, if any. Every exit below goes through it: a VM whose
	// host upsert failed still has to record the host ID it already had,
	// or the next pass loses track of it.
	finish := func() (Result, error) {
		if !ansibleBindingVMDetailsCurrent(child.Status, st) {
			if wErr := writeAnsibleBindingVMDetails(ctx, client, u, st); wErr != nil {
				log.Printf("[AnsibleBindingVM/%s/%s] failed to persist status: %v", child.Namespace, child.Name, wErr)
				recordErr(wErr)
			}
		}
		if adopted && firstErr == nil {
			if cErr := clearAdoptAnnotation(ctx, client, u); cErr != nil {
				log.Printf("[AnsibleBindingVM/%s/%s] failed to clear the adopt-status annotation: %v", child.Namespace, child.Name, cErr)
			}
		}
		return Result{Object: childWithStatus(u, st), RequeueAfter: requeueAfter}, firstErr
	}

	// The VirtualMachine is the one object below the workqueue still
	// read from the API server on every pass - there is no VM informer
	// yet, deliberately, since caching every VM on the Supervisor costs
	// memory proportional to the whole cluster.
	vm, err := client.Resource(vmGVR).Namespace(child.Namespace).Get(ctx, child.Spec.VMName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The VM is gone, so this object is on its way out: the
			// garbage collector deletes it once the owner reference
			// resolves to nothing. Nothing to reconcile in the meantime.
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("fetching VirtualMachine %q: %w", child.Spec.VMName, err)
	}

	ip, ready := vmReady(vm)
	st.ObservedIP = ip

	// Everything from here on costs AWX requests, so the three questions
	// that decide whether any of it is needed are answered first, from
	// what is already in hand. All three have to be quiet for the pass
	// to stop: a job still in flight takes the full path however idle
	// the rest of it looks.
	inFlight := prior.LastJobID != 0 && !isTerminalAWXStatus(prior.LastJobStatus)
	wantsRun := needsRun(st, child.Spec)
	hostCheckDue, untilNextHostCheck := dueFor(st.LastHostCheck, hostCheckPeriod)

	if !inFlight && !ready {
		// No address means no host to write and nothing to run against.
		// Pending means "never ran, waiting on the VM" - a VM that
		// already has a run keeps that run's phase.
		if st.LastJobID == 0 {
			st.Phase = PhasePending
		}
		return finish()
	}

	if !inFlight && !wantsRun && !hostCheckDue {
		requeueAfter = untilNextHostCheck
		return finish()
	}

	awxConnObj, err := getAWXConnection(ctx, client, child.Namespace, child.Spec.AWXConnectionRef)
	if err != nil {
		return Result{}, fmt.Errorf("fetching AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil {
		return Result{}, fmt.Errorf("decoding AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
	}
	if awxConn.Spec == nil {
		return Result{}, fmt.Errorf("AWXConnection %q has no spec", child.Spec.AWXConnectionRef)
	}
	awxClient, _, err := awxClientForConnection(ctx, client, awxConn)
	if err != nil {
		return Result{}, fmt.Errorf("preparing a client for AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
	}

	// Poll any in-flight job first: its outcome doesn't depend on the
	// VM's current power state, so this must happen even if the VM has
	// since powered off - and it needs no template lookup.
	if inFlight {
		status, sErr := pollJobStatus(ctx, awxClient, child.Spec.Template.Type, prior.LastJobID)
		if sErr != nil {
			st.Phase = PhaseRunning
			recordErr(fmt.Errorf("polling job %d: %w", prior.LastJobID, sErr))
			return finish()
		}
		st.LastJobStatus = status
		st.Phase = mapAWXStatus(status)
		if !isTerminalAWXStatus(status) {
			// Still running: don't relaunch, and leave appliedGeneration
			// and appliedTrigger alone so a re-run requested mid-flight
			// is honored once it finishes.
			st.History = upsertHistory(st.History, VMRunHistoryEntry{JobID: prior.LastJobID, Status: status})
			return finish()
		}
		st.History = upsertHistory(st.History, VMRunHistoryEntry{
			JobID: prior.LastJobID, Status: status, FinishedAt: nowRFC3339(),
		})
	}

	// The job that was in flight has just finished, and nothing else is
	// asking for AWX this pass.
	if !ready || (!wantsRun && !hostCheckDue) {
		if !ready && st.LastJobID == 0 {
			st.Phase = PhasePending
		}
		if ready {
			requeueAfter = untilNextHostCheck
		}
		return finish()
	}

	// A launch resolves the template from AWX every time, never from the
	// cache: ask_limit_on_launch is what stops a run going against the
	// whole inventory, and it can be switched off in the AWX UI between
	// one pass and the next.
	var tmpl *AWXTemplate
	if wantsRun {
		tmpl, err = resolveTemplateForLaunch(ctx, awxClient, child.Namespace, child.Spec.AWXConnectionRef, child.Spec.Template)
	} else {
		tmpl, err = resolveTemplateCached(ctx, awxClient, child.Namespace, child.Spec.AWXConnectionRef, child.Spec.Template)
	}
	if err != nil {
		return Result{}, err
	}

	targetsHost := !child.Spec.UseDefaultLimit && tmpl.Inventory != nil

	// AWX silently drops launch fields the template isn't configured to
	// accept. Re-checked here rather than only on the binding because
	// this is where the launch actually happens: a template edited in
	// AWX between the parent's pass and this one must not quietly widen
	// the run to the whole inventory.
	if err := checkTemplateLaunchFields(tmpl, child.Spec.Template.Name, targetsHost, len(child.Spec.ExtraVars) > 0); err != nil {
		return Result{}, err
	}

	hostName := child.Spec.VMName
	if child.Spec.HostName != "" {
		hostName = child.Spec.HostName
	}
	hostName = awxConn.Spec.HostNamePrefix + hostName

	// The ownership marker is keyed to the binding, not to this object,
	// so a host provisioned before the split is adopted here unchanged
	// rather than being seen as someone else's and refused.
	ownerMarker := hostOwnerMarker(child.Namespace, child.Spec.BindingName)

	cleanupPolicy := child.Spec.CleanupPolicy
	if cleanupPolicy == "" {
		cleanupPolicy = CleanupPolicyDelete
	}

	if tmpl.Inventory != nil {
		inventoryID := int64(*tmpl.Inventory)

		// A renamed host (changed hostNamePrefix or spec.hostName), or a
		// template repointed at a different inventory, would otherwise
		// orphan the old entry - under the old name, or in the old
		// inventory - while a second one appears alongside it.
		renamed := st.AWXHostName != "" && st.AWXHostName != hostName
		moved := st.AWXInventoryID != 0 && st.AWXInventoryID != inventoryID
		if st.AWXHostID != 0 && (renamed || moved) {
			if st.AWXHostCreated && cleanupPolicy == CleanupPolicyDelete {
				if dErr := awxClient.DeleteHost(ctx, int(st.AWXHostID)); dErr != nil {
					// Leave the recorded host in place and retry, rather
					// than losing track of it.
					recordErr(fmt.Errorf("retiring AWX host %q: %w", st.AWXHostName, dErr))
					return finish()
				}
			}
			st.AWXHostID = 0
			st.AWXHostCreated = false
			st.AWXInventoryID = 0
		}

		hostVars := map[string]string{"ansible_host": ip}
		for k, v := range child.Spec.HostVariables {
			hostVars[k] = v
		}

		// Reconcile the host against AWX itself on every pass, rather
		// than trusting what status says was pushed last time. A host
		// deleted or edited in the AWX UI is drift like any other, and
		// status cannot see it: the run would then fail forever with
		// "--limit does not match any hosts", or quietly run against
		// hand-edited variables, and nothing would ever repair it.
		hostID, owned, hErr := awxClient.UpsertHost(ctx, *tmpl.Inventory, hostName, ownerMarker, hostVars)
		if hErr != nil {
			st.Phase = PhaseFailed
			recordErr(fmt.Errorf("upserting AWX host: %w", hErr))
			return finish()
		}
		st.AWXHostID = int64(hostID)
		st.AWXHostName = hostName
		st.AWXHostCreated = owned
		st.AWXInventoryID = inventoryID
	}

	// Reaching here means the host is as AWX should have it (or the
	// template has no inventory and there is no host to keep). Recording
	// when that was true is what lets the next several passes skip AWX
	// entirely.
	st.LastHostCheck = nowRFC3339()

	if !wantsRun {
		requeueAfter = hostCheckPeriod
		return finish()
	}

	var limit string
	if targetsHost {
		limit = hostName
	}
	var jobID int
	var lErr error
	if child.Spec.Template.Type == TemplateTypeWorkflow {
		jobID, lErr = awxClient.LaunchWorkflowJobTemplate(ctx, tmpl.ID, limit, child.Spec.ExtraVars)
	} else {
		jobID, lErr = awxClient.LaunchJobTemplate(ctx, tmpl.ID, limit, child.Spec.ExtraVars)
	}
	if jobID != 0 {
		// Record the run even when lErr is set (AWX ignored fields): the
		// job is real and running, and must stay traceable.
		st.LastJobID = int64(jobID)
		st.LastJobURL = awxClient.JobURL(jobID, child.Spec.Template.Type == TemplateTypeWorkflow)
		st.LastJobStatus = "pending"
		st.Phase = PhaseRunning
		st.History = upsertHistory(st.History, VMRunHistoryEntry{JobID: int64(jobID), Status: "pending", StartedAt: nowRFC3339()})
		st.AppliedGeneration = child.Spec.BindingGeneration
		st.AppliedTrigger = child.Spec.BindingTrigger
	}
	if lErr != nil {
		if jobID == 0 {
			st.Phase = PhaseFailed
		}
		recordErr(fmt.Errorf("launching template: %w", lErr))
	}
	requeueAfter = hostCheckPeriod
	return finish()
}

// checkOwnedByItsVM refuses to reconcile a child that is not owned by
// the VirtualMachine it claims to be for.
//
// Without this, an AnsibleBindingVM written by hand reconciles happily:
// it creates AWX hosts and launches jobs, nothing garbage-collects it
// (no ownerReference) and no parent reaps it (no binding label). Because
// spec.bindingName keys the AWX host ownership marker, one could also be
// pointed at another binding's marker to interfere with its hosts. The
// user-facing roles are read-only on this kind, so this closes the gap a
// cluster-admin could still walk through.
//
// The presence of an ownerReference is not the check. A reference to a
// deleted VM sits in the list until the garbage collector gets to the
// object, and a reference to some unrelated live VM would pass just as
// easily. The reference has to name this child's own VM.
func checkOwnedByItsVM(child *AnsibleBindingVM) error {
	for _, ref := range child.OwnerReferences {
		if ref.Kind != "VirtualMachine" || ref.Name != child.Spec.VMName {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != vmGroup {
			continue
		}
		return nil
	}
	return fmt.Errorf("AnsibleBindingVM %q has no ownerReference to VirtualMachine %q: "+
		"children are created by their AnsibleBinding, not written by hand: %w",
		child.Name, child.Spec.VMName, errPermanentConfig)
}

// childWithStatus is the child as this pass leaves it: the object it was
// given, with the status just written merged in, so the engine can
// derive the generic state from it without re-reading the object.
//
// The engine's own field manager owns state/message/ready/lastUpdated,
// so those are carried across from the object rather than recomputed
// here; everything else is replaced wholesale, exactly as the
// server-side apply does, so a field that fell away actually disappears.
func childWithStatus(u *unstructured.Unstructured, st AnsibleBindingVMStatus) *unstructured.Unstructured {
	out := u.DeepCopy()
	details, err := structToMap(vmDetailsOf(st))
	if err != nil {
		return out
	}
	status := map[string]interface{}{}
	if existing, found, _ := unstructured.NestedMap(out.Object, "status"); found {
		for _, generic := range []string{"state", "message", "ready", "lastUpdated"} {
			if v, ok := existing[generic]; ok {
				status[generic] = v
			}
		}
	}
	for k, v := range details {
		status[k] = v
	}
	if err := unstructured.SetNestedMap(out.Object, status, "status"); err != nil {
		return u.DeepCopy()
	}
	return out
}

// needsRun decides whether this VM should launch, comparing what it last
// ran against the binding generation and re-run trigger the parent
// copied into this spec. The comparison is per VM, so a spec change or
// re-run request is never consumed on behalf of a VM that did not act on
// it - a job still in flight, or a powered-off VM.
//
// It is also what makes the migration safe. A child seeded from a
// pre-split binding carries that VM's previous appliedGeneration, so
// this returns false and the upgrade launches nothing. A child created
// without the seed would have a zero LastJobID here and re-run every
// playbook in the fleet at once.
func needsRun(st AnsibleBindingVMStatus, spec *AnsibleBindingVMSpec) bool {
	return st.LastJobID == 0 ||
		st.AppliedGeneration != spec.BindingGeneration ||
		st.AppliedTrigger != spec.BindingTrigger
}

// resolveTemplate looks up the AWX template a spec names.
func resolveTemplate(ctx context.Context, awxClient *AWXClient, ref TemplateRef) (*AWXTemplate, error) {
	var tmpl *AWXTemplate
	var err error
	switch ref.Type {
	case TemplateTypeJob:
		tmpl, err = awxClient.FindJobTemplate(ctx, ref.Name)
	case TemplateTypeWorkflow:
		tmpl, err = awxClient.FindWorkflowJobTemplate(ctx, ref.Name)
	default:
		return nil, fmt.Errorf("spec.template.type must be %q or %q, got %q", TemplateTypeJob, TemplateTypeWorkflow, ref.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving template %q: %w", ref.Name, err)
	}
	return tmpl, nil
}

// checkTemplateLaunchFields refuses a launch AWX would silently narrow
// or widen. A dropped limit means the template runs against its ENTIRE
// inventory rather than the targeted VM.
func checkTemplateLaunchFields(tmpl *AWXTemplate, name string, targetsHost, hasExtraVars bool) error {
	if targetsHost && !tmpl.AskLimitOnLaunch {
		return fmt.Errorf("template %q does not accept a limit at launch time (ask_limit_on_launch is false), "+
			"so AWX would ignore the per-VM limit and run against the whole inventory: enable Prompt on Launch for Limit in AWX, "+
			"or set spec.useDefaultLimit: true to accept the template's own scope", name)
	}
	if hasExtraVars && !tmpl.AskVariablesOnLaunch {
		return fmt.Errorf("template %q does not accept extra variables at launch time (ask_variables_on_launch is false), "+
			"so AWX would ignore spec.extraVars: enable Prompt on Launch for Variables in AWX, or remove spec.extraVars", name)
	}
	return nil
}

// priorVMState returns the status this pass should build on, and whether
// it came from the migration annotation rather than from status itself.
//
// A child migrated from a pre-split binding is created with its prior
// state in an annotation, because status is a subresource and cannot be
// set at creation time. Reading it here - before any launch decision -
// is what stops the upgrade re-running every playbook: the child has its
// previous appliedGeneration in hand on its very first pass.
func priorVMState(child *AnsibleBindingVM) (AnsibleBindingVMStatus, bool, error) {
	if child.Status != nil && (child.Status.AWXHostID != 0 || child.Status.LastJobID != 0 || child.Status.Phase != "") {
		return *child.Status, false, nil
	}
	raw, ok := child.Annotations[AdoptStatusAnnotation]
	if !ok || raw == "" {
		if child.Status != nil {
			return *child.Status, false, nil
		}
		return AnsibleBindingVMStatus{}, false, nil
	}
	var seeded AnsibleBindingVMStatus
	if err := json.Unmarshal([]byte(raw), &seeded); err != nil {
		return AnsibleBindingVMStatus{}, false, fmt.Errorf("decoding the %s annotation: %w: %w", AdoptStatusAnnotation, err, errPermanentConfig)
	}
	return seeded, true, nil
}

// clearAdoptAnnotation removes the migration seed once it has been
// persisted into status, so it cannot be replayed over newer state.
func clearAdoptAnnotation(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, AdoptStatusAnnotation))
	_, err := client.Resource(ansBindVMGVR).Namespace(obj.GetNamespace()).Patch(
		ctx, obj.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ansibleBindingVMDetailsCurrent reports whether status already says
// exactly what this pass computed, so an idle child writes nothing.
func ansibleBindingVMDetailsCurrent(prior *AnsibleBindingVMStatus, next AnsibleBindingVMStatus) bool {
	if prior == nil {
		return false
	}
	a := vmDetailsOf(*prior)
	b := vmDetailsOf(next)
	return reflect.DeepEqual(a, b)
}

// vmDetailsOf strips the fields the engine's own field manager owns, so
// comparing two statuses compares only what this file writes.
func vmDetailsOf(s AnsibleBindingVMStatus) AnsibleBindingVMStatus {
	s.State, s.Message, s.Ready, s.LastUpdated = "", "", false, ""
	return s
}

func writeAnsibleBindingVMDetails(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, st AnsibleBindingVMStatus) error {
	m, err := structToMap(vmDetailsOf(st))
	if err != nil {
		return fmt.Errorf("encoding status: %w", err)
	}
	return patchStatus(ctx, client, ansBindVMGVR, obj, m, vmDetailsFieldManager)
}

// cleanupAnsibleBindingVM deletes the AWX inventory host this object
// created, unless cleanupPolicy is Retain, before its finalizer is
// released.
//
// This is cleanupAnsibleBinding's job reduced to one host. That is the
// point of the split: the work a single finalizer has to complete no
// longer scales with how many VMs a binding selects, so it cannot run
// out of reconcile time part-way through and restart from the beginning
// on the next pass.
//
// Failures are returned so the delete is retried rather than leaking the
// host. Only a genuinely unrecoverable situation - the AWXConnection or
// its Secret is gone, or is malformed in a way no retry can fix - is
// logged and skipped, since blocking the delete forever would not bring
// the host back either.
func cleanupAnsibleBindingVM(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return nil
	}
	child, err := convertAnsibleBindingVM(u)
	if err != nil || child.Spec == nil || child.Status == nil {
		return nil
	}
	if child.Spec.CleanupPolicy == CleanupPolicyRetain {
		return nil
	}
	if child.Status.AWXHostID == 0 || !child.Status.AWXHostCreated {
		return nil
	}

	abandon := func(reason string, err error) {
		log.Printf("[AnsibleBindingVM/%s/%s] cleanup: %s, abandoning AWX host %d: %v",
			child.Namespace, child.Name, reason, child.Status.AWXHostID, err)
	}

	awxConnObj, err := getAWXConnection(ctx, client, child.Namespace, child.Spec.AWXConnectionRef)
	if err != nil {
		if !isPermanent(err) {
			return fmt.Errorf("fetching AWXConnection %q to clean up AWX host %d: %w", child.Spec.AWXConnectionRef, child.Status.AWXHostID, err)
		}
		abandon(fmt.Sprintf("AWXConnection %q is gone", child.Spec.AWXConnectionRef), err)
		return nil
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil || awxConn.Spec == nil {
		abandon(fmt.Sprintf("AWXConnection %q is malformed", child.Spec.AWXConnectionRef), err)
		return nil
	}
	// A missing token, or a base path that will not resolve, is the
	// difference between "this will never work" and "AWX is unreachable
	// right now" - and the second is worth retrying rather than leaking
	// a host over.
	awxClient, _, err := awxClientForConnection(ctx, client, awxConn)
	if err != nil {
		if isPermanent(err) {
			abandon("the AWX token is gone or the connection is malformed", err)
			return nil
		}
		return fmt.Errorf("preparing a client to clean up AWX host %d "+
			"(set spec.cleanupPolicy: Retain to release this object and leave it in place): %w", child.Status.AWXHostID, err)
	}
	if err := awxClient.DeleteHost(ctx, int(child.Status.AWXHostID)); err != nil {
		return fmt.Errorf("deleting AWX host %d: %w", child.Status.AWXHostID, err)
	}
	return nil
}

// updateAnsibleBindingVMStatus derives the generic state from the run
// phase, for the same reason the binding needs its own: an AWX job that
// ran and failed is not a reconcile error, so the engine's default
// updater would report the object Ready while the playbook under it was
// failing.
func updateAnsibleBindingVMStatus(u *unstructured.Unstructured, success bool, reconcileErr error) map[string]interface{} {
	if reconcileErr != nil || !success {
		return updateGenericStatus(u, success, reconcileErr)
	}

	status := func(state, message string, ready bool) map[string]interface{} {
		return map[string]interface{}{
			"state":       state,
			"message":     message,
			"ready":       ready,
			"lastUpdated": metav1.Now(),
		}
	}

	child, err := convertAnsibleBindingVM(u)
	if err != nil {
		return status("Pending", fmt.Sprintf("Could not read run status: %s", err), false)
	}
	if child.Status == nil {
		return status("Pending", "Waiting for the VirtualMachine to report an address.", false)
	}
	switch child.Status.Phase {
	case PhaseSucceeded:
		return status("Ready", fmt.Sprintf("Job %d completed successfully.", child.Status.LastJobID), true)
	case PhaseFailed:
		return status("Failed", fmt.Sprintf("Job %d failed.", child.Status.LastJobID), false)
	case PhaseRunning:
		return status("Running", fmt.Sprintf("Job %d is still running.", child.Status.LastJobID), false)
	default:
		return status("Pending", "VirtualMachine is not ready to run (powered off, or no reported address).", false)
	}
}
