package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

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
func applyAnsibleBinding(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) (Result, error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return Result{}, err
	}
	ac, err := convertAnsibleBinding(u)
	if err != nil {
		return Result{}, fmt.Errorf("decoding AnsibleBinding: %w", err)
	}
	if ac.Spec == nil {
		return Result{}, fmt.Errorf("spec is required")
	}
	if ac.Spec.AWXConnectionRef == "" {
		return Result{}, fmt.Errorf("spec.awxConnectionRef is required")
	}
	// An empty selector would match every VM in the namespace and run
	// the playbook against all of them. Refuse rather than guess.
	if len(ac.Spec.VMSelector) == 0 {
		return Result{}, fmt.Errorf("spec.vmSelector must not be empty: an empty selector would target every VM in the namespace")
	}
	if ac.Spec.Template.Type != TemplateTypeJob && ac.Spec.Template.Type != TemplateTypeWorkflow {
		return Result{}, fmt.Errorf("spec.template.type must be %q or %q, got %q", TemplateTypeJob, TemplateTypeWorkflow, ac.Spec.Template.Type)
	}

	vms, err := listVirtualMachines(ctx, client, ac.Namespace, ac.Spec.VMSelector)
	if err != nil {
		return Result{}, fmt.Errorf("listing target VMs: %w", err)
	}

	// A hostName override renames the inventory host, so it can only
	// apply to a single VM - across several matches they'd collide on
	// one AWX host.
	if ac.Spec.HostName != "" && len(vms) > 1 {
		return Result{}, fmt.Errorf("spec.hostName is set but vmSelector matches %d VMs: an inventory host name can only stand for one VM", len(vms))
	}

	children, err := listBindingChildrenCached(ctx, client, ac.Namespace, ac.Name)
	if err != nil {
		return Result{}, fmt.Errorf("listing the AnsibleBindingVMs for this binding: %w", err)
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

	var mu sync.Mutex
	var firstErr error
	recordErr := func(e error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = e
		}
	}

	matched := map[string]bool{}
	createdThisPass := false

	// The writes this pass wants, collected before any of them is
	// issued. A generation bump across a binding matching thousands of
	// VMs used to mean that many sequential Updates inside one reconcile
	// - the exact shape the split existed to remove, reappearing one
	// level up. Collecting them first is what lets the burst be bounded
	// and the rest carried to the next pass.
	var writes []func() error

	for i := range vms {
		vm := vms[i]
		name := vm.GetName()
		matched[name] = true

		desired := childSpecFor(&ac, name, triggerValue)

		existing, ok := childByVM[name]
		if !ok {
			seed := legacy[name]
			createdThisPass = true
			writes = append(writes, func() error {
				if cErr := createBindingChild(ctx, client, &ac, &vm, desired, seed); cErr != nil {
					if apierrors.IsAlreadyExists(cErr) {
						// Another pass got there first, or a child exists
						// under this name for a different binding. Either
						// way the next reconcile sees it in the list and
						// decides.
						return nil
					}
					return fmt.Errorf("creating the AnsibleBindingVM for VM %q: %w", name, cErr)
				}
				return nil
			})
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
		childNamespace, childObjName := existing.Namespace, existing.Name
		writes = append(writes, func() error {
			if uErr := applyBindingChildSpec(ctx, client, childNamespace, childObjName, desired); uErr != nil {
				return fmt.Errorf("updating the AnsibleBindingVM for VM %q: %w", name, uErr)
			}
			return nil
		})
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
		child := c
		writes = append(writes, func() error {
			if dErr := client.Resource(ansBindVMGVR).Namespace(child.Namespace).Delete(ctx, child.Name, metav1.DeleteOptions{}); dErr != nil && !apierrors.IsNotFound(dErr) {
				return fmt.Errorf("deleting the AnsibleBindingVM for VM %q: %w", child.Spec.VMName, dErr)
			}
			return nil
		})
	}

	issued, wErr := issueChildWrites(writes)
	if wErr != nil {
		recordErr(wErr)
	}
	deferred := len(writes) - issued

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

	// Orphan reaping is rare, destructive and costs an AWX request, so
	// it runs on its own period rather than every pass - and never while
	// writes are still outstanding, since the children that would claim
	// those hosts do not exist yet.
	orphanScan := ""
	if ac.Status != nil {
		orphanScan = ac.Status.LastOrphanScan
	}
	if due, _ := dueFor(orphanScan, orphanScanPeriod()); due && deferred == 0 && firstErr == nil {
		if rErr := reapOrphanHosts(ctx, client, &ac, children, matched); rErr != nil {
			recordErr(fmt.Errorf("reaping orphaned AWX hosts: %w", rErr))
		} else {
			orphanScan = nowRFC3339()
		}
	}

	if !ansibleBindingDetailsCurrent(ac.Status, summary, ac.Generation, triggerValue, clearLegacy, orphanScan) {
		if dErr := writeAnsibleBindingDetails(ctx, client, u, summary, ac.Generation, triggerValue, clearLegacy, orphanScan); dErr != nil {
			log.Printf("[AnsibleBinding/%s/%s] failed to persist status: %v", ac.Namespace, ac.Name, dErr)
			recordErr(dErr)
		}
	}

	result := Result{Object: bindingWithDetails(u, summary, ac.Generation, triggerValue, clearLegacy, orphanScan)}
	if deferred > 0 {
		// Level-triggered, so the next pass simply recomputes what is
		// still missing. Coming straight back keeps a large rollout
		// moving rather than waiting on the resync.
		log.Printf("[AnsibleBinding/%s/%s] issued %d child write(s), %d deferred to the next pass",
			ac.Namespace, ac.Name, issued, deferred)
		result.RequeueAfter = time.Second
	}
	return result, firstErr
}

// burstChildWrites bounds how many children one pass will create, update
// or delete. Past it the rest is carried to the next pass, which is
// bounded by --reconcile-timeout and would otherwise be spent entirely
// on one binding.
const burstChildWrites = 500

// issueChildWrites runs up to burstChildWrites of the collected writes,
// in parallel batches that double in size, and reports how many it
// issued.
//
// This is ReplicaSet's slowStartBatch: a capped burst on its own just
// moves the stall, because the writes are still sequential. Doubling
// from a small first batch means a systematic failure - a webhook
// rejecting every child, an exhausted quota - costs a handful of
// requests rather than the whole burst.
//
// It differs from ReplicaSet's in one way: a batch that fails only in
// part carries on. Stopping there would let one permanently broken VM
// starve every VM ordered behind it, pass after pass, since the order is
// stable. A batch in which everything failed is the systematic case, and
// that still stops the burst.
func issueChildWrites(writes []func() error) (int, error) {
	remaining := len(writes)
	if remaining > burstChildWrites {
		remaining = burstChildWrites
	}

	var firstErr error
	issued := 0
	index := 0
	for batchSize := min(remaining, 1); batchSize > 0; batchSize = min(2*batchSize, remaining) {
		errCh := make(chan error, batchSize)
		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func(idx int) {
				defer wg.Done()
				if err := writes[idx](); err != nil {
					errCh <- err
				}
			}(index + i)
		}
		wg.Wait()
		close(errCh)

		failures := 0
		for err := range errCh {
			failures++
			if firstErr == nil {
				firstErr = err
			}
		}
		index += batchSize
		issued += batchSize
		remaining -= batchSize
		// A batch of more than one in which everything failed is the
		// systematic case - a webhook rejecting every child, an
		// exhausted quota - and stops the burst. The first batch is a
		// single write, so this deliberately does not fire on it: one
		// permanently broken VM at the front of the list would otherwise
		// starve every VM behind it, pass after pass.
		if batchSize > 1 && failures == batchSize {
			return issued, firstErr
		}
	}
	return issued, firstErr
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

// childSpecFieldManager owns the child's spec. The parent applies the
// spec and nothing else, so the labels, annotations and ownerReference
// set at creation time - and the adopt-status annotation the child
// clears for itself - are owned by another manager and left alone.
const childSpecFieldManager = "ansible-supervisor-binding"

// applyBindingChildSpec server-side-applies the spec the binding wants
// on one child. The GET-then-Update it replaces was two round trips to
// write one field set, per child, on every generation bump.
func applyBindingChildSpec(ctx context.Context, client *dynamic.DynamicClient, namespace, name string, spec AnsibleBindingVMSpec) error {
	specMap, err := structToMap(spec)
	if err != nil {
		return fmt.Errorf("encoding spec: %w", err)
	}
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": ansBindVMGVR.GroupVersion().String(),
		"kind":       "AnsibleBindingVM",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": specMap,
	}}
	_, err = client.Resource(ansBindVMGVR).Namespace(namespace).Apply(
		ctx, name, child, metav1.ApplyOptions{FieldManager: childSpecFieldManager, Force: true},
	)
	return err
}

// listBindingChildrenCached lists a binding's children out of the
// informer store, through the namespace-and-binding index, so the
// parent's pass costs no API server read at all. The live list below is
// kept for the two places that must not act on a stale answer: releasing
// the binding's finalizer, and deleting an AWX host.
func listBindingChildrenCached(ctx context.Context, client *dynamic.DynamicClient, namespace, bindingName string) ([]AnsibleBindingVM, error) {
	if ansBindVMStore == nil {
		return listBindingChildren(ctx, client, namespace, bindingName)
	}
	objs, err := ansBindVMStore.ByIndex(childrenByBindingIndex, key(namespace, bindingName))
	if err != nil {
		return listBindingChildren(ctx, client, namespace, bindingName)
	}
	out := make([]AnsibleBindingVM, 0, len(objs))
	for _, obj := range objs {
		u, cErr := toUnstructured(obj)
		if cErr != nil {
			continue
		}
		c, cErr := convertAnsibleBindingVM(u)
		if cErr != nil {
			return nil, fmt.Errorf("decoding AnsibleBindingVM %q: %w", u.GetName(), cErr)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
		if c.Spec == nil {
			continue
		}
		// A child being deleted is counted apart from the phases rather
		// than dropped. Its VM is usually no longer matched - that is
		// what deleted it - so without this a child wedged in
		// Terminating disappears from the rollup entirely and the
		// binding above it reads Ready while something underneath it
		// retries forever.
		if !c.DeletionTimestamp.IsZero() {
			s.Terminating++
			continue
		}
		if !matched[c.Spec.VMName] {
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

func ansibleBindingDetailsCurrent(prior *AnsibleBindingStatus, summary BindingSummary, observedGeneration int64, lastTrigger string, clearLegacy bool, lastOrphanScan string) bool {
	if prior == nil {
		return false
	}
	if prior.ObservedGeneration != observedGeneration || prior.LastAppliedTrigger != lastTrigger {
		return false
	}
	if prior.LastOrphanScan != lastOrphanScan {
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

// ansibleBindingDetails is what this file's field manager owns in the
// binding's status.
func ansibleBindingDetails(summary BindingSummary, observedGeneration int64, lastTrigger string, clearLegacy bool, lastOrphanScan string) (map[string]interface{}, error) {
	summaryMap, err := structToMap(summary)
	if err != nil {
		return nil, fmt.Errorf("encoding the summary: %w", err)
	}
	statusData := map[string]interface{}{
		"summary":            summaryMap,
		"observedGeneration": observedGeneration,
		"lastAppliedTrigger": lastTrigger,
	}
	if lastOrphanScan != "" {
		statusData["lastOrphanScan"] = lastOrphanScan
	}
	if clearLegacy {
		// Owned by this field manager, so applying an empty list removes
		// the entries rather than merging with them.
		statusData["vms"] = []interface{}{}
	}
	return statusData, nil
}

func writeAnsibleBindingDetails(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, summary BindingSummary, observedGeneration int64, lastTrigger string, clearLegacy bool, lastOrphanScan string) error {
	statusData, err := ansibleBindingDetails(summary, observedGeneration, lastTrigger, clearLegacy, lastOrphanScan)
	if err != nil {
		return err
	}
	return patchStatus(ctx, client, ansBindGVR, obj, statusData, detailsFieldManager)
}

// bindingWithDetails is the binding as this pass leaves it: the object
// it was given, with the rollup just written merged in, so the engine
// can derive the aggregate state from it without re-reading the object
// it was handed a moment ago.
func bindingWithDetails(u *unstructured.Unstructured, summary BindingSummary, observedGeneration int64, lastTrigger string, clearLegacy bool, lastOrphanScan string) *unstructured.Unstructured {
	out := u.DeepCopy()
	details, err := ansibleBindingDetails(summary, observedGeneration, lastTrigger, clearLegacy, lastOrphanScan)
	if err != nil {
		return out
	}
	status, found, sErr := unstructured.NestedMap(out.Object, "status")
	if !found || sErr != nil {
		status = map[string]interface{}{}
	}
	for k, v := range details {
		status[k] = v
	}
	if err := unstructured.SetNestedMap(out.Object, status, "status"); err != nil {
		return u.DeepCopy()
	}
	return out
}

// reapOrphanHosts deletes AWX inventory hosts this binding owns that no
// child accounts for.
//
// A leaked host is by definition one no child knows about, so no child
// can find it - which is why this is the parent's job and not the
// child's. The marker filter means only hosts this controller created
// for this binding are ever considered; an adopted host carries no
// marker and cannot be returned here.
//
// The candidate list is built from the cached children, and then
// re-checked against a fresh list read from the API server immediately
// before anything is deleted. Reaping is rare and destructive, which is
// exactly when a quorum read is worth paying for: without it, a child
// created moments ago but not yet in the cache would have the host it is
// about to use deleted out from under a running playbook.
func reapOrphanHosts(ctx context.Context, client *dynamic.DynamicClient, ac *AnsibleBinding, children []AnsibleBindingVM, matched map[string]bool) error {
	if ac.Spec.CleanupPolicy == CleanupPolicyRetain {
		return nil
	}

	inventories := map[int64]bool{}
	for _, c := range children {
		if c.Status != nil && c.Status.AWXInventoryID != 0 {
			inventories[c.Status.AWXInventoryID] = true
		}
	}
	if len(inventories) == 0 {
		// No child has ever provisioned a host, so there is nothing this
		// binding could have left behind - and no inventory to look in
		// without resolving the template, which the parent no longer does.
		return nil
	}

	awxConnObj, err := getAWXConnection(ctx, client, ac.Namespace, ac.Spec.AWXConnectionRef)
	if err != nil {
		return fmt.Errorf("fetching AWXConnection %q: %w", ac.Spec.AWXConnectionRef, err)
	}
	awxConn, err := convertAWXConnection(awxConnObj)
	if err != nil || awxConn.Spec == nil {
		return fmt.Errorf("decoding AWXConnection %q: %w", ac.Spec.AWXConnectionRef, err)
	}
	awxClient, _, err := awxClientForConnection(ctx, client, awxConn)
	if err != nil {
		return fmt.Errorf("preparing a client for AWXConnection %q: %w", ac.Spec.AWXConnectionRef, err)
	}

	marker := hostOwnerMarker(ac.Namespace, ac.Name)
	claimed := claimedHostNames(ac, awxConn.Spec.HostNamePrefix, children, matched)

	var candidates []hostResult
	for inventory := range inventories {
		hosts, lErr := awxClient.ListOwnedHosts(ctx, int(inventory), marker)
		if lErr != nil {
			return lErr
		}
		for name, h := range hosts {
			if !claimed[name] {
				candidates = append(candidates, h)
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	fresh, err := listBindingChildren(ctx, client, ac.Namespace, ac.Name)
	if err != nil {
		return fmt.Errorf("re-reading the children before deleting an orphaned host: %w", err)
	}
	claimed = claimedHostNames(ac, awxConn.Spec.HostNamePrefix, fresh, matched)

	for _, h := range candidates {
		if claimed[h.Name] {
			continue
		}
		log.Printf("[AnsibleBinding/%s/%s] deleting orphaned AWX host %q (id %d): owned by this binding, claimed by no VM",
			ac.Namespace, ac.Name, h.Name, h.ID)
		if dErr := awxClient.DeleteHost(ctx, h.ID); dErr != nil {
			return fmt.Errorf("deleting orphaned AWX host %d: %w", h.ID, dErr)
		}
	}
	return nil
}

// claimedHostNames is every inventory host name this binding can account
// for: the one each child last recorded, and the one each currently
// matched VM would be given. The second half covers the child that
// exists but has not provisioned its host yet.
func claimedHostNames(ac *AnsibleBinding, prefix string, children []AnsibleBindingVM, matched map[string]bool) map[string]bool {
	claimed := map[string]bool{}
	for _, c := range children {
		if c.Status != nil && c.Status.AWXHostName != "" {
			claimed[c.Status.AWXHostName] = true
		}
		if c.Spec != nil {
			claimed[prefix+expectedHostName(ac, c.Spec.VMName)] = true
		}
	}
	for vmName := range matched {
		claimed[prefix+expectedHostName(ac, vmName)] = true
	}
	return claimed
}

func expectedHostName(ac *AnsibleBinding, vmName string) string {
	if ac.Spec.HostName != "" {
		return ac.Spec.HostName
	}
	return vmName
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
	if ab.Status == nil || ab.Status.Summary == nil || (ab.Status.Summary.Total == 0 && ab.Status.Summary.Terminating == 0) {
		return status("Pending", "No VirtualMachines match vmSelector yet.", false)
	}

	s := ab.Status.Summary
	switch {
	case s.Total == 0 && s.Terminating > 0:
		return status("Terminating", fmt.Sprintf("%d VM(s) still cleaning up.", s.Terminating), false)
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
	case s.Terminating > 0:
		// Every matched VM is done, but something underneath is still
		// being torn down. Ready would be a lie while a child is stuck
		// on an AWX host that will not delete.
		return status("Running", fmt.Sprintf("All %d VM(s) succeeded; %d child(ren) still cleaning up.", s.Total, s.Terminating), false)
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
