package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// The object's name is the claim, so an object sitting somewhere
	// else is not one - and must not reach AWX. Without this a child
	// created by hand under any other name would provision a VM whose
	// canonical claim belongs to a different binding, which is the one
	// thing the naming exists to prevent.
	if canonical := childName(child.Spec.VMName); u.GetName() != canonical {
		return Result{}, fmt.Errorf("this object is not the claim on VirtualMachine %q: that is %q. "+
			"AnsibleBindingVMs are created by the binding controller, not by hand", child.Spec.VMName, canonical)
	}
	if child.Spec.AWXConnectionRef == "" {
		return Result{}, fmt.Errorf("spec.awxConnectionRef is required")
	}
	ownerUID, err := checkOwnedByItsVM(&child)
	if err != nil {
		return Result{}, err
	}

	prior := priorVMState(&child)

	// st is what this pass will write. It starts as what the object
	// already says, so a field this pass has no opinion about is carried
	// forward rather than blanked.
	st := prior
	st.History = append([]VMRunHistoryEntry(nil), prior.History...)
	// Run outcome and reconciliation errors are separate: repairing a
	// host must not permanently turn a successful job into a failed one.
	if st.LastJobID != 0 {
		st.Phase = mapAWXStatus(st.LastJobStatus)
	}

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

	// The owner reference resolves by UID, and so must this: a VM deleted
	// and recreated under the same name is a different object, and until
	// the garbage collector catches up this child still exists alongside
	// it. Acting on the name alone would point the old VM's inventory
	// host - and its playbook run - at the new VM.
	if uid := string(vm.GetUID()); ownerUID != "" && uid != ownerUID {
		log.Printf("[AnsibleBindingVM/%s/%s] VirtualMachine %q is now UID %s, not the %s this child was created for: leaving it for garbage collection",
			child.Namespace, child.Name, child.Spec.VMName, uid, ownerUID)
		return Result{}, nil
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

	// Poll any in-flight job first: its outcome doesn't depend on the
	// VM's current power state, so this must happen even if the VM has
	// since powered off - and it needs no template lookup.
	if inFlight {
		status, sErr := pollRecordedJob(ctx, client, &child, prior)
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

	awxConnObj, err := getAWXConnection(ctx, client, child.Namespace, child.Spec.AWXConnectionRef)
	if err != nil {
		recordErr(fmt.Errorf("fetching AWXConnection %q: %w", child.Spec.AWXConnectionRef, err))
		return finish()
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil {
		recordErr(fmt.Errorf("decoding AWXConnection %q: %w", child.Spec.AWXConnectionRef, err))
		return finish()
	}
	if awxConn.Spec == nil {
		recordErr(fmt.Errorf("AWXConnection %q has no spec", child.Spec.AWXConnectionRef))
		return finish()
	}
	awxClient, basePath, err := awxClientForConnection(ctx, client, awxConn)
	if err != nil {
		recordErr(fmt.Errorf("preparing a client for AWXConnection %q: %w", child.Spec.AWXConnectionRef, err))
		return finish()
	}

	// Everything recorded below - host id, inventory id - is an id issued
	// by one AWX instance. Point the connection somewhere else and those
	// ids belong to whatever unrelated objects hold them there, so drop
	// them rather than act on them: the host is then looked up by name on
	// the new instance and adopted or created like any other.
	endpoint := awxEndpointFingerprint(awxConn.Spec.URL, basePath)
	if st.AWXEndpoint != "" && st.AWXEndpoint != endpoint {
		log.Printf("[AnsibleBindingVM/%s/%s] AWXConnection %q now points at a different AWX instance: forgetting host %d in inventory %d rather than acting on ids from the old one",
			child.Namespace, child.Name, child.Spec.AWXConnectionRef, st.AWXHostID, st.AWXInventoryID)
		st.AWXHostID, st.AWXInventoryID, st.AWXHostName, st.AWXHostCreated = 0, 0, "", false
	}
	st.AWXEndpoint = endpoint

	// A launch resolves the template from AWX every time, never from the
	// cache: ask_limit_on_launch is what stops a run going against the
	// whole inventory, and it can be switched off in the AWX UI between
	// one pass and the next.
	var tmpl *AWXTemplate
	connKey := connectionKey(awxConn)
	if wantsRun {
		tmpl, err = resolveTemplateForLaunch(ctx, awxClient, connKey, child.Spec.Template)
	} else {
		tmpl, err = resolveTemplateCached(ctx, awxClient, connKey, child.Spec.Template)
	}
	if err != nil {
		recordErr(err)
		return finish()
	}

	targetsHost := !child.Spec.UseDefaultLimit && tmpl.Inventory != nil

	// AWX silently drops launch fields the template isn't configured to
	// accept. Re-checked here rather than only on the binding because
	// this is where the launch actually happens: a template edited in
	// AWX between the parent's pass and this one must not quietly widen
	// the run to the whole inventory.
	if err := checkTemplateLaunchFields(tmpl, child.Spec.Template.Name, targetsHost, len(child.Spec.ExtraVars) > 0); err != nil {
		recordErr(err)
		return finish()
	}

	hostName := child.Spec.VMName
	if child.Spec.HostName != "" {
		hostName = child.Spec.HostName
	}
	hostName = awxConn.Spec.HostNamePrefix + hostName

	// The ownership marker is keyed to the binding, not to this object,
	// so a host survives its child being deleted and recreated - by a
	// relabelled VM, say - and is adopted rather than refused as someone
	// else's.
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
		// A previous upsert may have reached AWX before its ID reached
		// status. Recover it before moving to a different name/inventory.
		if st.AWXInventoryID != 0 && st.AWXHostName != "" && (renamed || moved) {
			host, err := awxClient.FindHost(ctx, int(st.AWXInventoryID), st.AWXHostName)
			if err != nil {
				recordErr(fmt.Errorf("recovering previous AWX host: %w", err))
				return finish()
			}
			st.AWXHostID, st.AWXHostCreated = 0, false
			if host != nil {
				st.AWXHostID = int64(host.ID)
				st.AWXHostCreated = strings.TrimSpace(host.Description) == ownerMarker
			}
		}
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
		if st.AWXHostID == 0 {
			// Persist the lookup coordinates BEFORE creating anything in
			// AWX. Finalization can recover a created host even if the
			// process dies before saving the returned host ID.
			st.AWXHostName, st.AWXInventoryID = hostName, inventoryID
			st.AWXHostCreated = false
			// Only when it says something new. A child whose host cannot
			// be created - AWX down - reaches here on every host check,
			// and rewriting the same intent each time would put back a
			// per-pass round trip.
			if !ansibleBindingVMDetailsCurrent(child.Status, st) {
				if err := writeAnsibleBindingVMDetails(ctx, client, u, st); err != nil {
					recordErr(fmt.Errorf("recording AWX host intent: %w", err))
					return finish()
				}
			}
		}
		hostID, owned, hErr := awxClient.UpsertHost(ctx, *tmpl.Inventory, hostName, ownerMarker, hostVars)
		if hErr != nil {
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
		st.LastJobType = child.Spec.Template.Type
		connection := *awxConn.Spec
		connection.APIBasePath = basePath
		st.LastJobConnection = &connection
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

// pollRecordedJob uses the immutable launch settings, not the next run's
// desired spec. Only Secret references are recorded; token/CA contents are
// read afresh so credential rotation still works during a long-running job.
func pollRecordedJob(ctx context.Context, client *dynamic.DynamicClient, child *AnsibleBindingVM, st AnsibleBindingVMStatus) (string, error) {
	// A job launched before this controller recorded launch identity has
	// neither field, and a non-terminal job is never abandoned - so
	// refusing to poll it would wedge that child forever, and an upgrade
	// while any job was in flight would wedge one per running job. Poll
	// it the way the controller that launched it would have: with the
	// connection and template type the spec names now.
	if !hasRecordedLaunchIdentity(st) {
		log.Printf("[AnsibleBindingVM/%s/%s] job %d predates recorded launch identity: polling it with the current connection and template type",
			child.Namespace, child.Name, st.LastJobID)
		awxConnObj, err := getAWXConnection(ctx, client, child.Namespace, child.Spec.AWXConnectionRef)
		if err != nil {
			return "", fmt.Errorf("fetching AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
		}
		awxConn, err := convertAWXConnection(awxConnObj)
		if err != nil || awxConn.Spec == nil {
			return "", fmt.Errorf("decoding AWXConnection %q: %w", child.Spec.AWXConnectionRef, err)
		}
		awxClient, _, err := awxClientForConnection(ctx, client, awxConn)
		if err != nil {
			return "", err
		}
		return pollJobStatus(ctx, awxClient, child.Spec.Template.Type, st.LastJobID)
	}
	conn := AWXConnection{ObjectMeta: metav1.ObjectMeta{Namespace: child.Namespace, Name: child.Spec.AWXConnectionRef}, Spec: st.LastJobConnection}
	token, err := getSecretValue(ctx, client, child.Namespace, conn.Spec.SecretRef, "token")
	if err != nil {
		return "", fmt.Errorf("reading recorded job credential: %w", err)
	}
	awxClient, _, err := awxClientFor(ctx, client, conn, token)
	if err != nil {
		return "", err
	}
	return pollJobStatus(ctx, awxClient, st.LastJobType, st.LastJobID)
}

// hasRecordedLaunchIdentity reports whether a job's status says which AWX
// instance and which kind of template it was launched against. Without
// both, the id alone is not enough to poll it: the same number is a
// different job on a different instance, and /jobs/42/ and
// /workflow_jobs/42/ are different objects on the same one.
func hasRecordedLaunchIdentity(st AnsibleBindingVMStatus) bool {
	if st.LastJobConnection == nil || st.LastJobConnection.URL == "" || st.LastJobConnection.SecretRef == "" {
		return false
	}
	return st.LastJobType == TemplateTypeJob || st.LastJobType == TemplateTypeWorkflow
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
func checkOwnedByItsVM(child *AnsibleBindingVM) (string, error) {
	for _, ref := range child.OwnerReferences {
		if ref.Kind != "VirtualMachine" || ref.Name != child.Spec.VMName {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != vmGroup {
			continue
		}
		return string(ref.UID), nil
	}
	return "", fmt.Errorf("AnsibleBindingVM %q has no ownerReference to VirtualMachine %q: "+
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
// It is also what makes a controller restart safe: the decision is made
// from the child's own status rather than from anything the process
// remembers, so coming back up relaunches nothing.
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

// priorVMState is the status this pass builds on.
func priorVMState(child *AnsibleBindingVM) AnsibleBindingVMStatus {
	if child.Status == nil {
		return AnsibleBindingVMStatus{}
	}
	return *child.Status
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

// hookPollInterval is how often a running deprovision job is looked at
// again. Each look is one AWX request on a terminating object, so it is
// slow enough not to hammer AWX during a large teardown and fast enough
// that a short playbook does not visibly delay a delete.
const hookPollInterval = 15 * time.Second

// hookPollJitter is spread on top of hookPollInterval. A namespace
// deleted in one go starts every hook in the same second, and without
// jitter they would poll in lockstep for the whole teardown.
const hookPollJitter = 3 * time.Second

// cleanupAnsibleBindingVM runs this VM's onDeleted hook, if it has one
// and its VirtualMachine is genuinely gone, and then deletes the AWX
// inventory host it created, before its finalizer is released.
//
// This is cleanupAnsibleBinding's job reduced to one host and one
// playbook. That is the point of the split: the work a single finalizer
// has to complete no longer scales with how many VMs a binding selects,
// so it cannot run out of reconcile time part-way through and restart
// from the beginning on the next pass.
//
// It never blocks indefinitely. A hook that fails, times out, or cannot
// be launched at all is recorded and released rather than held: a
// deregistration playbook that will never succeed must not be able to
// hold a VM - and the namespace above it - in Terminating forever. What
// happened is written to the log, to an Event, and to the child's own
// status on the way out.
//
// Failures that a retry could fix are returned so the delete is retried
// rather than leaking the host. Only a genuinely unrecoverable situation
// - the AWXConnection or its Secret is gone, or is malformed in a way no
// retry can fix - is logged and skipped, since blocking the delete
// forever would not bring the host back either.
func cleanupAnsibleBindingVM(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) (CleanupResult, error) {
	done := CleanupResult{Done: true}

	u, err := toUnstructured(obj)
	if err != nil {
		return done, nil
	}
	child, err := convertAnsibleBindingVM(u)
	if err != nil || child.Spec == nil {
		return done, nil
	}
	hook := child.Spec.OnDeleted
	retain := child.Spec.CleanupPolicy == CleanupPolicyRetain
	if hook == nil && retain {
		// Nothing to run and nothing to delete.
		return done, nil
	}

	// A hook resumes from what the previous pass recorded, so it must not
	// read a status the informer has yet to catch up with: a stale copy
	// would say no job had been launched and launch a second decommission
	// run. Finalization with a hook is rare, and running a teardown
	// playbook twice is exactly the kind of damage a round trip is worth
	// avoiding - the same reasoning that makes orphan reaping read live.
	if hook != nil {
		fresh, gErr := client.Resource(ansBindVMGVR).Namespace(child.Namespace).Get(ctx, child.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(gErr):
			return done, nil
		case gErr != nil:
			return CleanupResult{}, fmt.Errorf("re-reading %s before running its deprovision hook: %w", child.Name, gErr)
		}
		refreshed, cErr := convertAnsibleBindingVM(fresh)
		if cErr == nil && refreshed.Spec != nil {
			u, child = fresh, refreshed
			hook = child.Spec.OnDeleted
			retain = child.Spec.CleanupPolicy == CleanupPolicyRetain
		}
	}

	st := priorVMState(&child)

	// The hook's clock starts here, before the AWXConnection is read and
	// before the inventory host is looked up, and is durable before
	// either of those runs. Both can fail transiently, and a deadline
	// that existed only in memory left nothing saying the hook was still
	// owed: under cleanupPolicy: Retain a single failed host lookup
	// released the finalizer, and the deregistration playbook never ran.
	if hook != nil {
		if err := startHookClock(ctx, client, u, &child, &st, hook); err != nil {
			return CleanupResult{}, err
		}
	}

	// An overdue hook is timed out here, before any of the AWX work
	// below, so that finishing it does not depend on AWX answering.
	if hook != nil && st.Deprovision != nil {
		if dep := st.Deprovision; !isTerminalHookPhase(dep.Phase) && overdue(dep.Deadline) {
			copied := *dep
			copied.Phase = PhaseTimedOut
			copied.Message = "The hook did not finish before spec.onDeleted.timeoutSeconds elapsed."
			st.Deprovision = &copied
			log.Printf("[AnsibleBindingVM/%s/%s] deprovision hook timed out after %s", child.Namespace, child.Name, dep.Deadline)
		}
	}

	abandon := func(reason string, err error) {
		log.Printf("[AnsibleBindingVM/%s/%s] cleanup: %s, abandoning AWX host %d: %v",
			child.Namespace, child.Name, reason, st.AWXHostID, err)
	}

	awxConnObj, err := getAWXConnection(ctx, client, child.Namespace, child.Spec.AWXConnectionRef)
	if err != nil {
		if !isPermanent(err) {
			return CleanupResult{}, fmt.Errorf("fetching AWXConnection %q to clean up AWX host %d: %w", child.Spec.AWXConnectionRef, st.AWXHostID, err)
		}
		abandon(fmt.Sprintf("AWXConnection %q is gone", child.Spec.AWXConnectionRef), err)
		return finishHookless(ctx, client, u, &child, st, hook, "the AWXConnection is gone"), nil
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil || awxConn.Spec == nil {
		abandon(fmt.Sprintf("AWXConnection %q is malformed", child.Spec.AWXConnectionRef), err)
		return finishHookless(ctx, client, u, &child, st, hook, "the AWXConnection is malformed"), nil
	}
	// A missing token, or a base path that will not resolve, is the
	// difference between "this will never work" and "AWX is unreachable
	// right now" - and the second is worth retrying rather than leaking
	// a host over.
	awxClient, basePath, err := awxClientForConnection(ctx, client, awxConn)
	if err != nil {
		if isPermanent(err) {
			abandon("the AWX token is gone or the connection is malformed", err)
			return finishHookless(ctx, client, u, &child, st, hook, "the AWX token is gone"), nil
		}
		return CleanupResult{}, fmt.Errorf("preparing a client to clean up AWX host %d "+
			"(set spec.cleanupPolicy: Retain to release this object and leave it in place): %w", st.AWXHostID, err)
	}
	// Deleting by id on an instance that did not issue that id would
	// delete some unrelated host. There is nothing to clean up here: the
	// host this object created is on the old instance, which the
	// connection no longer names.
	//
	// Every fingerprint this object recorded is checked, not just the
	// provisioning one: a hook can be the only thing that ever wrote a
	// host on this instance, and a child whose host was rediscovered
	// rather than remembered has no provisioning fingerprint at all.
	// Without that, a connection repointed mid-teardown got as far as
	// finding a same-named host on the new instance and deleting it.
	endpoint := awxEndpointFingerprint(awxConn.Spec.URL, basePath)
	if recorded := recordedAWXEndpoint(st); recorded != "" && recorded != endpoint {
		abandon(fmt.Sprintf("AWXConnection %q now points at a different AWX instance", child.Spec.AWXConnectionRef), nil)
		return finishHookless(ctx, client, u, &child, st, hook, "the AWXConnection now points at a different AWX instance"), nil
	}

	// A hook whose job is already running needs nothing but that job's
	// status. Everything below - the host lookup, the live
	// VirtualMachine read - was needed to authorize the launch, and none
	// of it can change the answer while the job runs, so an ordinary
	// poll skips it: one live child read and one AWX request per pass,
	// rather than two of each.
	//
	// The launch's own instance fingerprint is what makes this safe to
	// take, so a hook that recorded none takes the long way round.
	if dep := st.Deprovision; hook != nil && dep != nil && dep.JobID != 0 && dep.Endpoint != "" && !isTerminalHookPhase(dep.Phase) {
		pass := newHookPass(client, u, &child, &st)
		result, hookErr := pass.pollDeprovisionJob(ctx, awxClient, endpoint)
		if hookErr != nil {
			return CleanupResult{}, hookErr
		}
		if !result.Done {
			return result, nil
		}
		// It has just finished, so this pass goes on to find the host,
		// take the connection override off it and delete it - once per
		// hook, rather than once per poll.
	}

	// What this hook is aimed at, decided once and from the record when
	// there is one, so every branch below agrees about it.
	mode := hookTargeting(hook, st.Deprovision)

	// A lookup failure a Template-targeted hook was allowed to run
	// through is still owed to the host cleanup after it.
	host, inventory, hostName, hostErr := discoverCleanupHost(ctx, awxClient, awxConn, &child, st)
	if err := hostErr; err != nil {
		if isPermanent(err) {
			// The template is gone or ambiguous, so there is no way left
			// to work out which inventory to look in. Blocking the delete
			// forever would not find it.
			abandon("the template names no resolvable inventory", err)
			return finishHookless(ctx, client, u, &child, st, hook, "the inventory could not be resolved"), nil
		}
		// Under Delete this is retried however long it takes: an
		// abandoned host keeps an ansible_host address AWX may later hand
		// to an unrelated VM, and that is worth holding the object for.
		// Under Retain there is no host to delete, so the only thing a
		// retry could still achieve is taking a connection override back
		// off - which is not worth holding a namespace open for either.
		//
		// A hook still owed its run is the exception, and what it needs
		// depends on what it is aimed at. ManagedHost cannot launch
		// without this host, so the lookup is retried until the hook's
		// own deadline - releasing here is what used to turn one 503
		// from AWX into a deregistration that never ran and left no
		// record of it. Template is not aimed at the host at all: that
		// hook runs now, and the lookup is owed only to the cleanup
		// after it.
		owed := hook != nil && st.Deprovision != nil && !isTerminalHookPhase(st.Deprovision.Phase)
		switch {
		case owed && mode == TargetingTemplate:
			log.Printf("[AnsibleBindingVM/%s/%s] could not look up host %q, which a Template-targeted hook does not need: %v",
				child.Namespace, child.Name, st.AWXHostName, err)
			host, inventory, hostName = nil, 0, st.AWXHostName
		case owed:
			gone, vmErr := vmIsGone(ctx, client, &child)
			if vmErr != nil {
				return CleanupResult{}, vmErr
			}
			if gone {
				return CleanupResult{}, fmt.Errorf("looking up the AWX host to run the deprovision hook for VM %q against: %w", child.Spec.VMName, err)
			}
			// The VM is still there, so this is a detach and there was
			// never a hook to run. What the lookup said does not matter
			// to the hook - only to the host still to be deleted.
			st.Deprovision = skipHook(st.Deprovision, "The VirtualMachine is still present, so this is a detach rather than a deletion.")
			if !retain {
				persistHookState(ctx, client, u, &child, st)
				return CleanupResult{}, err
			}
			return releaseWithoutHost(ctx, client, u, &child, st, err), nil
		case !retain:
			// The host has to be deleted, so this is retried - with
			// whatever the hook has already settled written down first,
			// or a later pass finds no record of it and starts again.
			persistHookState(ctx, client, u, &child, st)
			return CleanupResult{}, err
		default:
			return releaseWithoutHost(ctx, client, u, &child, st, err), nil
		}
	}

	// Who the host belongs to decides everything below, so it is
	// established once. A host marked by another binding is not ours to
	// delete - and, since the hook mutates its variables and launches a
	// playbook at it, not ours to run against either. An unmarked host
	// was made by hand and adopted: it is a legitimate target for this
	// VM's playbooks, but it is never deleted and never left altered.
	marker := ""
	if host != nil {
		marker = strings.TrimSpace(host.Description)
	}
	ours := host != nil && marker == hostOwnerMarker(child.Namespace, child.Spec.BindingName)
	foreign := host != nil && !ours && strings.HasPrefix(marker, hostMarkerPrefix)
	// A host that will still be there afterwards must be handed back
	// exactly as it was found. Decided here, from the policy in force
	// now, rather than when the hook launched: cleanupPolicy can change
	// mid-teardown, and the parent copies that change down into a
	// terminating child on purpose.
	hostSurvives := host != nil && (retain || !ours)

	// The hook fires for a VM that is actually gone. A VM that merely
	// stopped matching the selector - relabelled, or the selector
	// narrowed - is still running, and a decommission playbook is not
	// something to run against a live production machine by accident.
	// That case is a separate hook, deliberately not built yet.
	if hook != nil {
		gone, vmErr := vmIsGone(ctx, client, &child)
		if vmErr != nil {
			return CleanupResult{}, vmErr
		}
		// The two host conditions below are ManagedHost's, because they
		// are about the host it aims the playbook at. A Template-mode
		// hook is aimed by its own template: it has nothing to say about
		// this host, does not touch it, and runs whether or not it is
		// there - which is the point, since the records it deregisters
		// live somewhere else.
		switch {
		case !gone:
			st.Deprovision = skipHook(st.Deprovision, "The VirtualMachine is still present, so this is a detach rather than a deletion.")
			log.Printf("[AnsibleBindingVM/%s/%s] deprovision hook skipped: VirtualMachine %q is still present",
				child.Namespace, child.Name, child.Spec.VMName)
		case mode != TargetingManagedHost && mode != TargetingTemplate:
			failed := skipHook(st.Deprovision, fmt.Sprintf(
				"spec.onDeleted.targeting is %q, which is neither %s nor %s, so nothing was launched.",
				mode, TargetingManagedHost, TargetingTemplate))
			failed.Phase = PhaseFailed
			st.Deprovision = failed
			log.Printf("[AnsibleBindingVM/%s/%s] deprovision hook refused: unknown targeting %q",
				child.Namespace, child.Name, mode)
		case mode == TargetingManagedHost && host == nil:
			st.Deprovision = skipHook(st.Deprovision, "There is no AWX inventory host to run against. "+
				"Set spec.onDeleted.targeting: Template for a hook that does not need one.")
			log.Printf("[AnsibleBindingVM/%s/%s] deprovision hook skipped: no AWX inventory host named %q",
				child.Namespace, child.Name, hostName)
		case mode == TargetingManagedHost && foreign:
			st.Deprovision = skipHook(st.Deprovision, fmt.Sprintf(
				"Inventory host %q belongs to another ansible-supervisor binding (%s), "+
					"so it was neither modified nor run against.", hostName, marker))
			log.Printf("[AnsibleBindingVM/%s/%s] deprovision hook skipped: host %q is owned by %s",
				child.Namespace, child.Name, hostName, marker)
		default:
			result, hookErr := runDeprovisionHook(ctx, client, awxClient, u, &child, &st, hook, mode, host, inventory, endpoint)
			if hookErr != nil {
				return CleanupResult{}, hookErr
			}
			if !result.Done {
				return result, nil
			}
		}
	}

	// The hook has had its pass. The host it did not need still has to
	// be found - to take an override off, or to delete it - so the
	// lookup that failed before is retried, with the outcome already
	// recorded so that a later pass resumes rather than relaunching.
	if hostErr != nil {
		persistHookState(ctx, client, u, &child, st)
		if !retain {
			return CleanupResult{}, hostErr
		}
		return releaseWithoutHost(ctx, client, u, &child, st, hostErr), nil
	}

	// The hook pins ansible_connection so a playbook that forgets
	// delegate_to cannot reach a re-leased address. On a host that
	// outlives the hook that pin would send the NEXT provisioning run to
	// the AWX control node instead of the machine, so it comes back off
	// before the finalizer is released.
	//
	// Whether the host survives is read here rather than at launch time.
	// A hook that started under Delete and finished under Retain keeps
	// its host, and the override has to come off that host too.
	if dep := st.Deprovision; dep != nil && dep.HostPinned && hostSurvives && dep.PinnedHostID != 0 && int64(host.ID) != dep.PinnedHostID {
		// Same name, different host: the one the hook pinned was deleted
		// out of band and something took its name. The override went
		// with it, and writing the remembered prior value onto this one
		// would set a variable its owner never had.
		log.Printf("[AnsibleBindingVM/%s/%s] host %q is id %d, not the id %d the hook pinned, so ansible_connection was left alone",
			child.Namespace, child.Name, hostName, host.ID, dep.PinnedHostID)
		dep.Message = strings.TrimSpace(dep.Message + fmt.Sprintf(
			" Host %q is no longer the host the hook pinned (id %d, now id %d), so the ansible_connection override was not restored onto it.",
			hostName, dep.PinnedHostID, host.ID))
		dep.HostPinned, dep.PriorConnection = false, nil
	}
	if dep := st.Deprovision; dep != nil && dep.HostPinned && hostSurvives {
		if err := awxClient.RestoreHostVariable(ctx, host, "ansible_connection", dep.PriorConnection); err != nil {
			// Reported rather than retried. Holding the finalizer on a
			// host this controller does not even own would trade a wrong
			// variable for a namespace that will not terminate, which is
			// the worse of the two.
			dep.Message = strings.TrimSpace(dep.Message + fmt.Sprintf(
				" The hook's ansible_connection: local override could not be taken back off host %q (%s), so remove it in AWX before that host is used again.", hostName, err))
			log.Printf("[AnsibleBindingVM/%s/%s] could not restore ansible_connection on host %q: %v",
				child.Namespace, child.Name, hostName, err)
		} else {
			dep.HostPinned, dep.PriorConnection = false, nil
		}
	}

	if !retain && ours {
		if err := awxClient.DeleteHost(ctx, host.ID); err != nil {
			// Recorded before the retry. The hook is finished either
			// way, and a later pass that found no record of it would
			// relaunch the playbook or rewrite the outcome as Skipped.
			persistHookState(ctx, client, u, &child, st)
			return CleanupResult{}, fmt.Errorf("deleting AWX host %d: %w", host.ID, err)
		}
	}
	reportHookOutcome(ctx, client, u, &child, st)
	return done, nil
}

// finishHookless records that a hook could not run because AWX itself is
// out of reach for good, and releases. The host is abandoned either way;
// what matters is that the reason survives the object.
func finishHookless(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, child *AnsibleBindingVM, st AnsibleBindingVMStatus, hook *DeprovisionHook, reason string) CleanupResult {
	if hook != nil {
		st.Deprovision = hooklessOutcome(st.Deprovision, reason)
		reportHookOutcome(ctx, client, u, child, st)
	}
	return CleanupResult{Done: true}
}

// hooklessOutcome is what a hook's record says once AWX has gone out of
// reach for good.
//
// A hook that never started is Skipped, which is what it is. One that
// did start is not rewritten as though nothing happened: a job may be
// running on the instance that just became unreachable, and an outcome
// already reached is the record of what the playbook did. Both keep the
// reason appended, since that is the only place it survives the object.
func hooklessOutcome(dep *DeprovisionStatus, reason string) *DeprovisionStatus {
	note := "The deprovision hook could not run: " + reason + "."
	if dep == nil || (dep.Phase == "" || dep.Phase == PhasePending) && dep.JobID == 0 {
		return &DeprovisionStatus{Phase: PhaseSkipped, Message: note}
	}
	copied := *dep
	if !isTerminalHookPhase(copied.Phase) {
		copied.Phase = PhaseFailed
		note = "The deprovision hook could not be followed to a conclusion: " + reason + ". Check AWX for a job against this host."
	} else {
		note = "Cleanup after the hook could not finish: " + reason + "."
	}
	copied.Message = strings.TrimSpace(copied.Message + " " + note)
	return &copied
}

// vmIsGone reports whether the VirtualMachine this child tracks has
// actually gone away, as opposed to merely stopping to match its
// binding's selector.
//
// Three things count as gone, and the second two are why a name lookup
// alone is not enough: the object is absent; it is being deleted, since
// vm-operator's own finalizer holds it while it destroys the machine;
// or the name now resolves to a different UID, which means the original
// was destroyed and something else took its name.
func vmIsGone(ctx context.Context, client *dynamic.DynamicClient, child *AnsibleBindingVM) (bool, error) {
	vm, err := client.Resource(vmGVR).Namespace(child.Namespace).Get(ctx, child.Spec.VMName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking whether VirtualMachine %q is gone: %w", child.Spec.VMName, err)
	}
	if !vm.GetDeletionTimestamp().IsZero() {
		return true, nil
	}
	if ownerUID, _ := checkOwnedByItsVM(child); ownerUID != "" && string(vm.GetUID()) != ownerUID {
		return true, nil
	}
	return false, nil
}

// releaseWithoutHost gives up on an inventory host that could not be
// looked up, under a policy that does not need it deleted. What was left
// on it is recorded, since nothing after this will get the chance to.
func releaseWithoutHost(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, child *AnsibleBindingVM, st AnsibleBindingVMStatus, err error) CleanupResult {
	log.Printf("[AnsibleBindingVM/%s/%s] cleanup: could not look up host %q, and cleanupPolicy is Retain so there is nothing to delete: %v",
		child.Namespace, child.Name, st.AWXHostName, err)
	if dep := st.Deprovision; dep != nil && dep.HostPinned {
		dep.Message = strings.TrimSpace(dep.Message + fmt.Sprintf(
			" The hook's ansible_connection: local override is still on host %q, which could not be reached to undo it (%s).", st.AWXHostName, err))
	}
	reportHookOutcome(ctx, client, u, child, st)
	return CleanupResult{Done: true}
}

// hookTargeting is the mode this hook runs under: what the record says
// if it has started, and what the spec says if it has not.
//
// The record wins because the spec can be edited while a hook is
// running - the parent copies its binding's spec down into a
// terminating child on purpose - and a hook that changed what it was
// aimed at half way through would pin a host it is no longer targeting,
// or leave one pinned that it is.
func hookTargeting(hook *DeprovisionHook, dep *DeprovisionStatus) string {
	if dep != nil && dep.Targeting != "" {
		return dep.Targeting
	}
	if dep != nil && dep.StartedAt != "" {
		// Started before the mode was recorded, which was ManagedHost
		// because nothing else existed.
		return TargetingManagedHost
	}
	if hook != nil && hook.Targeting != "" {
		// Returned as written, including a value that is neither mode.
		// The CRD's enum refuses those at admission, and a hook that
		// reached the controller with one is refused rather than run
		// under a mode nobody asked for.
		return hook.Targeting
	}
	return TargetingManagedHost
}

// skipHook records that a hook will not run, keeping the clock and mode
// the pass that opened the record wrote.
func skipHook(dep *DeprovisionStatus, message string) *DeprovisionStatus {
	copied := DeprovisionStatus{}
	if dep != nil {
		copied = *dep
	}
	copied.Phase, copied.Message = PhaseSkipped, message
	return &copied
}

// persistHookState writes the hook's record without reporting it as
// final. It is what stops a terminal hook being relaunched because the
// host cleanup that follows it failed and the pass returned early.
func persistHookState(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, child *AnsibleBindingVM, st AnsibleBindingVMStatus) {
	if ansibleBindingVMDetailsCurrent(child.Status, st) {
		return
	}
	if err := writeAnsibleBindingVMDetails(ctx, client, u, st); err != nil {
		log.Printf("[AnsibleBindingVM/%s/%s] could not record deprovision progress before retrying host cleanup: %v",
			child.Namespace, child.Name, err)
	}
}

// startHookClock stamps the hook's deadline the first time finalization
// looks at this object, and persists it before anything that can fail.
//
// Written once and read back afterwards, never recomputed: an edit to
// spec.onDeleted.timeoutSeconds mid-teardown would otherwise extend a
// hook that is already overdue, or expire one that is not.
func startHookClock(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, child *AnsibleBindingVM, st *AnsibleBindingVMStatus, hook *DeprovisionHook) error {
	if st.Deprovision != nil && st.Deprovision.StartedAt != "" {
		return nil
	}
	dep := &DeprovisionStatus{}
	if st.Deprovision != nil {
		copied := *st.Deprovision
		dep = &copied
	}
	timeout := time.Duration(hook.TimeoutSeconds) * time.Second
	if hook.TimeoutSeconds <= 0 {
		timeout = defaultHookTimeoutSeconds * time.Second
	}
	// Read before the clock is stamped: hookTargeting treats a record
	// that has started without a mode as ManagedHost, and stamping
	// startedAt first would make every new hook look like one of those.
	dep.Targeting = hookTargeting(hook, dep)
	now := time.Now().UTC()
	dep.StartedAt = now.Format(time.RFC3339)
	dep.Deadline = now.Add(timeout).Format(time.RFC3339)
	if dep.Phase == "" {
		dep.Phase = PhasePending
	}
	st.Deprovision = dep
	if ansibleBindingVMDetailsCurrent(child.Status, *st) {
		return nil
	}
	if err := writeAnsibleBindingVMDetails(ctx, client, u, *st); err != nil {
		return fmt.Errorf("recording the deprovision deadline: %w", err)
	}
	return nil
}

// recordedAWXEndpoint is the AWX instance this child's external state
// lives on, from whichever pass last wrote one down.
//
// status.awxEndpoint is only set once a provisioning pass has recorded a
// host, so it cannot be the whole answer during finalization: the hook
// pins a host and launches a job of its own, and both are just as
// meaningless on an instance that never saw them.
func recordedAWXEndpoint(st AnsibleBindingVMStatus) string {
	if st.AWXEndpoint != "" {
		return st.AWXEndpoint
	}
	if dep := st.Deprovision; dep != nil {
		if dep.PinnedHostEndpoint != "" {
			return dep.PinnedHostEndpoint
		}
		return dep.Endpoint
	}
	return ""
}

// discoverCleanupHost finds the AWX inventory host this child is
// responsible for, or nil when there is none.
//
// It resolves by name and re-reads what AWX currently holds even when
// status has an ID: an AWX host deleted out of band may have been
// recreated just before a crash, and the description on whatever is
// there now is the only trustworthy statement of ownership.
func discoverCleanupHost(ctx context.Context, awxClient *AWXClient, awxConn AWXConnection, child *AnsibleBindingVM, st AnsibleBindingVMStatus) (*hostResult, int64, string, error) {
	inventory, name := st.AWXInventoryID, st.AWXHostName
	if inventory == 0 || name == "" {
		// Also handle a child with no status. Never assume a missing ID
		// alone proves there is no external host to clean up.
		tmpl, err := resolveTemplate(ctx, awxClient, child.Spec.Template)
		if err != nil {
			return nil, 0, "", fmt.Errorf("resolving inventory for host cleanup: %w", err)
		}
		if tmpl.Inventory == nil {
			return nil, 0, "", nil
		}
		inventory = int64(*tmpl.Inventory)
		name = child.Spec.HostName
		if name == "" {
			name = child.Spec.VMName
		}
		name = awxConn.Spec.HostNamePrefix + name
	}
	host, err := awxClient.FindHost(ctx, int(inventory), name)
	if err != nil {
		return nil, inventory, name, fmt.Errorf("rediscovering AWX host for cleanup: %w", err)
	}
	return host, inventory, name, nil
}

// runDeprovisionHook advances the hook by one step and says whether it
// has finished. It is called once per pass on a terminating child, and
// every decision it makes comes from status rather than from anything
// the process remembers, so a restart resumes rather than relaunches.
//
// The hook is finished when it reaches any terminal phase - including
// Failed and TimedOut. Neither holds the finalizer: releasing on a
// failed teardown loses a deregistration, but blocking on one wedges the
// VM, its binding and any namespace being deleted above it, and a
// namespace that will not terminate is the worse failure by some way.
// hookPass is one pass over one hook: the status it is building, and
// how that status reaches the object. Both the full path and the
// poll-only fast path go through it, so the two cannot disagree about
// what a phase transition writes.
type hookPass struct {
	client *dynamic.DynamicClient
	u      *unstructured.Unstructured
	child  *AnsibleBindingVM
	st     *AnsibleBindingVMStatus
	dep    *DeprovisionStatus
}

func newHookPass(client *dynamic.DynamicClient, u *unstructured.Unstructured, child *AnsibleBindingVM, st *AnsibleBindingVMStatus) *hookPass {
	dep := &DeprovisionStatus{}
	if st.Deprovision != nil {
		copied := *st.Deprovision
		dep = &copied
	}
	return &hookPass{client: client, u: u, child: child, st: st, dep: dep}
}

// persist writes the hook's progress, and only when it says something
// new: an unchanged poll of a job that is still running must cost no
// write at all, or a large teardown writes to etcd once per VM per poll
// for the length of the slowest playbook.
func (p *hookPass) persist(ctx context.Context) error {
	p.st.Deprovision = p.dep
	if ansibleBindingVMDetailsCurrent(p.child.Status, *p.st) {
		return nil
	}
	return writeAnsibleBindingVMDetails(ctx, p.client, p.u, *p.st)
}

func (p *hookPass) waiting(ctx context.Context, phase, message string) (CleanupResult, error) {
	p.dep.Phase, p.dep.Message = phase, message
	if err := p.persist(ctx); err != nil {
		return CleanupResult{}, fmt.Errorf("recording deprovision progress: %w", err)
	}
	return CleanupResult{RequeueAfter: hookRequeue(p.dep)}, nil
}

func (p *hookPass) settle(phase, message string) (CleanupResult, error) {
	p.dep.Phase, p.dep.Message = phase, message
	p.st.Deprovision = p.dep
	return CleanupResult{Done: true}, nil
}

// pollDeprovisionJob advances a hook whose job is already running, and
// is the whole of what an ordinary poll needs.
//
// Everything the launch had to establish - that the VM is gone, which
// host to target, that the template accepts a limit and shares the
// inventory - was established when the launch was authorized and does
// not change while a job runs. Re-deriving it on every poll costs an
// AWX host lookup and a live VirtualMachine read per VM per poll, which
// at teardown scale is most of the traffic this feature adds.
func (p *hookPass) pollDeprovisionJob(ctx context.Context, awxClient *AWXClient, endpoint string) (CleanupResult, error) {
	dep := p.dep
	// A job id means nothing on an instance that did not issue it, and
	// an AWXConnection can be repointed while a hook is running. This is
	// checked against what the launch recorded rather than against
	// status.awxEndpoint, which is only set once a provisioning pass has
	// written it - a child whose host was rediscovered rather than
	// remembered has none.
	if dep.Endpoint != "" && endpoint != "" && dep.Endpoint != endpoint {
		return p.settle(PhaseFailed, fmt.Sprintf(
			"The AWXConnection was repointed at a different AWX instance while the hook was running, so job %d could not be followed to a conclusion. Check %s for how it ended.",
			dep.JobID, dep.Endpoint))
	}
	status, err := pollJobStatus(ctx, awxClient, dep.JobType, dep.JobID)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("polling deprovision job %d: %w", dep.JobID, err)
	}
	dep.JobStatus = status
	if !isTerminalAWXStatus(status) {
		return p.waiting(ctx, PhaseRunning, "")
	}
	// A launch AWX narrowed is not a success however well the job then
	// ran: the playbook was scoped to something other than this host,
	// which is the one thing the limit exists to prevent.
	if dep.LaunchError != "" {
		return p.settle(PhaseFailed, "AWX did not accept the launch as requested: "+dep.LaunchError)
	}
	if mapAWXStatus(status) == PhaseSucceeded {
		return p.settle(PhaseSucceeded, "")
	}
	return p.settle(PhaseFailed, "The deprovision job did not succeed. The inventory host was removed anyway.")
}

// hookRequeue is how long to wait before looking at a running hook
// again, jittered so that a namespace deleted all at once does not put
// every one of its hooks on the same fifteen-second beat forever after.
// It never reaches past the hook's own deadline: a poll scheduled after
// the deadline would find an expired hook rather than a finished one.
func hookRequeue(dep *DeprovisionStatus) time.Duration {
	interval := hookPollInterval + time.Duration(rand.Int63n(int64(hookPollJitter)))
	if dep == nil || dep.Deadline == "" {
		return interval
	}
	at, err := time.Parse(time.RFC3339, dep.Deadline)
	if err != nil {
		return interval
	}
	if remaining := time.Until(at); remaining < interval {
		if remaining < time.Second {
			return time.Second
		}
		return remaining
	}
	return interval
}

func runDeprovisionHook(ctx context.Context, client *dynamic.DynamicClient, awxClient *AWXClient, u *unstructured.Unstructured, child *AnsibleBindingVM, st *AnsibleBindingVMStatus, hook *DeprovisionHook, mode string, host *hostResult, inventory int64, endpoint string) (CleanupResult, error) {
	pass := newHookPass(client, u, child, st)
	dep := pass.dep
	persist := func() error { return pass.persist(ctx) }
	waiting := func(phase, message string) (CleanupResult, error) { return pass.waiting(ctx, phase, message) }
	settle := pass.settle

	// startHookClock has normally stamped this already, before the AWX
	// lookups that got us here - the deadline has to be durable before
	// the first thing that can fail, or a retry restarts the clock and
	// timeoutSeconds bounds nothing. This is the fallback for a caller
	// that reached the hook without one.
	if dep.StartedAt == "" {
		timeout := time.Duration(hook.TimeoutSeconds) * time.Second
		if hook.TimeoutSeconds <= 0 {
			timeout = defaultHookTimeoutSeconds * time.Second
		}
		now := time.Now().UTC()
		dep.StartedAt = now.Format(time.RFC3339)
		dep.Deadline = now.Add(timeout).Format(time.RFC3339)
		dep.Phase = PhasePending
		if err := persist(); err != nil {
			return CleanupResult{}, fmt.Errorf("recording the deprovision deadline: %w", err)
		}
	}

	if isTerminalHookPhase(dep.Phase) {
		st.Deprovision = dep
		return CleanupResult{Done: true}, nil
	}
	switch dep.Phase {
	case PhaseLaunching:
		// The launch request went out and its answer never reached
		// status. The job may well be running; relaunching a
		// decommission playbook to find out is worse than not knowing,
		// so this stops here and says so.
		return settle(PhaseFailed, "A launch was issued but its outcome was never recorded, so it was not retried. Check AWX for a job against this host.")
	}

	if overdue(dep.Deadline) {
		return settle(PhaseTimedOut, "The hook did not finish before spec.onDeleted.timeoutSeconds elapsed. The inventory host was removed anyway.")
	}

	// A provisioning job still running against this host would be
	// configuring a machine the deprovision playbook is about to
	// deregister. Two playbooks, same target, opposite intent - so wait
	// for the first, bounded by the same deadline as everything else.
	if dep.JobID == 0 && st.LastJobID != 0 && !isTerminalAWXStatus(st.LastJobStatus) {
		status, err := pollRecordedJob(ctx, client, child, *st)
		if err != nil {
			return CleanupResult{}, fmt.Errorf("polling in-flight job %d before the deprovision hook: %w", st.LastJobID, err)
		}
		st.LastJobStatus = status
		st.Phase = mapAWXStatus(status)
		if !isTerminalAWXStatus(status) {
			return waiting(PhasePending, fmt.Sprintf("Waiting for provisioning job %d to finish before launching the hook.", st.LastJobID))
		}
	}

	if dep.JobID != 0 {
		return pass.pollDeprovisionJob(ctx, awxClient, endpoint)
	}

	return launchDeprovisionHook(ctx, awxClient, child, st, dep, hook, mode, host, inventory, endpoint, persist, settle)
}

// launchDeprovisionHook resolves the hook template and launches it,
// under ManagedHost also narrowing the run to this VM's inventory host
// and pinning that host to the control node.
func launchDeprovisionHook(ctx context.Context, awxClient *AWXClient, child *AnsibleBindingVM, st *AnsibleBindingVMStatus, dep *DeprovisionStatus, hook *DeprovisionHook, mode string, host *hostResult, inventory int64, endpoint string,
	persist func() error, settle func(string, string) (CleanupResult, error)) (CleanupResult, error) {

	// Resolved from AWX at launch time, never from the template cache:
	// ask_limit_on_launch is what stops this running against every host
	// in the inventory, and it can be switched off in the AWX UI between
	// one pass and the next.
	tmpl, err := resolveTemplate(ctx, awxClient, hook.Template)
	if err != nil {
		if isPermanent(err) {
			return settle(PhaseFailed, fmt.Sprintf("The hook template could not be resolved: %s", err))
		}
		return CleanupResult{}, err
	}

	dep.Targeting = mode

	// Under Template the controller narrows nothing: no limit, no
	// inventory, and no host to pin. Everything below this block exists
	// to make a limit safe, and a launch that supplies none of it needs
	// none of it. What the job runs against is then whatever the
	// template says, which is what the mode was asked for.
	if mode == TargetingTemplate {
		return submitDeprovisionHook(ctx, awxClient, child, st, dep, hook, tmpl, "", "the template's own targeting", endpoint, persist, settle)
	}

	// Unlike a provisioning run, a hook has no useDefaultLimit escape.
	// A deprovision playbook that ran against a whole inventory would
	// decommission every host in it, so a template that will not accept
	// a limit is refused rather than launched.
	if !tmpl.AskLimitOnLaunch {
		return settle(PhaseFailed, fmt.Sprintf("Template %q does not accept a limit at launch time (ask_limit_on_launch is false), "+
			"so AWX would have run it against the whole inventory rather than this host. Enable Prompt on Launch for Limit in AWX, "+
			"or set spec.onDeleted.targeting: Template if the hook is meant to run against what the template itself targets.", hook.Template.Name))
	}
	// A limit selects hosts within the job's OWN inventory; it does not
	// reach across to the inventory this VM's host lives in. A hook
	// template pointed somewhere else would run against an unrelated host
	// that happens to share the name, or against nothing at all while the
	// teardown reported success - so the two inventories have to be the
	// same one, and a template with no inventory of its own (common on
	// workflow templates, where each node carries its own) cannot target
	// a host at all.
	//
	// Neither is quietly downgraded to Template targeting. A template
	// that stopped accepting a limit is a hook that no longer does what
	// the manifest says, and widening the run to whatever it does target
	// is not a repair.
	if tmpl.Inventory == nil {
		return settle(PhaseFailed, fmt.Sprintf("Template %q has no inventory of its own, so a per-host limit cannot select %q. "+
			"Point spec.onDeleted.template at a template configured with the same inventory as spec.template, "+
			"or set spec.onDeleted.targeting: Template to launch it as configured.", hook.Template.Name, host.Name))
	}
	if int64(*tmpl.Inventory) != inventory {
		return settle(PhaseFailed, fmt.Sprintf("Template %q runs against inventory %d, but host %q is in inventory %d. "+
			"A limit only selects hosts within the job's own inventory, so the hook would have run against the wrong host or none at all. "+
			"Point spec.onDeleted.template at a template configured with the same inventory as spec.template, "+
			"or set spec.onDeleted.targeting: Template to launch it as configured.",
			hook.Template.Name, *tmpl.Inventory, host.Name, inventory))
	}

	// The guest is already destroyed by the time this runs, and its
	// address may have been re-leased to an unrelated machine. Pinning
	// the host to the control node means a playbook that forgets
	// delegate_to cannot SSH to whatever now answers there.
	//
	// What the variable said before is recorded first, so the override
	// can be taken back off a host that outlives the hook rather than
	// silently redirecting the next provisioning run to the control node.
	//
	// Recorded on every pin, not only when the host looks like it will
	// survive: cleanupPolicy can change while a hook runs - the parent
	// copies it down into a terminating child precisely so it can - and a
	// hook that started under Delete and finished under Retain would
	// otherwise keep the host with no way to undo the override.
	//
	// Recorded once, not per attempt. After a restart between the PATCH
	// below and the next status write, the host already says "local", so
	// re-reading it would save "local" as the original value and restore
	// the pin instead of removing it.
	if !dep.HostPinned {
		if prior, had := hostVariable(host, "ansible_connection"); had {
			value := prior
			dep.PriorConnection = &value
		} else {
			dep.PriorConnection = nil
		}
		dep.HostPinned = true
		// Which host, and on which instance. The restore runs passes
		// later off a lookup that resolves by name, and the name alone
		// cannot tell the pinned host from a replacement that took it.
		dep.PinnedHostID, dep.PinnedHostEndpoint = int64(host.ID), endpoint
		// Written before the host is touched, not after: a controller
		// that died in between would otherwise leave the override on a
		// host nothing knows to take it off again.
		if err := persist(); err != nil {
			return CleanupResult{}, fmt.Errorf("recording the connection override on host %q: %w", host.Name, err)
		}
	}
	if err := awxClient.SetHostVariables(ctx, host, map[string]string{"ansible_connection": "local"}); err != nil {
		return CleanupResult{}, fmt.Errorf("pinning host %q to the control node before the deprovision hook: %w", host.Name, err)
	}

	return submitDeprovisionHook(ctx, awxClient, child, st, dep, hook, tmpl, host.Name, fmt.Sprintf("host %q", host.Name), endpoint, persist, settle)
}

// submitDeprovisionHook is the half of a launch both modes share: the
// deletion context, the durable record that a launch was issued, and
// what came back. Only what the run is narrowed to differs, and that
// arrives already decided as limit - empty under Template, where the
// controller supplies no targeting at all.
//
// Nothing here reads the inventory host. A hook launched under Template
// may not have one, and the context below is reconstructed from the
// child's own identity and status precisely so that it does not need it.
func submitDeprovisionHook(ctx context.Context, awxClient *AWXClient, child *AnsibleBindingVM, st *AnsibleBindingVMStatus, dep *DeprovisionStatus, hook *DeprovisionHook, tmpl *AWXTemplate, limit, target, endpoint string,
	persist func() error, settle func(string, string) (CleanupResult, error)) (CleanupResult, error) {

	// AWX silently drops launch fields the template is not configured to
	// accept, so variables go only to a template that asks for them. A
	// limit is refused rather than dropped because it is a safety
	// property; these are context, and a deregistration playbook that
	// works from inventory_hostname needs none of them.
	extraVars := map[string]string{
		"asb_hook":          "onDeleted",
		"asb_vm_name":       child.Spec.VMName,
		"asb_binding":       child.Spec.BindingName,
		"asb_last_known_ip": st.ObservedIP,
	}
	if uid, _ := checkOwnedByItsVM(child); uid != "" {
		extraVars["asb_vm_uid"] = uid
	}
	varsNote := ""
	if !tmpl.AskVariablesOnLaunch {
		extraVars = nil
		varsNote = fmt.Sprintf("Template %q does not accept variables at launch time, so the asb_* variables were not passed.", hook.Template.Name)
	}

	// Written before the launch goes out: a controller that dies between
	// the request and the response leaves a record that something was
	// started, which is what stops the next pass launching a second one.
	dep.Phase, dep.Message, dep.JobType, dep.Endpoint = PhaseLaunching, varsNote, hook.Template.Type, endpoint

	if err := persist(); err != nil {
		return CleanupResult{}, fmt.Errorf("recording the deprovision launch: %w", err)
	}

	var jobID int
	var err error
	if hook.Template.Type == TemplateTypeWorkflow {
		jobID, err = awxClient.LaunchWorkflowJobTemplate(ctx, tmpl.ID, limit, extraVars)
	} else {
		jobID, err = awxClient.LaunchJobTemplate(ctx, tmpl.ID, limit, extraVars)
	}
	if jobID != 0 {
		dep.JobID = int64(jobID)
		dep.JobURL = awxClient.JobURL(jobID, hook.Template.Type == TemplateTypeWorkflow)
		dep.JobStatus = "pending"
		dep.Phase = PhaseRunning
		// AWX answers a launch it narrowed with a job id AND an error
		// naming what it dropped. The job is real and has to be tracked
		// to a terminal state either way, but a dropped limit means it is
		// not running against this host, so the error is carried through
		// to the outcome rather than discarded here.
		if err != nil {
			dep.LaunchError = err.Error()
			log.Printf("[AnsibleBindingVM/%s/%s] deprovision job %d launched but AWX did not accept it as requested: %v",
				child.Namespace, child.Name, jobID, err)
		}
		if err := persist(); err != nil {
			return CleanupResult{}, fmt.Errorf("recording deprovision job %d: %w", jobID, err)
		}
		log.Printf("[AnsibleBindingVM/%s/%s] deprovision hook launched: job %d (%s) against %s",
			child.Namespace, child.Name, jobID, dep.JobURL, target)
		return CleanupResult{RequeueAfter: hookRequeue(dep)}, nil
	}
	return settle(PhaseFailed, fmt.Sprintf("The hook could not be launched: %s", err))
}

// isTerminalHookPhase reports whether a hook has finished, one way or
// another. Failed, TimedOut and Skipped are as final as Succeeded: none
// of them is retried, and none of them holds the finalizer.
func isTerminalHookPhase(phase string) bool {
	switch phase {
	case PhaseSucceeded, PhaseFailed, PhaseTimedOut, PhaseSkipped:
		return true
	}
	return false
}

// overdue reports whether an RFC3339 deadline has passed. An unparseable
// or empty deadline is not overdue: a hook is never abandoned because
// its own bookkeeping was unreadable.
func overdue(deadline string) bool {
	if deadline == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		return false
	}
	return time.Now().After(at)
}

// reportHookOutcome writes what the hook did to the three places it can
// still be read from after this object is deleted: the child's own
// status, for as long as it exists; the controller log, which outlives
// everything and carries the AWX job URL; and an Event on the parent
// binding, which is where an operator looks when a VM they deleted did
// not deregister.
func reportHookOutcome(ctx context.Context, client *dynamic.DynamicClient, u *unstructured.Unstructured, child *AnsibleBindingVM, st AnsibleBindingVMStatus) {
	if st.Deprovision == nil {
		return
	}
	message := hookOutcomeMessage(child.Spec.VMName, st.Deprovision)

	if !ansibleBindingVMDetailsCurrent(child.Status, st) {
		if err := writeAnsibleBindingVMDetails(ctx, client, u, st); err != nil {
			log.Printf("[AnsibleBindingVM/%s/%s] could not record the deprovision outcome in status: %v", child.Namespace, child.Name, err)
		}
	}

	eventType, reason := eventNormal, "DeprovisionHookSucceeded"
	switch st.Deprovision.Phase {
	case PhaseSucceeded:
	case PhaseSkipped:
		reason = "DeprovisionHookSkipped"
	case PhaseTimedOut:
		eventType, reason = eventWarning, "DeprovisionHookTimedOut"
	default:
		eventType, reason = eventWarning, "DeprovisionHookFailed"
	}

	log.Printf("[AnsibleBindingVM/%s/%s] %s", child.Namespace, child.Name, message)
	recordEvent(ctx, client, eventTarget(ctx, client, u, child.Spec.BindingName), eventType, reason, message)
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
