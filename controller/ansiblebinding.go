package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
)

// detailsFieldManager owns status.summary/observedGeneration/
// lastAppliedTrigger/vms. It's deliberately different from
// StatusFieldManager so the generic state/message/ready/lastUpdated
// patch (applied by the engine after provisionFunc returns) never
// clobbers these fields, and vice versa.
const detailsFieldManager = "ansible-supervisor-details"

// applyAnsibleBinding reconciles the set of AnsibleBindingVM children
// against the VMs vmSelector currently matches: one child per matched
// VM, and no child for a VM that has stopped matching.
//
// It does not talk to AWX at all. Everything that touches an inventory
// host or launches a job now happens on the child, which is what keeps
// this pass O(1) in API calls no matter how many VMs are selected, and
// lets several VMs reconcile in parallel instead of queueing behind one
// another inside a single binding's reconcile.
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
	if ac.Spec.Template.Type != TemplateTypeJob && ac.Spec.Template.Type != TemplateTypeWorkflow {
		return fmt.Errorf("spec.template.type must be %q or %q, got %q", TemplateTypeJob, TemplateTypeWorkflow, ac.Spec.Template.Type)
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

	children, err := listBindingChildren(ctx, client, ac.Namespace, ac.Name)
	if err != nil {
		return fmt.Errorf("listing the AnsibleBindingVMs for this binding: %w", err)
	}
	childByVM := map[string]AnsibleBindingVM{}
	for _, c := range children {
		if c.Spec == nil {
			continue
		}
		childByVM[c.Spec.VMName] = c
	}

	// Legacy per-VM entries from before the split. Their state is seeded
	// into the child at creation so the upgrade does not re-run every
	// playbook - see AdoptStatusAnnotation.
	legacy := map[string]VMStatus{}
	if ac.Status != nil {
		for _, v := range ac.Status.VMs {
			legacy[v.Name] = v
		}
	}

	triggerValue := ac.Annotations[ReconcileRequestedAtAnnotation]

	var firstErr error
	recordErr := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	matched := map[string]bool{}
	createdThisPass := false

	for i := range vms {
		vm := vms[i]
		name := vm.GetName()
		matched[name] = true

		desired := childSpecFor(&ac, name, triggerValue)

		existing, ok := childByVM[name]
		if !ok {
			if cErr := createBindingChild(ctx, client, &ac, &vm, desired, legacy[name]); cErr != nil {
				if apierrors.IsAlreadyExists(cErr) {
					// Another pass got there first, or a child exists
					// under this name for a different binding. Either way
					// the next reconcile sees it in the list and decides.
					continue
				}
				recordErr(fmt.Errorf("creating the AnsibleBindingVM for VM %q: %w", name, cErr))
				continue
			}
			createdThisPass = true
			continue
		}

		// Refuse to adopt a child another binding owns rather than
		// fighting over it - the same refusal the AWX host path makes on
		// its ownership marker.
		if existing.Spec.BindingName != ac.Name {
			recordErr(fmt.Errorf("AnsibleBindingVM %q is owned by binding %q, not %q",
				existing.Name, existing.Spec.BindingName, ac.Name))
			continue
		}
		if reflect.DeepEqual(*existing.Spec, desired) {
			continue
		}
		if uErr := updateBindingChildSpec(ctx, client, existing.Namespace, existing.Name, desired); uErr != nil {
			recordErr(fmt.Errorf("updating the AnsibleBindingVM for VM %q: %w", name, uErr))
		}
	}

	// A child whose VM no longer matches is deleted. Its own finalizer
	// then cleans up the AWX host, so nothing is leaked by removing it
	// here.
	for _, c := range children {
		if c.Spec == nil || matched[c.Spec.VMName] || c.Spec.BindingName != ac.Name {
			continue
		}
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		if dErr := client.Resource(ansBindVMGVR).Namespace(c.Namespace).Delete(ctx, c.Name, metav1.DeleteOptions{}); dErr != nil && !apierrors.IsNotFound(dErr) {
			recordErr(fmt.Errorf("deleting the AnsibleBindingVM for VM %q: %w", c.Spec.VMName, dErr))
		}
	}

	summary := summarize(children, matched)

	// status.vms is cleared only once every legacy entry has a child,
	// and never in the same pass that created one. Downgrading to a
	// pre-split controller before then still finds the old state intact.
	clearLegacy := len(legacy) > 0 && !createdThisPass
	if clearLegacy {
		for name := range legacy {
			if _, ok := childByVM[name]; !ok {
				clearLegacy = false
				break
			}
		}
	}

	if !ansibleBindingDetailsCurrent(ac.Status, summary, ac.Generation, triggerValue, clearLegacy) {
		if dErr := writeAnsibleBindingDetails(ctx, client, u, summary, ac.Generation, triggerValue, clearLegacy); dErr != nil {
			log.Printf("[AnsibleBinding/%s/%s] failed to persist status: %v", ac.Namespace, ac.Name, dErr)
			recordErr(dErr)
		}
	}

	return firstErr
}

// childSpecFor is the spec the binding wants on the child for one VM.
// Everything the child needs to reconcile - and to finalize after this
// binding is gone - is copied down rather than referenced.
func childSpecFor(ac *AnsibleBinding, vmName, trigger string) AnsibleBindingVMSpec {
	return AnsibleBindingVMSpec{
		VMName:            vmName,
		BindingName:       ac.Name,
		AWXConnectionRef:  ac.Spec.AWXConnectionRef,
		Template:          ac.Spec.Template,
		HostName:          ac.Spec.HostName,
		HostVariables:     ac.Spec.HostVariables,
		UseDefaultLimit:   ac.Spec.UseDefaultLimit,
		ExtraVars:         ac.Spec.ExtraVars,
		CleanupPolicy:     ac.Spec.CleanupPolicy,
		BindingGeneration: ac.Generation,
		BindingTrigger:    trigger,
	}
}

// createBindingChild creates one child, owned by the VirtualMachine.
//
// The owner reference is what makes a deleted VM delete this object
// without the binding having to notice: the garbage collector resolves
// owners by UID, so a VM deleted and recreated under the same name
// collects the old child rather than handing it to the new VM.
//
// seed, when non-zero, is the VM's state under the pre-split binding. It
// travels in an annotation because status is a subresource and cannot be
// set at creation time; the child reads it on its first pass, before it
// decides whether to launch anything.
func createBindingChild(ctx context.Context, client *dynamic.DynamicClient, ac *AnsibleBinding, vm *unstructured.Unstructured, spec AnsibleBindingVMSpec, seed VMStatus) error {
	specMap, err := structToMap(spec)
	if err != nil {
		return fmt.Errorf("encoding spec: %w", err)
	}

	annotations := map[string]interface{}{}
	if seed.Name != "" {
		seeded, sErr := json.Marshal(adoptStatusFrom(seed))
		if sErr != nil {
			return fmt.Errorf("encoding the migration seed: %w", sErr)
		}
		annotations[AdoptStatusAnnotation] = string(seeded)
	}

	meta := map[string]interface{}{
		"name":      childName(ac.Name, spec.VMName),
		"namespace": ac.Namespace,
		"labels": map[string]interface{}{
			BindingLabel: ac.Name,
		},
		"ownerReferences": []interface{}{
			map[string]interface{}{
				"apiVersion": vm.GetAPIVersion(),
				"kind":       vm.GetKind(),
				"name":       vm.GetName(),
				"uid":        string(vm.GetUID()),
			},
		},
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}

	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": ansBindVMGVR.GroupVersion().String(),
		"kind":       "AnsibleBindingVM",
		"metadata":   meta,
		"spec":       specMap,
	}}

	_, err = client.Resource(ansBindVMGVR).Namespace(ac.Namespace).Create(ctx, child, metav1.CreateOptions{})
	return err
}

// adoptStatusFrom converts a pre-split per-VM entry into the child's
// status. appliedGeneration and appliedTrigger are the fields that
// matter: without them the child concludes it has never run.
func adoptStatusFrom(v VMStatus) AnsibleBindingVMStatus {
	return AnsibleBindingVMStatus{
		ObservedIP:        v.ObservedIP,
		Phase:             v.Phase,
		AWXHostID:         v.AWXHostID,
		AWXInventoryID:    v.AWXInventoryID,
		AWXHostName:       v.AWXHostName,
		AWXHostCreated:    v.AWXHostCreated,
		LastJobID:         v.LastJobID,
		LastJobURL:        v.LastJobURL,
		LastJobStatus:     v.LastJobStatus,
		AppliedGeneration: v.AppliedGeneration,
		AppliedTrigger:    v.AppliedTrigger,
		History:           v.History,
	}
}

func updateBindingChildSpec(ctx context.Context, client *dynamic.DynamicClient, namespace, name string, spec AnsibleBindingVMSpec) error {
	specMap, err := structToMap(spec)
	if err != nil {
		return fmt.Errorf("encoding spec: %w", err)
	}
	current, err := client.Resource(ansBindVMGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedMap(current.Object, specMap, "spec"); err != nil {
		return err
	}
	_, err = client.Resource(ansBindVMGVR).Namespace(namespace).Update(ctx, current, metav1.UpdateOptions{})
	return err
}

func listBindingChildren(ctx context.Context, client *dynamic.DynamicClient, namespace, bindingName string) ([]AnsibleBindingVM, error) {
	list, err := client.Resource(ansBindVMGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{BindingLabel: bindingName}).String(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]AnsibleBindingVM, 0, len(list.Items))
	for i := range list.Items {
		c, cErr := convertAnsibleBindingVM(&list.Items[i])
		if cErr != nil {
			return nil, fmt.Errorf("decoding AnsibleBindingVM %q: %w", list.Items[i].GetName(), cErr)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// summarize rolls the children up into the fixed-size counts the binding
// reports. Children whose VM no longer matches are on their way out and
// are not something the binding is waiting on.
func summarize(children []AnsibleBindingVM, matched map[string]bool) BindingSummary {
	var s BindingSummary
	for _, c := range children {
		if c.Spec == nil || !matched[c.Spec.VMName] {
			continue
		}
		s.Total++
		var phase, state, message string
		if c.Status != nil {
			phase, state, message = c.Status.Phase, c.Status.State, c.Status.Message
		}
		switch {
		// A child that could not reconcile at all - an AWX template that
		// does not exist, a connection that will not resolve - has no run
		// phase to report, only the engine's Failed state. Counting that
		// as merely Pending would leave the binding green-ish and silent
		// about a misconfiguration the user has to fix, which is a worse
		// error surface than the per-VM list it replaced.
		case phase == PhaseFailed || state == "Failed":
			s.Failed++
			s.FailedVMs = append(s.FailedVMs, c.Spec.VMName)
			if s.FirstFailure == "" && message != "" {
				s.FirstFailure = message
			}
		case phase == PhaseSucceeded:
			s.Succeeded++
		case phase == PhaseRunning:
			s.Running++
		default:
			s.Pending++
		}
	}
	// Bounded so a binding matching hundreds of VMs cannot produce an
	// unreadable status object.
	if len(s.FailedVMs) > summaryNameLimit {
		s.FailedVMs = s.FailedVMs[:summaryNameLimit]
	}
	return s
}

// summaryNameLimit bounds how many failing VM names the rollup lists.
const summaryNameLimit = 3

func ansibleBindingDetailsCurrent(prior *AnsibleBindingStatus, summary BindingSummary, observedGeneration int64, lastTrigger string, clearLegacy bool) bool {
	if prior == nil {
		return false
	}
	if prior.ObservedGeneration != observedGeneration || prior.LastAppliedTrigger != lastTrigger {
		return false
	}
	if clearLegacy && len(prior.VMs) > 0 {
		return false
	}
	if prior.Summary == nil {
		return false
	}
	return reflect.DeepEqual(*prior.Summary, summary)
}

func writeAnsibleBindingDetails(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, summary BindingSummary, observedGeneration int64, lastTrigger string, clearLegacy bool) error {
	summaryMap, err := structToMap(summary)
	if err != nil {
		return fmt.Errorf("encoding the summary: %w", err)
	}
	statusData := map[string]interface{}{
		"summary":            summaryMap,
		"observedGeneration": observedGeneration,
		"lastAppliedTrigger": lastTrigger,
	}
	if clearLegacy {
		// Owned by this field manager, so applying an empty list removes
		// the entries rather than merging with them.
		statusData["vms"] = []interface{}{}
	}
	return patchStatus(ctx, client, ansBindGVR, obj, statusData, detailsFieldManager)
}

// cleanupAnsibleBinding deletes the binding's children and waits for
// them to go.
//
// The children are owned by their VirtualMachines, not by this object,
// so the garbage collector will not remove them when the binding goes -
// it has to be done here. Returning while any remain keeps the binding
// in Terminating, which is what makes each child's own finalizer run to
// completion before the binding disappears.
func cleanupAnsibleBinding(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return nil
	}
	ac, err := convertAnsibleBinding(u)
	if err != nil {
		return nil
	}

	children, err := listBindingChildren(ctx, client, ac.Namespace, ac.Name)
	if err != nil {
		return fmt.Errorf("listing the AnsibleBindingVMs to clean up: %w", err)
	}

	var remaining int
	for _, c := range children {
		if c.Spec != nil && c.Spec.BindingName != ac.Name {
			continue
		}
		remaining++
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		if dErr := client.Resource(ansBindVMGVR).Namespace(c.Namespace).Delete(ctx, c.Name, metav1.DeleteOptions{}); dErr != nil && !apierrors.IsNotFound(dErr) {
			return fmt.Errorf("deleting AnsibleBindingVM %q: %w", c.Name, dErr)
		}
	}
	if remaining > 0 {
		return fmt.Errorf("waiting for %d AnsibleBindingVM(s) to finish cleaning up", remaining)
	}
	return nil
}

// updateAnsibleBindingStatus derives the binding's aggregate state from
// the rollup applyAnsibleBinding wrote.
//
// The generic updater cannot do this: it only sees whether the reconcile
// returned an error, and an AWX job that ran and failed is not a
// reconcile error - the controller did its job. Reporting Ready off the
// back of that would mark a binding healthy while every run under it was
// failing.
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
		return status("Pending", fmt.Sprintf("Could not read the per-VM rollup: %s", err), false)
	}
	if ab.Status == nil || ab.Status.Summary == nil || ab.Status.Summary.Total == 0 {
		return status("Pending", "No VirtualMachines match vmSelector yet.", false)
	}

	s := ab.Status.Summary
	switch {
	case s.Failed > 0:
		msg := fmt.Sprintf("%d of %d VM(s) failed their last run: %s.", s.Failed, s.Total, nameList(s.FailedVMs))
		if s.FirstFailure != "" {
			msg += " " + s.FirstFailure
		}
		return status("Failed", msg, false)
	case s.Running > 0:
		return status("Running", fmt.Sprintf("%d of %d VM(s) still running.", s.Running, s.Total), false)
	case s.Pending > 0:
		return status("Pending", fmt.Sprintf("%d of %d VM(s) not ready to run (powered off, or no reported IP).", s.Pending, s.Total), false)
	default:
		return status("Ready", fmt.Sprintf("All %d VM(s) completed the requested run successfully.", s.Total), true)
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
