package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// detailsFieldManager owns status.vms/observedGeneration/lastAppliedTrigger.
// It's deliberately different from StatusFieldManager so the generic
// state/message/ready/lastUpdated patch (applied by the engine after
// provisionFunc returns) never clobbers these fields, and vice versa -
// the same dual-field-manager pattern used elsewhere in this service's
// lineage for extra per-resource status data.
const detailsFieldManager = "ansible-supervisor-details"

// applyAnsibleBinding resolves the AWXConnection and template,
// finds every VM currently matched by vmSelector, and for each one:
// keeps its AWX inventory host in sync, launches (or polls) a run, and
// records the result. VMs that dropped out of the selector since the
// last reconcile have their AWX host cleaned up per spec.cleanupPolicy.
//
// A failure against one VM doesn't abort the others - every VM's status
// is still recorded, and the first error encountered is returned so the
// generic engine marks the AnsibleBinding as a whole Failed with
// that message (status.vms still shows exactly which VM(s) succeeded).
func applyAnsibleBinding(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	ac, err := convertAnsibleBinding(u)
	if err != nil {
		return fmt.Errorf("decoding AnsibleBinding: %w", err)
	}
	if ac.Spec == nil {
		return fmt.Errorf("spec is required")
	}
	if ac.Spec.AWXConnectionRef == "" {
		return fmt.Errorf("spec.awxConnectionRef is required")
	}
	// An empty selector would match every VM in the namespace and run
	// the playbook against all of them. Refuse rather than guess.
	if len(ac.Spec.VMSelector) == 0 {
		return fmt.Errorf("spec.vmSelector must not be empty: an empty selector would target every VM in the namespace")
	}

	awxConnObj, err := client.Resource(awxConnGVR).Namespace(ac.Namespace).Get(ctx, ac.Spec.AWXConnectionRef, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("fetching AWXConnection %q: %w", ac.Spec.AWXConnectionRef, err)
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil {
		return fmt.Errorf("decoding AWXConnection %q: %w", ac.Spec.AWXConnectionRef, err)
	}
	if awxConn.Spec == nil {
		return fmt.Errorf("AWXConnection %q has no spec", ac.Spec.AWXConnectionRef)
	}

	token, err := getSecretValue(ctx, client, ac.Namespace, awxConn.Spec.SecretRef, "token")
	if err != nil {
		return fmt.Errorf("reading AWX token from secret %q: %w", awxConn.Spec.SecretRef, err)
	}
	awxClient, _, err := awxClientFor(ctx, client, awxConn, token)
	if err != nil {
		return fmt.Errorf("preparing a client for AWXConnection %q: %w", ac.Spec.AWXConnectionRef, err)
	}

	var tmpl *AWXTemplate
	switch ac.Spec.Template.Type {
	case TemplateTypeJob:
		tmpl, err = awxClient.FindJobTemplate(ctx, ac.Spec.Template.Name)
	case TemplateTypeWorkflow:
		tmpl, err = awxClient.FindWorkflowJobTemplate(ctx, ac.Spec.Template.Name)
	default:
		err = fmt.Errorf("spec.template.type must be %q or %q, got %q", TemplateTypeJob, TemplateTypeWorkflow, ac.Spec.Template.Type)
	}
	if err != nil {
		return fmt.Errorf("resolving template %q: %w", ac.Spec.Template.Name, err)
	}

	vms, err := listVirtualMachines(ctx, client, ac.Namespace, ac.Spec.VMSelector)
	if err != nil {
		return fmt.Errorf("listing target VMs: %w", err)
	}

	// A hostName override renames the inventory host, so it can only
	// apply to a single VM - across several matches they'd collide on
	// one AWX host.
	if ac.Spec.HostName != "" && len(vms) > 1 {
		return fmt.Errorf("spec.hostName is set but vmSelector matches %d VMs: an inventory host name can only stand for one VM", len(vms))
	}

	// targetsHost is whether we intend to scope each run to the VM's own
	// inventory host via --limit.
	targetsHost := !ac.Spec.UseDefaultLimit && tmpl.Inventory != nil

	if err := checkTemplateAcceptsLaunchFields(tmpl, ac.Spec.Template.Name, targetsHost, len(ac.Spec.ExtraVars) > 0,
		"set spec.useDefaultLimit: true to accept the template's own scope"); err != nil {
		return err
	}

	priorByName := map[string]VMStatus{}
	if ac.Status != nil {
		for _, v := range ac.Status.VMs {
			priorByName[v.Name] = v
		}
	}

	triggerValue := ac.Annotations[ReconcileRequestedAtAnnotation]

	cleanupPolicy := ac.Spec.CleanupPolicy
	if cleanupPolicy == "" {
		cleanupPolicy = CleanupPolicyDelete
	}

	matched := map[string]bool{}
	var newVMStatuses []VMStatus
	var firstErr error
	recordErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for i := range vms {
		vm := vms[i]
		name := vm.GetName()
		matched[name] = true
		prior := priorByName[name]

		vs := prior
		vs.Name = name
		vs.PendingCleanup = false
		vs.LastUpdated = nowRFC3339()
		vs.History = append([]VMRunHistoryEntry(nil), prior.History...)

		// Poll any in-flight job first: its outcome doesn't depend on
		// the VM's current power state, so this must happen even if the
		// VM has since powered off.
		if prior.LastJobID != 0 && !isTerminalAWXStatus(prior.LastJobStatus) {
			status, sErr := pollJobStatus(ctx, awxClient, ac.Spec.Template.Type, prior.LastJobID)
			if sErr != nil {
				vs.Phase = PhaseRunning
				recordErr(fmt.Errorf("polling job %d for VM %q: %w", prior.LastJobID, name, sErr))
				newVMStatuses = append(newVMStatuses, vs)
				continue
			}
			vs.LastJobStatus = status
			vs.Phase = mapAWXStatus(status)
			if !isTerminalAWXStatus(status) {
				// Still running: don't relaunch, and leave this VM's
				// appliedGeneration/appliedTrigger alone so a re-run
				// requested mid-flight is honored once it finishes.
				vs.History = upsertHistory(vs.History, VMRunHistoryEntry{JobID: prior.LastJobID, Status: status})
				newVMStatuses = append(newVMStatuses, vs)
				continue
			}
			vs.History = upsertHistory(vs.History, VMRunHistoryEntry{
				JobID: prior.LastJobID, Status: status, FinishedAt: nowRFC3339(),
			})
		}

		ip, ready := vmReady(&vm)
		vs.ObservedIP = ip
		if !ready {
			// Pending means "never ran, waiting on the VM" - a VM that
			// already has a run keeps that run's phase.
			if vs.LastJobID == 0 {
				vs.Phase = PhasePending
			}
			newVMStatuses = append(newVMStatuses, vs)
			continue
		}

		hostName := name
		if ac.Spec.HostName != "" {
			hostName = ac.Spec.HostName
		}
		hostName = awxConn.Spec.HostNamePrefix + hostName

		if tmpl.Inventory != nil {
			inventoryID := int64(*tmpl.Inventory)

			// A renamed host (changed hostNamePrefix or spec.hostName), or
			// a template repointed at a different inventory, would
			// otherwise orphan the old entry - under the old name, or in
			// the old inventory - while a second one appears alongside it.
			renamed := vs.AWXHostName != "" && vs.AWXHostName != hostName
			moved := vs.AWXInventoryID != 0 && vs.AWXInventoryID != inventoryID
			if vs.AWXHostID != 0 && (renamed || moved) {
				if vs.AWXHostCreated && cleanupPolicy == CleanupPolicyDelete {
					if dErr := awxClient.DeleteHost(ctx, int(vs.AWXHostID)); dErr != nil {
						// Leave the recorded host in place and retry,
						// rather than losing track of it.
						recordErr(fmt.Errorf("retiring AWX host %q for VM %q: %w", vs.AWXHostName, name, dErr))
						newVMStatuses = append(newVMStatuses, vs)
						continue
					}
				}
				vs.AWXHostID = 0
				vs.AWXHostCreated = false
				vs.AWXInventoryID = 0
			}

			hostID, owned, hErr := upsertInventoryHost(ctx, awxClient, *tmpl.Inventory, hostName,
				hostOwnerMarker(ac.Namespace, ac.Name), ip, ac.Spec.HostVariables)
			if hErr != nil {
				vs.Phase = PhaseFailed
				recordErr(fmt.Errorf("upserting AWX host for VM %q: %w", name, hErr))
				newVMStatuses = append(newVMStatuses, vs)
				continue
			}
			vs.AWXHostID = int64(hostID)
			vs.AWXHostName = hostName
			vs.AWXHostCreated = owned
			vs.AWXInventoryID = inventoryID
		}

		// The relaunch decision is per VM, so a spec change or re-run
		// request is never consumed on behalf of a VM that didn't act on
		// it (mid-flight job, powered-off VM).
		needsRun := vs.LastJobID == 0 ||
			vs.AppliedGeneration != ac.Generation ||
			vs.AppliedTrigger != triggerValue
		if !needsRun {
			newVMStatuses = append(newVMStatuses, vs)
			continue
		}

		var limit string
		if targetsHost {
			limit = hostName
		}
		var jobID int
		var lErr error
		if ac.Spec.Template.Type == TemplateTypeWorkflow {
			jobID, lErr = awxClient.LaunchWorkflowJobTemplate(ctx, tmpl.ID, limit, ac.Spec.ExtraVars)
		} else {
			jobID, lErr = awxClient.LaunchJobTemplate(ctx, tmpl.ID, limit, ac.Spec.ExtraVars)
		}
		if jobID != 0 {
			// Record the run even when lErr is set (AWX ignored fields):
			// the job is real and running, and must stay traceable.
			vs.LastJobID = int64(jobID)
			vs.LastJobURL = awxClient.JobURL(jobID, ac.Spec.Template.Type == TemplateTypeWorkflow)
			vs.LastJobStatus = "pending"
			vs.Phase = PhaseRunning
			vs.History = upsertHistory(vs.History, VMRunHistoryEntry{JobID: int64(jobID), Status: "pending", StartedAt: nowRFC3339()})
			vs.AppliedGeneration = ac.Generation
			vs.AppliedTrigger = triggerValue
		}
		if lErr != nil {
			if jobID == 0 {
				vs.Phase = PhaseFailed
			}
			recordErr(fmt.Errorf("launching template for VM %q: %w", name, lErr))
		}
		newVMStatuses = append(newVMStatuses, vs)
	}

	// VMs that were previously tracked but no longer match the selector
	// (deleted, relabeled) get their AWX host cleaned up now rather than
	// left stale - see spec.cleanupPolicy for the opt-out. Only hosts
	// this controller created are ever deleted.
	for name, prior := range priorByName {
		if matched[name] {
			continue
		}
		if cleanupPolicy != CleanupPolicyDelete || prior.AWXHostID == 0 || !prior.AWXHostCreated {
			continue
		}
		if dErr := awxClient.DeleteHost(ctx, int(prior.AWXHostID)); dErr != nil {
			log.Printf("[AnsibleBinding/%s/%s] failed to delete AWX host %d for unmatched VM %q: %v", ac.Namespace, ac.Name, prior.AWXHostID, name, dErr)
			recordErr(fmt.Errorf("deleting AWX host for unmatched VM %q: %w", name, dErr))
			// Keep tracking it, otherwise the host is leaked: dropping
			// it from status would lose the ID and the retry with it.
			stale := prior
			stale.PendingCleanup = true
			stale.LastUpdated = nowRFC3339()
			newVMStatuses = append(newVMStatuses, stale)
		}
	}

	sort.Slice(newVMStatuses, func(i, j int) bool { return newVMStatuses[i].Name < newVMStatuses[j].Name })

	if detailErr := writeAnsibleBindingDetails(ctx, client, u, newVMStatuses, ac.Generation, triggerValue); detailErr != nil {
		log.Printf("[AnsibleBinding/%s/%s] failed to persist per-VM status: %v", ac.Namespace, ac.Name, detailErr)
		recordErr(detailErr)
	}

	return firstErr
}

func writeAnsibleBindingDetails(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, vms []VMStatus, observedGeneration int64, lastTrigger string) error {
	vmsData := make([]interface{}, 0, len(vms))
	for _, v := range vms {
		m, err := structToMap(v)
		if err != nil {
			return fmt.Errorf("encoding VM status for %q: %w", v.Name, err)
		}
		vmsData = append(vmsData, m)
	}
	statusData := map[string]interface{}{
		"vms":                vmsData,
		"observedGeneration": observedGeneration,
		"lastAppliedTrigger": lastTrigger,
	}
	return patchStatus(ctx, client, ansBindGVR, obj, statusData, detailsFieldManager)
}

// cleanupAnsibleBinding deletes every AWX host this CR created
// (unless cleanupPolicy is Retain) before the finalizer is released.
//
// Failures are returned so the delete is retried rather than leaking the
// host. Only a genuinely unrecoverable situation - the AWXConnection or
// its Secret is gone, or is malformed in a way no retry can fix - is
// logged and skipped, since blocking the delete forever would not bring
// the host back either. A temporary failure (AWX unreachable, the API
// server erroring) is never one of those: abandoning a host there would
// leak an inventory entry whose IP AWX may later hand to an unrelated
// VM. To release a binding whose AWX instance is permanently gone, set
// spec.cleanupPolicy: Retain - that is honored on a terminating object.
func cleanupAnsibleBinding(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return nil
	}
	ac, err := convertAnsibleBinding(u)
	if err != nil || ac.Spec == nil || ac.Status == nil {
		return nil
	}
	if ac.Spec.CleanupPolicy == CleanupPolicyRetain {
		return nil
	}

	var toDelete []VMStatus
	for _, vs := range ac.Status.VMs {
		if vs.AWXHostID != 0 && vs.AWXHostCreated {
			toDelete = append(toDelete, vs)
		}
	}
	if len(toDelete) == 0 {
		return nil
	}

	abandon := func(reason string, err error) {
		log.Printf("[AnsibleBinding/%s/%s] cleanup: %s, abandoning %d AWX host(s): %v",
			ac.Namespace, ac.Name, reason, len(toDelete), err)
	}

	awxConnObj, err := client.Resource(awxConnGVR).Namespace(ac.Namespace).Get(ctx, ac.Spec.AWXConnectionRef, metav1.GetOptions{})
	if err != nil {
		if !isPermanent(err) {
			return fmt.Errorf("fetching AWXConnection %q to clean up %d AWX host(s): %w", ac.Spec.AWXConnectionRef, len(toDelete), err)
		}
		abandon(fmt.Sprintf("AWXConnection %q is gone", ac.Spec.AWXConnectionRef), err)
		return nil
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil || awxConn.Spec == nil {
		abandon(fmt.Sprintf("AWXConnection %q is malformed", ac.Spec.AWXConnectionRef), err)
		return nil
	}
	token, err := getSecretValue(ctx, client, ac.Namespace, awxConn.Spec.SecretRef, "token")
	if err != nil {
		if !isPermanent(err) {
			return fmt.Errorf("reading the AWX token to clean up %d AWX host(s): %w", len(toDelete), err)
		}
		abandon("the AWX token is gone", err)
		return nil
	}
	// A base path that will not resolve means AWX is unreachable right
	// now - exactly the transient case worth retrying rather than
	// leaking hosts over.
	awxClient, _, err := awxClientFor(ctx, client, awxConn, token)
	if err != nil {
		return fmt.Errorf("resolving the AWX API base path to clean up %d AWX host(s) "+
			"(set spec.cleanupPolicy: Retain to release this binding and leave them in place): %w", len(toDelete), err)
	}

	var firstErr error
	for _, vs := range toDelete {
		if err := awxClient.DeleteHost(ctx, int(vs.AWXHostID)); err != nil {
			log.Printf("[AnsibleBinding/%s/%s] cleanup: failed to delete AWX host %d for VM %q: %v", ac.Namespace, ac.Name, vs.AWXHostID, vs.Name, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("deleting AWX host %d for VM %q: %w", vs.AWXHostID, vs.Name, err)
			}
		}
	}
	return firstErr
}

// updateAnsibleBindingStatus derives the binding's aggregate state from
// the per-VM outcomes in status.vms.
//
// The generic updater cannot do this: it only sees whether the reconcile
// returned an error, and an AWX job that ran and failed is not a
// reconcile error - the controller did its job. Reporting Ready off the
// back of that would mark a binding healthy while every run under it was
// failing, or while no VM had run at all.
func updateAnsibleBindingStatus(u *unstructured.Unstructured, success bool, reconcileErr error) map[string]interface{} {
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

	ab, err := convertAnsibleBinding(u)
	if err != nil {
		return status("Pending", fmt.Sprintf("Could not read per-VM status: %s", err), false)
	}
	if ab.Status == nil || len(ab.Status.VMs) == 0 {
		return status("Pending", "No VirtualMachines match vmSelector yet.", false)
	}

	var pending, running, failed, succeeded []string
	for _, vm := range ab.Status.VMs {
		// Entries kept only to retry a host deletion are former targets,
		// not something the binding is waiting on.
		if vm.PendingCleanup {
			continue
		}
		switch vm.Phase {
		case PhaseSucceeded:
			succeeded = append(succeeded, vm.Name)
		case PhaseFailed:
			failed = append(failed, vm.Name)
		case PhaseRunning:
			running = append(running, vm.Name)
		default:
			pending = append(pending, vm.Name)
		}
	}

	total := len(pending) + len(running) + len(failed) + len(succeeded)
	switch {
	case total == 0:
		return status("Pending", "No VirtualMachines match vmSelector yet.", false)
	case len(failed) > 0:
		return status("Failed", fmt.Sprintf("%d of %d VM(s) failed their last run: %s.",
			len(failed), total, nameList(failed)), false)
	case len(running) > 0:
		return status("Running", fmt.Sprintf("%d of %d VM(s) still running: %s.",
			len(running), total, nameList(running)), false)
	case len(pending) > 0:
		return status("Pending", fmt.Sprintf("%d of %d VM(s) not ready to run (powered off, or no reported IP): %s.",
			len(pending), total, nameList(pending)), false)
	default:
		return status("Ready", fmt.Sprintf("All %d VM(s) completed the requested run successfully.", total), true)
	}
}

// nameList renders VM names for a status message, bounded so a selector
// matching hundreds of VMs does not produce an unreadable status.
func nameList(names []string) string {
	const max = 3
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:max], ", "), len(names)-max)
}
