package main

import (
	"context"
	"fmt"
	"log"
	"sort"

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
func applyAnsibleBinding(client *dynamic.DynamicClient, obj interface{}, _ []string) error {
	ctx := context.Background()

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

	token, err := getSecretValue(client, ac.Namespace, awxConn.Spec.SecretRef, "token")
	if err != nil {
		return fmt.Errorf("reading AWX token from secret %q: %w", awxConn.Spec.SecretRef, err)
	}
	awxClient, _, err := awxClientFor(awxConn, token)
	if err != nil {
		return fmt.Errorf("resolving the API base path for AWXConnection %q: %w", ac.Spec.AWXConnectionRef, err)
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

	vms, err := listVirtualMachines(client, ac.Namespace, ac.Spec.VMSelector)
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

	// AWX silently drops launch fields the template isn't configured to
	// accept (ask_*_on_launch). A dropped limit means the template runs
	// against its ENTIRE inventory rather than the targeted VM, so refuse
	// to launch at all rather than widen the blast radius.
	if targetsHost && !tmpl.AskLimitOnLaunch {
		return fmt.Errorf("template %q does not accept a limit at launch time (ask_limit_on_launch is false), "+
			"so AWX would ignore the per-VM limit and run against the whole inventory: enable Prompt on Launch for Limit in AWX, "+
			"or set spec.useDefaultLimit: true to accept the template's own scope", ac.Spec.Template.Name)
	}
	if len(ac.Spec.ExtraVars) > 0 && !tmpl.AskVariablesOnLaunch {
		return fmt.Errorf("template %q does not accept extra variables at launch time (ask_variables_on_launch is false), "+
			"so AWX would ignore spec.extraVars: enable Prompt on Launch for Variables in AWX, or remove spec.extraVars",
			ac.Spec.Template.Name)
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
			// A renamed host (changed hostNamePrefix or spec.hostName)
			// would otherwise orphan the entry under the old name.
			if vs.AWXHostID != 0 && vs.AWXHostName != "" && vs.AWXHostName != hostName {
				if vs.AWXHostCreated && cleanupPolicy == CleanupPolicyDelete {
					if dErr := awxClient.DeleteHost(ctx, int(vs.AWXHostID)); dErr != nil {
						// Leave the recorded host in place and retry,
						// rather than losing track of it.
						recordErr(fmt.Errorf("retiring renamed AWX host %q for VM %q: %w", vs.AWXHostName, name, dErr))
						newVMStatuses = append(newVMStatuses, vs)
						continue
					}
				}
				vs.AWXHostID = 0
				vs.AWXHostCreated = false
				vs.HostVarsHash = ""
			}

			hostVars := map[string]string{"ansible_host": ip}
			for k, v := range ac.Spec.HostVariables {
				hostVars[k] = v
			}
			// Only touch AWX when the host or its variables actually
			// changed, instead of re-PATCHing on every resync.
			hash := hostVarsHash(hostName, hostVars)
			if vs.AWXHostID == 0 || vs.HostVarsHash != hash {
				hostID, owned, hErr := awxClient.UpsertHost(ctx, *tmpl.Inventory, hostName, hostOwnerMarker(ac.Namespace, ac.Name), hostVars)
				if hErr != nil {
					vs.Phase = PhaseFailed
					recordErr(fmt.Errorf("upserting AWX host for VM %q: %w", name, hErr))
					newVMStatuses = append(newVMStatuses, vs)
					continue
				}
				vs.AWXHostID = int64(hostID)
				vs.AWXHostName = hostName
				vs.AWXHostCreated = owned
				vs.HostVarsHash = hash
			}
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

	if detailErr := writeAnsibleBindingDetails(client, u, newVMStatuses, ac.Generation, triggerValue); detailErr != nil {
		log.Printf("[AnsibleBinding/%s/%s] failed to persist per-VM status: %v", ac.Namespace, ac.Name, detailErr)
		recordErr(detailErr)
	}

	return firstErr
}

func writeAnsibleBindingDetails(client *dynamic.DynamicClient, obj *unstructured.Unstructured, vms []VMStatus, observedGeneration int64, lastTrigger string) error {
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
	return patchStatus(context.Background(), client, ansBindGVR, obj, statusData, detailsFieldManager)
}

// cleanupAnsibleBinding deletes every AWX host this CR created
// (unless cleanupPolicy is Retain) before the finalizer is released.
//
// Host deletion failures are returned so the delete is retried rather
// than leaking the host. An unrecoverable situation - the AWXConnection
// or its Secret is already gone, so there's no way left to reach AWX -
// is logged and skipped instead, since blocking the delete forever would
// not bring the host back either.
func cleanupAnsibleBinding(client *dynamic.DynamicClient, obj interface{}) error {
	ctx := context.Background()
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

	awxConnObj, err := client.Resource(awxConnGVR).Namespace(ac.Namespace).Get(ctx, ac.Spec.AWXConnectionRef, metav1.GetOptions{})
	if err != nil {
		log.Printf("[AnsibleBinding/%s/%s] cleanup: AWXConnection %q unavailable, abandoning %d AWX host(s): %v", ac.Namespace, ac.Name, ac.Spec.AWXConnectionRef, len(toDelete), err)
		return nil
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil || awxConn.Spec == nil {
		log.Printf("[AnsibleBinding/%s/%s] cleanup: AWXConnection %q invalid, abandoning %d AWX host(s)", ac.Namespace, ac.Name, ac.Spec.AWXConnectionRef, len(toDelete))
		return nil
	}
	token, err := getSecretValue(client, ac.Namespace, awxConn.Spec.SecretRef, "token")
	if err != nil {
		log.Printf("[AnsibleBinding/%s/%s] cleanup: reading AWX token failed, abandoning %d AWX host(s): %v", ac.Namespace, ac.Name, len(toDelete), err)
		return nil
	}
	awxClient, _, err := awxClientFor(awxConn, token)
	if err != nil {
		log.Printf("[AnsibleBinding/%s/%s] cleanup: resolving the AWX API base path failed, abandoning %d AWX host(s): %v", ac.Namespace, ac.Name, len(toDelete), err)
		return nil
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
