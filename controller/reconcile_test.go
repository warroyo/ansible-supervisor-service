package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestChildNameIsCanonicalPerVMBoundedAndStable(t *testing.T) {
	// The claim only works because every binding computes the same name
	// for one VM: two bindings selecting web-1 must collide on create.
	if childName("web-1") != childName("web-1") {
		t.Error("child names must be deterministic: the create is the arbitration")
	}
	if childName("web-1") == childName("web-2") {
		t.Error("different VMs must get different children")
	}

	long := strings.Repeat("n", 300)
	name := childName(long)
	if len(name) > 253 {
		t.Errorf("child name must fit a DNS subdomain, got %d characters", len(name))
	}
	if strings.Contains(name, "--") || strings.HasSuffix(name, "-") {
		t.Errorf("truncation left a name that is not a valid DNS subdomain: %q", name)
	}

	// Truncation must not merge two VMs into one claim.
	if childName(long+"x") == childName(long+"y") {
		t.Error("two VMs whose names differ past the truncation point must still get distinct children")
	}

	// The readable half is still readable.
	if !strings.HasPrefix(childName("web-1"), "vm-web-1-") {
		t.Errorf("expected the VM name to stay legible, got %q", childName("web-1"))
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
	vmRef := metav1.OwnerReference{APIVersion: vmGroup + "/v1alpha2", Kind: "VirtualMachine", Name: "web-1", UID: "vm-uid-1"}

	uid, err := checkOwnedByItsVM(child(vmRef))
	if err != nil {
		t.Errorf("a child owned by its own VM must reconcile: %v", err)
	}
	if uid != "vm-uid-1" {
		t.Errorf("expected the owner UID back so the VM read can be checked against it, got %q", uid)
	}
	if _, err := checkOwnedByItsVM(child()); err == nil {
		t.Error("a child with no owner at all must be refused")
	}
	// The case a presence check would miss: a real ownerReference to a
	// real VirtualMachine that is not this child's VM.
	other := vmRef
	other.Name = "web-2"
	if _, err := checkOwnedByItsVM(child(other)); err == nil {
		t.Error("an ownerReference naming another VM must be refused")
	}
	wrongGroup := vmRef
	wrongGroup.APIVersion = "apps/v1"
	if _, err := checkOwnedByItsVM(child(wrongGroup)); err == nil {
		t.Error("an ownerReference to a VirtualMachine in another API group must be refused")
	}
	if _, err := checkOwnedByItsVM(child(wrongGroup, vmRef)); err != nil {
		t.Errorf("one valid reference among several is enough: %v", err)
	}
	_, noOwner := checkOwnedByItsVM(child())
	if !isPermanent(noOwner) {
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
	s := summarize(children, map[string]bool{"web-1": true}, nil, 0, "")
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

func TestSummarizeCountsAChildOnAnOldGenerationAsPending(t *testing.T) {
	// The parent updates child specs and then summarizes the children as
	// they were before the update. Without this, a spec edit leaves the
	// binding Ready - with observedGeneration already bumped - describing
	// a run that answered the previous request.
	children := []AnsibleBindingVM{
		{
			Spec:   &AnsibleBindingVMSpec{VMName: "web-1"},
			Status: &AnsibleBindingVMStatus{Phase: PhaseSucceeded, AppliedGeneration: 3, AppliedTrigger: "t1"},
		},
	}
	matched := map[string]bool{"web-1": true}

	if got := summarize(children, matched, nil, 3, "t1"); got.Succeeded != 1 || got.Pending != 0 {
		t.Errorf("a child on the current generation counts as succeeded, got %+v", got)
	}
	if got := summarize(children, matched, nil, 4, "t1"); got.Pending != 1 || got.Succeeded != 0 {
		t.Errorf("a new generation must put the binding back to pending, got %+v", got)
	}
	if got := summarize(children, matched, nil, 3, "t2"); got.Pending != 1 || got.Succeeded != 0 {
		t.Errorf("a new re-run trigger must put the binding back to pending, got %+v", got)
	}

	// A failed run on an old generation is about to be retried, so it is
	// pending too - but a child that could not reconcile at all is a
	// misconfiguration blocking every generation, and stays visible.
	failedRun := []AnsibleBindingVM{{
		Spec:   &AnsibleBindingVMSpec{VMName: "web-1"},
		Status: &AnsibleBindingVMStatus{Phase: PhaseFailed, AppliedGeneration: 3},
	}}
	if got := summarize(failedRun, matched, nil, 4, ""); got.Pending != 1 || got.Failed != 0 {
		t.Errorf("an old failed run is pending a retry, got %+v", got)
	}
	brokenConfig := []AnsibleBindingVM{{
		Spec:   &AnsibleBindingVMSpec{VMName: "web-1"},
		Status: &AnsibleBindingVMStatus{State: "Failed", Message: "template not found", AppliedGeneration: 3},
	}}
	if got := summarize(brokenConfig, matched, nil, 4, ""); got.Failed != 1 {
		t.Errorf("a child that cannot reconcile at all stays failed on any generation, got %+v", got)
	}
}

func TestBindingLabelValueFitsAndStaysDistinct(t *testing.T) {
	short := "webserver-config"
	if bindingLabelValue(short) != short {
		t.Errorf("a name that fits must be used as-is, got %q", bindingLabelValue(short))
	}

	// Object names go to 253 characters, label values only to 63, and the
	// label is what children are found by - so a long binding name used to
	// make every child it created invalid.
	long := strings.Repeat("n", 200)
	got := bindingLabelValue(long)
	if len(got) > maxLabelValue {
		t.Errorf("label value is %d characters, over the %d limit", len(got), maxLabelValue)
	}
	if strings.HasSuffix(got, "-") || strings.HasPrefix(got, "-") {
		t.Errorf("label value %q is not a valid label value", got)
	}
	if bindingLabelValue(long+"a") == bindingLabelValue(long+"b") {
		t.Error("two long binding names must not share one label value: each would list the other's children")
	}
}

func TestAWXEndpointFingerprintDistinguishesInstances(t *testing.T) {
	a := awxEndpointFingerprint("https://awx.example.com", "/api/v2")
	if a != awxEndpointFingerprint("https://awx.example.com/", "/api/v2") {
		t.Error("a trailing slash is the same endpoint")
	}
	if a == awxEndpointFingerprint("https://awx-2.example.com", "/api/v2") {
		t.Error("a different instance must not look like the same one - its host ids mean something else")
	}
	if a == awxEndpointFingerprint("https://awx.example.com", "/api/controller/v2") {
		t.Error("a different API root is a different endpoint")
	}
}

func TestChildSpecPatchReplacesRatherThanMerges(t *testing.T) {
	// The bug this replaced: the child is created under one field manager
	// and was then updated by server-side apply under another, so fields
	// the apply omitted stayed owned by the creating manager and survived.
	// useDefaultLimit true -> false is omitempty, so it vanished from the
	// applied config and the child kept running against the whole
	// inventory.
	child := &AnsibleBindingVM{ObjectMeta: metav1.ObjectMeta{
		Name: "bind-web-1-abc", Namespace: "ns", UID: "child-uid", ResourceVersion: "42",
	}}
	spec := AnsibleBindingVMSpec{
		VMName: "web-1", BindingName: "bind", AWXConnectionRef: "awx",
		Template: TemplateRef{Name: "Configure Webserver", Type: TemplateTypeJob},
	}

	raw, err := childSpecPatch(child, spec)
	if err != nil {
		t.Fatalf("childSpecPatch: %v", err)
	}
	var ops []map[string]interface{}
	if err := json.Unmarshal(raw, &ops); err != nil {
		t.Fatalf("patch is not valid JSON Patch: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("expected uid test, resourceVersion test and a spec replace, got %d ops", len(ops))
	}
	if ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "child-uid" {
		t.Errorf("first op should pin the object identity, got %v", ops[0])
	}
	if ops[1]["op"] != "test" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "42" {
		t.Errorf("second op should pin the version read, got %v", ops[1])
	}
	if ops[2]["op"] != "replace" || ops[2]["path"] != "/spec" {
		t.Fatalf("third op should replace the whole spec, got %v", ops[2])
	}

	// The value carries no useDefaultLimit and no hostName at all - which
	// is the point: replacing the whole spec makes the API server apply
	// the CRD default rather than leaving the previous value in place.
	value := ops[2]["value"].(map[string]interface{})
	for _, absent := range []string{"useDefaultLimit", "hostName"} {
		if _, found := value[absent]; found {
			t.Errorf("%q should be absent from a spec that does not set it, got %v", absent, value[absent])
		}
	}
	if value["vmName"] != "web-1" {
		t.Errorf("the spec that is set must survive, got %v", value["vmName"])
	}
}

func TestSummarizeCountsAMatchedVMWithNoChildYet(t *testing.T) {
	// The child is created during the pass, so it is not in the snapshot
	// this pass summarized. Counting only the children present reported
	// 1 of 1 succeeded - Ready - while a matched VM had never run.
	children := []AnsibleBindingVM{{
		Spec:   &AnsibleBindingVMSpec{VMName: "web-1"},
		Status: &AnsibleBindingVMStatus{Phase: PhaseSucceeded, AppliedGeneration: 2, AppliedTrigger: "t1"},
	}}
	matched := map[string]bool{"web-1": true, "web-2": true}

	got := summarize(children, matched, nil, 2, "t1")
	if got.Total != 2 {
		t.Errorf("total should count every matched VM, got %d", got.Total)
	}
	if got.Succeeded != 1 || got.Pending != 1 {
		t.Errorf("the VM with no child yet is pending, got %+v", got)
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"summary": map[string]interface{}{
			"total": int64(got.Total), "succeeded": int64(got.Succeeded), "pending": int64(got.Pending),
		}},
	}}
	if status := updateAnsibleBindingStatus(obj, true, nil); status["ready"] != false {
		t.Errorf("a binding with a VM that has never run must not be ready: %v", status)
	}
}

func TestHasRecordedLaunchIdentity(t *testing.T) {
	full := AnsibleBindingVMStatus{
		LastJobID:         42,
		LastJobType:       TemplateTypeJob,
		LastJobConnection: &AWXConnectionSpec{URL: "https://awx.example.com", SecretRef: "awx-token", APIBasePath: "/api/v2"},
	}
	if !hasRecordedLaunchIdentity(full) {
		t.Error("a job launched with its identity recorded should be polled with it")
	}
	// A job launched before this was recorded has to stay pollable, or an
	// upgrade wedges one child per job in flight - a non-terminal job is
	// never abandoned, so it would never resolve.
	noIdentity := full
	noIdentity.LastJobConnection = nil
	noIdentity.LastJobType = ""
	if hasRecordedLaunchIdentity(noIdentity) {
		t.Error("a job with no recorded identity must fall back rather than claim one")
	}
	noType := full
	noType.LastJobType = ""
	if hasRecordedLaunchIdentity(noType) {
		t.Error("a connection without a template type cannot say which endpoint to poll")
	}
	noURL := full
	noURL.LastJobConnection = &AWXConnectionSpec{SecretRef: "awx-token"}
	if hasRecordedLaunchIdentity(noURL) {
		t.Error("a connection with no URL cannot be rebuilt")
	}
}
