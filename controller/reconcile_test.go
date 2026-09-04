package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestChildNameIsUnambiguousBoundedAndStable(t *testing.T) {
	// The collision plain concatenation allowed: one binding would sit
	// permanently failing on a VM it could not adopt.
	if childName("a-b", "c") == childName("a", "b-c") {
		t.Error("binding a-b + VM c must not produce the same child name as binding a + VM b-c")
	}

	if childName("bind", "web-1") != childName("bind", "web-1") {
		t.Error("child names must be deterministic: the parent creates blind and relies on AlreadyExists")
	}

	long := strings.Repeat("n", 300)
	name := childName(long, long)
	if len(name) > 253 {
		t.Errorf("child name must fit a DNS subdomain, got %d characters", len(name))
	}
	if strings.Contains(name, "--") || strings.HasSuffix(name, "-") {
		t.Errorf("truncation left a name that is not a valid DNS subdomain: %q", name)
	}

	// Truncation must not merge two distinct pairs into one name.
	if childName(long, long+"x") == childName(long, long+"y") {
		t.Error("two VMs whose names differ past the truncation point must still get distinct children")
	}

	// The readable half is still readable.
	if !strings.HasPrefix(childName("bind", "web-1"), "bind-web-1-") {
		t.Errorf("expected the binding and VM names to stay legible, got %q", childName("bind", "web-1"))
	}
}

func TestDueForPeriod(t *testing.T) {
	if due, _ := dueFor("", time.Minute); !due {
		t.Error("work that has never been done is always due")
	}
	if due, _ := dueFor("not a timestamp", time.Minute); !due {
		t.Error("an unreadable timestamp must be treated as never done, not as just done")
	}
	if due, _ := dueFor(time.Now().Add(-2*time.Minute).UTC().Format(time.RFC3339), time.Minute); !due {
		t.Error("a period that has elapsed is due")
	}
	due, remaining := dueFor(time.Now().Add(-10*time.Second).UTC().Format(time.RFC3339), time.Minute)
	if due {
		t.Error("a period that has not elapsed is not due")
	}
	if remaining <= 0 || remaining > time.Minute {
		t.Errorf("expected the wait to be what is left of the period, got %s", remaining)
	}
	// A timestamp from the future would otherwise park the work
	// arbitrarily far out.
	if due, _ := dueFor(time.Now().Add(time.Hour).UTC().Format(time.RFC3339), time.Minute); !due {
		t.Error("a timestamp in the future must not defer the work indefinitely")
	}
}

func TestChildRefusesAnOwnerThatIsNotItsVM(t *testing.T) {
	child := func(refs ...metav1.OwnerReference) *AnsibleBindingVM {
		return &AnsibleBindingVM{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-web-1-abc", OwnerReferences: refs},
			Spec:       &AnsibleBindingVMSpec{VMName: "web-1", BindingName: "bind"},
		}
	}
	vmRef := metav1.OwnerReference{APIVersion: vmGroup + "/v1alpha2", Kind: "VirtualMachine", Name: "web-1"}

	if err := checkOwnedByItsVM(child(vmRef)); err != nil {
		t.Errorf("a child owned by its own VM must reconcile: %v", err)
	}
	if err := checkOwnedByItsVM(child()); err == nil {
		t.Error("a child with no owner at all must be refused")
	}
	// The case a presence check would miss: a real ownerReference to a
	// real VirtualMachine that is not this child's VM.
	other := vmRef
	other.Name = "web-2"
	if err := checkOwnedByItsVM(child(other)); err == nil {
		t.Error("an ownerReference naming another VM must be refused")
	}
	wrongGroup := vmRef
	wrongGroup.APIVersion = "apps/v1"
	if err := checkOwnedByItsVM(child(wrongGroup)); err == nil {
		t.Error("an ownerReference to a VirtualMachine in another API group must be refused")
	}
	if err := checkOwnedByItsVM(child(wrongGroup, vmRef)); err != nil {
		t.Errorf("one valid reference among several is enough: %v", err)
	}
	if !isPermanent(checkOwnedByItsVM(child())) {
		t.Error("a hand-made child is a configuration error, not something to retry forever")
	}
}

func TestSummarizeCountsTerminatingChildrenSeparately(t *testing.T) {
	deleting := metav1.NewTime(time.Now())
	children := []AnsibleBindingVM{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Spec:       &AnsibleBindingVMSpec{VMName: "web-1"},
			Status:     &AnsibleBindingVMStatus{Phase: PhaseSucceeded},
		},
		{
			// Wedged on the way out: its VM is gone, so it is not
			// matched, and before this it vanished from the rollup
			// entirely.
			ObjectMeta: metav1.ObjectMeta{Name: "b", DeletionTimestamp: &deleting},
			Spec:       &AnsibleBindingVMSpec{VMName: "web-2"},
			Status:     &AnsibleBindingVMStatus{Phase: PhaseSucceeded},
		},
	}
	s := summarize(children, map[string]bool{"web-1": true})
	if s.Total != 1 || s.Succeeded != 1 {
		t.Errorf("expected the live child to be counted once, got %+v", s)
	}
	if s.Terminating != 1 {
		t.Errorf("expected the terminating child to be counted, got %+v", s)
	}

	// A binding whose every child is terminating must not read Ready.
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"summary": map[string]interface{}{"total": int64(0), "terminating": int64(1)},
		},
	}}
	status := updateAnsibleBindingStatus(obj, true, nil)
	if status["ready"] != false {
		t.Errorf("a binding with a stuck child must not be ready: %v", status)
	}
	if !strings.Contains(status["message"].(string), "cleaning up") {
		t.Errorf("expected the message to name the cleanup, got %q", status["message"])
	}
}

func TestIssueChildWritesBoundsTheBurst(t *testing.T) {
	var ran int64
	writes := make([]func() error, 1200)
	for i := range writes {
		writes[i] = func() error {
			atomic.AddInt64(&ran, 1)
			return nil
		}
	}
	issued, err := issueChildWrites(writes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issued != burstChildWrites || ran != int64(burstChildWrites) {
		t.Errorf("expected the burst to be capped at %d, issued %d and ran %d", burstChildWrites, issued, ran)
	}
}

func TestIssueChildWritesRunsBatchesInParallel(t *testing.T) {
	// Capped-but-sequential just moves the stall, so the batches have to
	// actually overlap. The second batch is two writes; if they are
	// issued one after another this deadlocks and the test times out.
	var wg sync.WaitGroup
	wg.Add(2)
	started := make(chan struct{}, 4)
	writes := []func() error{
		func() error { return nil },
		func() error { started <- struct{}{}; wg.Done(); wg.Wait(); return nil },
		func() error { started <- struct{}{}; wg.Done(); wg.Wait(); return nil },
	}
	if _, err := issueChildWrites(writes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(started) != 2 {
		t.Errorf("expected both writes in the second batch to run, got %d", len(started))
	}
}

func TestIssueChildWritesStopsOnAWhollyFailedBatchButNotAPartialOne(t *testing.T) {
	// One permanently broken VM must not starve every VM ordered behind
	// it, pass after pass.
	var ran int64
	writes := []func() error{
		func() error { atomic.AddInt64(&ran, 1); return fmt.Errorf("this one is broken") },
		func() error { atomic.AddInt64(&ran, 1); return nil },
		func() error { atomic.AddInt64(&ran, 1); return nil },
	}
	issued, err := issueChildWrites(writes)
	if err == nil {
		t.Error("expected the failure to be reported")
	}
	if issued != 3 || ran != 3 {
		t.Errorf("expected every write to be attempted despite one failing, issued %d ran %d", issued, ran)
	}

	// A batch in which everything failed is the systematic case - a
	// webhook rejecting every child, an exhausted quota - and stops the
	// burst instead of hammering the API server with the rest.
	ran = 0
	failing := make([]func() error, 100)
	for i := range failing {
		failing[i] = func() error { atomic.AddInt64(&ran, 1); return fmt.Errorf("quota exhausted") }
	}
	issued, err = issueChildWrites(failing)
	if err == nil {
		t.Error("expected the failure to be reported")
	}
	// One write, then a batch of two: the burst stops there rather than
	// issuing the other 97.
	if issued != 3 {
		t.Errorf("expected the burst to stop after the first wholly failed batch of more than one, issued %d", issued)
	}
}

func TestChildWithStatusCarriesTheEngineOwnedFields(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "field.vmware.com/v1",
		"kind":       "AnsibleBindingVM",
		"metadata":   map[string]interface{}{"name": "bind-web-1-abc", "namespace": "ns"},
		"status": map[string]interface{}{
			"state":     "Running",
			"ready":     false,
			"phase":     PhaseRunning,
			"lastJobID": int64(41),
		},
	}}

	out := childWithStatus(u, AnsibleBindingVMStatus{Phase: PhaseSucceeded, LastJobID: 42})

	phase, _, _ := unstructured.NestedString(out.Object, "status", "phase")
	if phase != PhaseSucceeded {
		t.Errorf("expected the status this pass computed, got %q", phase)
	}
	jobID, _, _ := unstructured.NestedInt64(out.Object, "status", "lastJobID")
	if jobID != 42 {
		t.Errorf("expected lastJobID 42, got %d", jobID)
	}
	// state/message/ready/lastUpdated belong to the engine's own field
	// manager and are what it is about to compute from this object.
	state, _, _ := unstructured.NestedString(out.Object, "status", "state")
	if state != "Running" {
		t.Errorf("expected the engine-owned state to be carried across, got %q", state)
	}
	// The object handed in must not be modified: it is the informer's
	// copy, shared with every other reader.
	original, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if original != PhaseRunning {
		t.Errorf("the object handed in was mutated: phase is now %q", original)
	}
}

func TestClaimedHostNamesCoversChildrenAndMatchedVMs(t *testing.T) {
	ac := &AnsibleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "bind", Namespace: "ns"},
		Spec:       &AnsibleBindingSpec{},
	}
	children := []AnsibleBindingVM{
		// Provisioned: claims the host name it recorded.
		{Spec: &AnsibleBindingVMSpec{VMName: "web-1"}, Status: &AnsibleBindingVMStatus{AWXHostName: "sup-web-1"}},
		// Created moments ago and yet to provision: claims the name it
		// is going to be given, or the reap would delete the host out
		// from under it.
		{Spec: &AnsibleBindingVMSpec{VMName: "web-2"}},
	}
	claimed := claimedHostNames(ac, "sup-", children, map[string]bool{"web-1": true, "web-2": true, "web-3": true})

	for _, name := range []string{"sup-web-1", "sup-web-2", "sup-web-3"} {
		if !claimed[name] {
			t.Errorf("expected %q to be claimed", name)
		}
	}
	if claimed["sup-web-9"] {
		t.Error("a host no child and no matched VM accounts for is exactly what reaping is for")
	}

	// A renamed host: the old name is still claimed by the child that
	// recorded it, so a rename in flight is not reaped mid-flight.
	renamed := claimedHostNames(ac, "new-", children, map[string]bool{"web-1": true})
	if !renamed["sup-web-1"] {
		t.Error("the host name a child last recorded stays claimed across a rename")
	}
}
