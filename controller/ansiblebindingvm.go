package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
func applyAnsibleBindingVM(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	child, err := convertAnsibleBindingVM(u)
	if err != nil {
		return fmt.Errorf("decoding AnsibleBindingVM: %w", err)
	}
	if child.Spec == nil {
		return fmt.Errorf("spec is required")
	}
	if child.Spec.VMName == "" {
		return fmt.Errorf("spec.vmName is required")
	}
	if child.Spec.BindingName == "" {
		return fmt.Errorf("spec.bindingName is required")
	}
	if child.Spec.AWXConnectionRef == "" {
		return fmt.Errorf("spec.awxConnectionRef is required")
	}

	prior, adopted, err := priorVMState(&child)
	if err != nil {
		return err
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

	// finish persists whatever this pass worked out and returns the
	// first error, if any. Every exit below goes through it: a VM whose
	// host upsert failed still has to record the host ID it already had,
	// or the next pass loses track of it.
	finish := func() error {
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
		return firstErr
	}

	awxConnObj, err := client.Resource(awxConnGVR).Namespace(child.Namespace).Get(ctx, child.Spec.AWXConnectionRef, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("fetching AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil {
		return fmt.Errorf("decoding AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
	}
	if awxConn.Spec == nil {
		return fmt.Errorf("AWXConnection %q has no spec", child.Spec.AWXConnectionRef)
	}
	token, err := getSecretValue(ctx, client, child.Namespace, awxConn.Spec.SecretRef, "token")
	if err != nil {
		return fmt.Errorf("reading AWX token from secret %q: %w", awxConn.Spec.SecretRef, err)
	}
	awxClient, _, err := awxClientFor(ctx, client, awxConn, token)
	if err != nil {
		return fmt.Errorf("preparing a client for AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
	}

	tmpl, err := resolveTemplate(ctx, awxClient, child.Spec.Template)
	if err != nil {
		return err
	}

	targetsHost := !child.Spec.UseDefaultLimit && tmpl.Inventory != nil

	// AWX silently drops launch fields the template isn't configured to
	// accept. Re-checked here rather than only on the binding because
	// this is where the launch actually happens: a template edited in
	// AWX between the parent's pass and this one must not quietly widen
	// the run to the whole inventory.
	if err := checkTemplateLaunchFields(tmpl, child.Spec.Template.Name, targetsHost, len(child.Spec.ExtraVars) > 0); err != nil {
		return err
	}

	vm, err := client.Resource(vmGVR).Namespace(child.Namespace).Get(ctx, child.Spec.VMName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The VM is gone, so this object is on its way out: the
			// garbage collector deletes it once the owner reference
			// resolves to nothing. Nothing to reconcile in the meantime.
			return nil
		}
		return fmt.Errorf("fetching VirtualMachine %q: %w", child.Spec.VMName, err)
	}

	// Poll any in-flight job first: its outcome doesn't depend on the
	// VM's current power state, so this must happen even if the VM has
	// since powered off.
	if prior.LastJobID != 0 && !isTerminalAWXStatus(prior.LastJobStatus) {
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

	ip, ready := vmReady(vm)
	st.ObservedIP = ip
	if !ready {
		// Pending means "never ran, waiting on the VM" - a VM that
		// already has a run keeps that run's phase.
		if st.LastJobID == 0 {
			st.Phase = PhasePending
		}
		return finish()
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

	if !needsRun(st, child.Spec) {
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
	return finish()
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

	awxConnObj, err := client.Resource(awxConnGVR).Namespace(child.Namespace).Get(ctx, child.Spec.AWXConnectionRef, metav1.GetOptions{})
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
	token, err := getSecretValue(ctx, client, child.Namespace, awxConn.Spec.SecretRef, "token")
	if err != nil {
		if !isPermanent(err) {
			return fmt.Errorf("reading the AWX token to clean up AWX host %d: %w", child.Status.AWXHostID, err)
		}
		abandon("the AWX token is gone", err)
		return nil
	}
	// A base path that will not resolve means AWX is unreachable right
	// now - exactly the transient case worth retrying rather than
	// leaking a host over.
	awxClient, _, err := awxClientFor(ctx, client, awxConn, token)
	if err != nil {
		return fmt.Errorf("resolving the AWX API base path to clean up AWX host %d "+
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
