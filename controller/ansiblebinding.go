package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// detailsFieldManager owns status.summary/observedGeneration/
// lastAppliedTrigger/lastOrphanScan. It's deliberately different from
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

	triggerValue := ac.Annotations[ReconcileRequestedAtAnnotation]

	var firstErr error
	recordErr := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	matched := map[string]bool{}
	vmUIDs := map[string]types.UID{}

	// Conflicts are recorded rather than returned as errors. A VM some
	// other binding owns is not a reconciliation failure - nothing is
	// broken, nothing will be fixed by retrying quickly, and treating it
	// as an error would bury the one thing the operator has to see
	// behind a generic "reconciliation failed". They are collected under
	// a lock because the create path records them from inside the write
	// batch, which runs in parallel.
	var conflictMu sync.Mutex
	conflicts := map[string]string{}
	recordConflict := func(vmName, owner string) {
		conflictMu.Lock()
		defer conflictMu.Unlock()
		conflicts[vmName] = owner
	}

	// The writes this pass wants, collected before any of them is
	// issued. A generation bump across a binding matching thousands of
	// VMs used to mean that many sequential Updates inside one reconcile
	// - the exact shape the split existed to remove, reappearing one
	// level up. Collecting them first is what lets the burst be bounded
	// and the rest carried to the next pass.
	var writes, deletions []func() error

	for i := range vms {
		vm := vms[i]
		name := vm.GetName()
		matched[name] = true
		vmUIDs[name] = vm.GetUID()

		desired := childSpecFor(&ac, name, triggerValue)
		canonical := childName(name)

		// This binding's own children come from the index it already
		// listed. Anything else claiming the canonical name is read out
		// of the shared informer store - one map lookup, no request -
		// because the binding index by definition cannot show a claim
		// held by a different binding.
		existing, ok := childByVM[name]
		if !ok {
			if cached, found := getCachedChild(ac.Namespace, canonical); found {
				existing, ok = *cached, true
			}
		}
		if !ok {
			// Not in any cache, which is not the same as not there. The
			// create is the arbitration: Kubernetes lets exactly one
			// object hold the name, so whoever it rejects has lost the
			// VM rather than merely raced with itself.
			writes = append(writes, func() error {
				cErr := createBindingChild(ctx, client, &ac, &vm, desired)
				if cErr == nil {
					return nil
				}
				if !apierrors.IsAlreadyExists(cErr) {
					return fmt.Errorf("creating the AnsibleBindingVM for VM %q: %w", name, cErr)
				}
				owner, gErr := client.Resource(ansBindVMGVR).Namespace(ac.Namespace).Get(ctx, canonical, metav1.GetOptions{})
				switch {
				case apierrors.IsNotFound(gErr):
					// Deleted between the create and the read. Nothing
					// is decided; the next pass tries again.
					return nil
				case gErr != nil:
					// An unread claim is not an available VM.
					return fmt.Errorf("reading AnsibleBindingVM %q to resolve the claim on VM %q: %w", canonical, name, gErr)
				}
				claimant, cvErr := convertAnsibleBindingVM(owner)
				if cvErr != nil {
					recordConflict(name, "an unreadable object under the claim name")
					return nil
				}
				if claimHeldBy(&claimant, &ac) {
					// Our own create from an earlier pass, seen before
					// the cache caught up.
					return nil
				}
				recordConflict(name, claimOwnerOf(&claimant))
				return nil
			})
			continue
		}

		// Refuse to adopt a claim another binding holds rather than
		// fighting over it - the same refusal the AWX host path makes on
		// its ownership marker, and for the same reason: two owners
		// running playbooks at one machine is worse than one binding
		// reporting that it cannot.
		if !claimHeldBy(&existing, &ac) {
			recordConflict(name, claimOwnerOf(&existing))
			continue
		}
		if !existing.DeletionTimestamp.IsZero() || !bindingChildMatchesVM(&existing, vm.GetUID()) {
			// Held until the child is really gone, finalizers included.
			// A VM replaced under the same name waits for its
			// predecessor's cleanup rather than provisioning over it.
			continue
		}
		if reflect.DeepEqual(*existing.Spec, desired) {
			continue
		}
		writes = append(writes, func() error {
			if uErr := updateBindingChildSpec(ctx, client, &existing, desired); uErr != nil {
				return fmt.Errorf("updating the AnsibleBindingVM for VM %q: %w", name, uErr)
			}
			return nil
		})
	}

	// Delete obsolete children independently of creation failures. In
	// particular, exhausted object quota must not block the deletes that
	// free it. A replacement VM must finish retiring its old child first.
	var summaryChildren []AnsibleBindingVM
	for _, c := range children {
		if c.Spec == nil || !claimHeldBy(&c, &ac) {
			continue
		}
		currentVM := matched[c.Spec.VMName] && bindingChildMatchesVM(&c, vmUIDs[c.Spec.VMName])
		if currentVM || !c.DeletionTimestamp.IsZero() {
			summaryChildren = append(summaryChildren, c)
		}
		if currentVM {
			continue
		}
		child := c
		deletions = append(deletions, func() error {
			return deleteBindingChild(ctx, client, &child, ac.Spec.CleanupPolicy)
		})
	}

	deleted, dErr := issueChildWrites(deletions)
	if dErr != nil {
		recordErr(dErr)
	}
	issued, wErr := issueChildWrites(writes)
	if wErr != nil {
		recordErr(wErr)
	}
	deferred := len(writes) - issued + len(deletions) - deleted
	issued += deleted

	summary := summarize(summaryChildren, matched, conflicts, ac.Generation, triggerValue)

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

	// An Event only on entering a conflict, or when which VMs are
	// conflicted changes. The helper creates a new Event per call, so a
	// binding that stays conflicted would otherwise write one per pass
	// for as long as the overlap lasts.
	if conflictWorthAnEvent(ac.Status, summary) {
		recordEvent(ctx, client, u, eventWarning, "VMClaimedByAnotherBinding", conflictMessage(summary))
	}

	if !ansibleBindingDetailsCurrent(ac.Status, summary, ac.Generation, triggerValue, orphanScan) {
		if dErr := writeAnsibleBindingDetails(ctx, client, u, summary, ac.Generation, triggerValue, orphanScan); dErr != nil {
			log.Printf("[AnsibleBinding/%s/%s] failed to persist status: %v", ac.Namespace, ac.Name, dErr)
			recordErr(dErr)
		}
	}

	result := Result{Object: bindingWithDetails(u, summary, ac.Generation, triggerValue, orphanScan)}
	if summary.Conflicted > 0 && firstErr == nil {
		// A claim is released by its owner's child being deleted, and
		// that wakes the owner, not the bindings waiting behind it. So a
		// waiter comes back on its own - slowly, and jittered, because
		// the alternative is waking every binding in the namespace on
		// every child status update to catch the rare handover.
		result.RequeueAfter = conflictRetryInterval + time.Duration(rand.Int63n(int64(conflictRetryJitter)))
	}
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

// maxWriteConcurrency caps how wide a batch gets. Past the client's own
// burst budget, more goroutines only queue deeper inside the rate
// limiter while each holds an object and a closure, so the batches stop
// doubling here rather than reaching burstChildWrites in one go.
const maxWriteConcurrency = 32

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
	for batchSize := min(remaining, 1); batchSize > 0; batchSize = min(2*batchSize, remaining, maxWriteConcurrency) {
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
		OnDeleted:         ac.Spec.OnDeleted,
		VMName:            vmName,
		BindingName:       ac.Name,
		BindingUID:        string(ac.UID),
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

// claimHeldBy reports whether a child is this binding incarnation's
// claim on its VM.
//
// Both halves of the identity are checked. The name alone would let a
// recreated binding inherit whatever the previous one left behind, and
// the UID alone would not survive being read from a child written by a
// controller that did not record it - which is a child from before this
// scheme existed, and is refused rather than adopted. The startup gate
// is what makes sure there are none left to refuse.
func claimHeldBy(child *AnsibleBindingVM, ac *AnsibleBinding) bool {
	if child == nil || child.Spec == nil || ac == nil {
		return false
	}
	return child.Spec.BindingName == ac.Name && child.Spec.BindingUID == string(ac.UID) && child.Spec.BindingUID != ""
}

// claimOwnerOf names the binding a claim belongs to, for a status
// message an operator has to act on.
func claimOwnerOf(child *AnsibleBindingVM) string {
	if child == nil || child.Spec == nil || child.Spec.BindingName == "" {
		return "an unrecognised object"
	}
	if child.Spec.BindingUID == "" {
		return child.Namespace + "/" + child.Spec.BindingName + " (an earlier controller's child)"
	}
	return child.Namespace + "/" + child.Spec.BindingName
}

// createBindingChild creates one child, owned by the VirtualMachine.
//
// The owner reference is what makes a deleted VM delete this object
// without the binding having to notice: the garbage collector resolves
// owners by UID, so a VM deleted and recreated under the same name
// collects the old child rather than handing it to the new VM.
func createBindingChild(ctx context.Context, client *dynamic.DynamicClient, ac *AnsibleBinding, vm *unstructured.Unstructured, spec AnsibleBindingVMSpec) error {
	specMap, err := structToMap(spec)
	if err != nil {
		return fmt.Errorf("encoding spec: %w", err)
	}

	meta := map[string]interface{}{
		"name":      childName(spec.VMName),
		"namespace": ac.Namespace,
		"labels": map[string]interface{}{
			BindingLabel: bindingLabelValue(ac.Name),
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
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": ansBindVMGVR.GroupVersion().String(),
		"kind":       "AnsibleBindingVM",
		"metadata":   meta,
		"spec":       specMap,
	}}

	_, err = client.Resource(ansBindVMGVR).Namespace(ac.Namespace).Create(ctx, child, metav1.CreateOptions{})
	return err
}

// updateBindingChildSpec replaces the entire parent-owned spec in one
// request. Unlike apply after Create, this removes omitted optional fields.
// Preconditions prevent stale informer data from updating a replacement;
// JSON Patch also cannot recreate a child that has been deleted.
func updateBindingChildSpec(ctx context.Context, client *dynamic.DynamicClient, child *AnsibleBindingVM, spec AnsibleBindingVMSpec) error {
	patch, err := childSpecPatch(child, spec)
	if err != nil {
		return err
	}
	_, err = client.Resource(ansBindVMGVR).Namespace(child.Namespace).Patch(ctx, child.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
	return err
}

// childSpecPatch replaces the whole spec, guarded on the child still
// being the object that was read.
//
// Replacing rather than merging is the point: an omitted optional field
// has to disappear, so that clearing spec.hostName clears it on the
// child, and useDefaultLimit going true to false actually narrows the
// run. Server-side apply after a Create does not do that - the creating
// field manager still owns the fields the apply leaves out, so they
// survive - which is how an unset useDefaultLimit stayed true and kept a
// run scoped to the whole inventory.
func childSpecPatch(child *AnsibleBindingVM, spec AnsibleBindingVMSpec) ([]byte, error) {
	patch, err := json.Marshal([]map[string]interface{}{
		{"op": "test", "path": "/metadata/uid", "value": string(child.UID)},
		{"op": "test", "path": "/metadata/resourceVersion", "value": child.ResourceVersion},
		{"op": "replace", "path": "/spec", "value": spec},
	})
	if err != nil {
		return nil, fmt.Errorf("encoding child spec patch: %w", err)
	}
	return patch, nil
}

func bindingChildMatchesVM(child *AnsibleBindingVM, uid types.UID) bool {
	ownerUID, err := checkOwnedByItsVM(child)
	return err == nil && ownerUID != "" && ownerUID == string(uid)
}

// deleteBindingChild deletes one child, first copying the policy in force
// down to it.
//
// Policy changes have to remain effective during finalization, when the
// normal parent reconcile no longer runs and so no longer copies its spec
// down - otherwise setting cleanupPolicy: Retain on a binding already
// stuck on an unreachable AWX changes nothing, which is the one thing the
// docs promise it does.
//
// An empty policy means "leave whatever the child already carries": the
// parent's spec is gone, so there is nothing to copy down and guessing
// would risk turning a Retain into a Delete.
func deleteBindingChild(ctx context.Context, client *dynamic.DynamicClient, child *AnsibleBindingVM, policy string) error {
	if policy != "" && child.Spec != nil && child.Spec.CleanupPolicy != policy {
		spec := *child.Spec
		spec.CleanupPolicy = policy
		if err := updateBindingChildSpec(ctx, client, child, spec); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("updating cleanup policy on AnsibleBindingVM %q: %w", child.Name, err)
		}
	}
	if !child.DeletionTimestamp.IsZero() {
		return nil
	}
	err := client.Resource(ansBindVMGVR).Namespace(child.Namespace).Delete(ctx, child.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &child.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting AnsibleBindingVM %q: %w", child.Name, err)
	}
	return nil
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
	objs, err := ansBindVMStore.ByIndex(childrenByBindingIndex, key(namespace, bindingLabelValue(bindingName)))
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
		LabelSelector: labels.SelectorFromSet(map[string]string{BindingLabel: bindingLabelValue(bindingName)}).String(),
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
//
// wantGeneration and wantTrigger are what the binding is currently
// asking for. A child that has not applied them yet counts as Pending
// whatever its last run says, because that run answered an earlier
// request: the parent updates child specs and then summarizes the
// children as they were before the update, so without this a spec edit
// or a re-run annotation leaves the binding reading Ready - with
// observedGeneration already bumped to the new generation - for the
// window between the request and the first child acting on it. Anything
// waiting on the binding would take that as "the new playbook has run".
func summarize(children []AnsibleBindingVM, matched map[string]bool, conflicts map[string]string, wantGeneration int64, wantTrigger string) BindingSummary {
	s := BindingSummary{Total: len(matched), Pending: len(matched) - len(conflicts), Conflicted: len(conflicts)}
	// Sorted so an unchanged conflict produces an unchanged status, and
	// bounded for the same reason the failure sample is: a selector
	// overlapping hundreds of VMs must not produce an unwritable object.
	for vmName, owner := range conflicts {
		s.ConflictedVMs = append(s.ConflictedVMs, fmt.Sprintf("%s (%s)", vmName, owner))
	}
	sort.Strings(s.ConflictedVMs)
	if len(s.ConflictedVMs) > summaryNameLimit {
		s.ConflictedVMs = s.ConflictedVMs[:summaryNameLimit]
	}
	seen := map[string]bool{}
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
		if !matched[c.Spec.VMName] || seen[c.Spec.VMName] || conflicts[c.Spec.VMName] != "" {
			continue
		}
		seen[c.Spec.VMName] = true
		s.Pending--
		var phase, state, message string
		stale := true
		if c.Status != nil {
			phase, state, message = c.Status.Phase, c.Status.State, c.Status.Message
			stale = c.Status.AppliedGeneration != wantGeneration || c.Status.AppliedTrigger != wantTrigger
		}
		switch {
		// A child that could not reconcile at all - an AWX template that
		// does not exist, a connection that will not resolve - has no run
		// phase to report, only the engine's Failed state. Counted
		// regardless of which generation it is on, because it is a
		// misconfiguration the user has to fix before any generation can
		// run. Counting it as merely Pending would leave the binding
		// green-ish and silent about it, which is a worse error surface
		// than the per-VM list it replaced.
		case state == "Failed":
			s.Failed++
			s.FailedVMs = append(s.FailedVMs, c.Spec.VMName)
			if s.FirstFailure == "" && message != "" {
				s.FirstFailure = message
			}
		// Everything below is about a run, and a run that answered an
		// earlier request says nothing about this one.
		case stale:
			s.Pending++
		case phase == PhaseFailed:
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

// How long a binding waits before looking again at a VM another binding
// owns. Long, because a handover is rare and the wait costs nothing but
// latency on it; jittered, because a namespace whose bindings all
// overlap would otherwise retry in lockstep forever.
const (
	conflictRetryInterval = 30 * time.Second
	conflictRetryJitter   = 10 * time.Second
)

// conflictMessage says what is conflicted and what to do about it.
func conflictMessage(s BindingSummary) string {
	return fmt.Sprintf("%d of %d selected VM(s) are claimed by another binding: %s. "+
		"Narrow vmSelector, or release the existing owner; one binding owns a VM's whole lifecycle, "+
		"so several playbooks for one VM belong in one AWX workflow under it.",
		s.Conflicted, s.Total, nameList(s.ConflictedVMs))
}

// conflictWorthAnEvent reports whether this pass found something an
// operator has not already been told, so a standing conflict costs no
// writes at all.
func conflictWorthAnEvent(prior *AnsibleBindingStatus, summary BindingSummary) bool {
	if summary.Conflicted == 0 {
		return false
	}
	if prior == nil || prior.Summary == nil {
		return true
	}
	return prior.Summary.Conflicted != summary.Conflicted ||
		!reflect.DeepEqual(prior.Summary.ConflictedVMs, summary.ConflictedVMs)
}

func ansibleBindingDetailsCurrent(prior *AnsibleBindingStatus, summary BindingSummary, observedGeneration int64, lastTrigger string, lastOrphanScan string) bool {
	if prior == nil {
		return false
	}
	if prior.ObservedGeneration != observedGeneration || prior.LastAppliedTrigger != lastTrigger {
		return false
	}
	if prior.LastOrphanScan != lastOrphanScan {
		return false
	}
	if prior.Summary == nil {
		return false
	}
	return reflect.DeepEqual(*prior.Summary, summary)
}

// ansibleBindingDetails is what this file's field manager owns in the
// binding's status.
func ansibleBindingDetails(summary BindingSummary, observedGeneration int64, lastTrigger string, lastOrphanScan string) (map[string]interface{}, error) {
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
	return statusData, nil
}

func writeAnsibleBindingDetails(ctx context.Context, client *dynamic.DynamicClient, obj *unstructured.Unstructured, summary BindingSummary, observedGeneration int64, lastTrigger string, lastOrphanScan string) error {
	statusData, err := ansibleBindingDetails(summary, observedGeneration, lastTrigger, lastOrphanScan)
	if err != nil {
		return err
	}
	return patchStatus(ctx, client, ansBindGVR, obj, statusData, detailsFieldManager)
}

// bindingWithDetails is the binding as this pass leaves it: the object
// it was given, with the rollup just written merged in, so the engine
// can derive the aggregate state from it without re-reading the object
// it was handed a moment ago.
func bindingWithDetails(u *unstructured.Unstructured, summary BindingSummary, observedGeneration int64, lastTrigger string, lastOrphanScan string) *unstructured.Unstructured {
	out := u.DeepCopy()
	details, err := ansibleBindingDetails(summary, observedGeneration, lastTrigger, lastOrphanScan)
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
func cleanupAnsibleBinding(ctx context.Context, client *dynamic.DynamicClient, obj interface{}) (CleanupResult, error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return CleanupResult{Done: true}, nil
	}
	ac, err := convertAnsibleBinding(u)
	if err != nil {
		return CleanupResult{Done: true}, nil
	}

	// Waiting is read from the cache. A binding whose children are
	// running teardown playbooks waits for as long as the slowest of
	// them, and a live LIST every pass for the whole of that - one
	// request, but a response carrying every remaining child - is the
	// most expensive thing a terminating binding does.
	children, err := listBindingChildrenCached(ctx, client, ac.Namespace, ac.Name)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("listing the AnsibleBindingVMs to clean up: %w", err)
	}
	remaining, err := deleteBindingChildren(ctx, client, &ac, children)
	if err != nil {
		return CleanupResult{}, err
	}
	if remaining > 0 {
		// Waiting for children that are finalizing normally - a
		// deprovision hook can take minutes - is not a failure, so it is
		// not reported as one. Each child that finishes wakes the parent
		// through its child watch, so this interval is the backstop for a
		// missed event rather than the mechanism.
		debugf("[AnsibleBinding/%s/%s] waiting for %d AnsibleBindingVM(s) to finish cleaning up",
			ac.Namespace, ac.Name, remaining)
		return CleanupResult{RequeueAfter: childCleanupPollInterval}, nil
	}

	// The cache says there is nothing left, which is not something to
	// release a finalizer on: a cache lags, and a child it has not seen
	// yet would be abandoned with its AWX host and its teardown playbook
	// unrun. Releasing is rare and irreversible, so it is worth the one
	// live read - the same rule the orphan reaper follows before it
	// deletes anything.
	fresh, err := listBindingChildren(ctx, client, ac.Namespace, ac.Name)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("confirming the AnsibleBindingVMs are gone: %w", err)
	}
	remaining, err = deleteBindingChildren(ctx, client, &ac, fresh)
	if err != nil {
		return CleanupResult{}, err
	}
	if remaining > 0 {
		debugf("[AnsibleBinding/%s/%s] the cache said no children remained, but %d are still there",
			ac.Namespace, ac.Name, remaining)
		return CleanupResult{RequeueAfter: childCleanupPollInterval}, nil
	}
	return CleanupResult{Done: true}, nil
}

// deleteBindingChildren deletes the children a binding still owns and
// reports how many it is waiting on.
//
// The policy in force is copied down into each one first: finalization
// no longer runs the normal reconcile that would otherwise do it, so
// without this, setting cleanupPolicy: Retain on a binding already stuck
// on an unreachable AWX would change nothing.
func deleteBindingChildren(ctx context.Context, client *dynamic.DynamicClient, ac *AnsibleBinding, children []AnsibleBindingVM) (int, error) {
	var remaining int
	for _, c := range children {
		if c.Spec != nil && c.Spec.BindingName != ac.Name {
			continue
		}
		remaining++
		policy := ""
		if ac.Spec != nil {
			policy = ac.Spec.CleanupPolicy
		}
		if err := deleteBindingChild(ctx, client, &c, policy); err != nil {
			return remaining, err
		}
	}
	return remaining, nil
}

// childCleanupPollInterval is how often a binding whose children are
// still finalizing looks again on its own. It is long because it is not
// the thing that drives progress: a child finishing wakes the parent
// through the child watch, and this only has to cover an event that
// never arrived.
const childCleanupPollInterval = 30 * time.Second

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
	// Conflict outranks a failed run. A run that failed is this
	// binding's own work going wrong and its detail is in the summary
	// either way; a VM claimed elsewhere is a misconfiguration nothing
	// will resolve on its own, and it is the reason those VMs are not
	// running at all.
	case s.Conflicted > 0:
		msg := conflictMessage(*s)
		if s.Failed > 0 {
			msg += fmt.Sprintf(" %d other VM(s) also failed their last run.", s.Failed)
		}
		return status("Conflict", msg, false)
	case s.Failed > 0:
		msg := fmt.Sprintf("%d of %d VM(s) failed their last run: %s.", s.Failed, s.Total, nameList(s.FailedVMs))
		if s.FirstFailure != "" {
			msg += " " + s.FirstFailure
		}
		return status("Failed", msg, false)
	case s.Running > 0:
		return status("Running", fmt.Sprintf("%d of %d VM(s) still running.", s.Running, s.Total), false)
	case s.Pending > 0:
		return status("Pending", fmt.Sprintf("%d of %d VM(s) have not completed this request yet (not yet started, powered off, or no reported IP).", s.Pending, s.Total), false)
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
