package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// bindingWith builds the unstructured form of an AnsibleBinding the way
// the API server hands one back, so status conversion is exercised for
// real rather than against a hand-built map.
func bindingWith(t *testing.T, summary *BindingSummary) *unstructured.Unstructured {
	t.Helper()
	ab := AnsibleBinding{Status: &AnsibleBindingStatus{Summary: summary}}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ab)
	if err != nil {
		t.Fatalf("building unstructured binding: %v", err)
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestUpdateAnsibleBindingStatus(t *testing.T) {
	tests := []struct {
		name      string
		summary   *BindingSummary
		wantState string
		wantReady bool
	}{
		{"no children yet", nil, "Pending", false},
		{"nothing matched", &BindingSummary{}, "Pending", false},
		{"all succeeded", &BindingSummary{Total: 2, Succeeded: 2}, "Ready", true},
		{"one running", &BindingSummary{Total: 2, Succeeded: 1, Running: 1}, "Running", false},
		{"one pending", &BindingSummary{Total: 2, Succeeded: 1, Pending: 1}, "Pending", false},
		{
			"a failure outranks a run still in flight",
			&BindingSummary{Total: 3, Running: 1, Failed: 1, Succeeded: 1, FailedVMs: []string{"web-2"}},
			"Failed", false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := updateAnsibleBindingStatus(bindingWith(t, tc.summary), true, nil)
			if got["state"] != tc.wantState {
				t.Errorf("state = %v, want %v", got["state"], tc.wantState)
			}
			if got["ready"] != tc.wantReady {
				t.Errorf("ready = %v, want %v", got["ready"], tc.wantReady)
			}
		})
	}
}

func TestUpdateAnsibleBindingStatusCarriesFirstFailure(t *testing.T) {
	// "Why is this binding red" has to be answerable on the binding
	// itself, without listing children, or the rollup has cost the user
	// the thing the per-VM list used to give them.
	got := updateAnsibleBindingStatus(bindingWith(t, &BindingSummary{
		Total: 2, Succeeded: 1, Failed: 1,
		FailedVMs:    []string{"web-2"},
		FirstFailure: "Job 91 failed.",
	}), true, nil)
	msg := got["message"].(string)
	if !strings.Contains(msg, "web-2") || !strings.Contains(msg, "Job 91 failed.") {
		t.Errorf("message %q should name the failing VM and carry its message", msg)
	}
}

func TestUpdateAnsibleBindingStatusReconcileError(t *testing.T) {
	// A reconcile error wins outright: even an all-succeeded rollup
	// describes the last run, not the state of the world the controller
	// just failed to observe.
	got := updateAnsibleBindingStatus(
		bindingWith(t, &BindingSummary{Total: 1, Succeeded: 1}),
		false, errors.New("AWX unreachable"),
	)
	if got["state"] != "Failed" || got["ready"] != false {
		t.Fatalf("got state=%v ready=%v, want Failed/false", got["state"], got["ready"])
	}
	if !strings.Contains(got["message"].(string), "AWX unreachable") {
		t.Errorf("message %q does not carry the underlying error", got["message"])
	}
}

func TestSummarizeCountsOnlyMatchedChildren(t *testing.T) {
	children := []AnsibleBindingVM{
		{Spec: &AnsibleBindingVMSpec{VMName: "web-1"}, Status: &AnsibleBindingVMStatus{Phase: PhaseSucceeded}},
		{Spec: &AnsibleBindingVMSpec{VMName: "web-2"}, Status: &AnsibleBindingVMStatus{Phase: PhaseFailed, Message: "Job 91 failed."}},
		{Spec: &AnsibleBindingVMSpec{VMName: "web-3"}, Status: &AnsibleBindingVMStatus{Phase: PhaseRunning}},
		{Spec: &AnsibleBindingVMSpec{VMName: "web-4"}},
		// Already deleted from the selector: on its way out, not
		// something the binding is waiting on.
		{Spec: &AnsibleBindingVMSpec{VMName: "gone-1"}, Status: &AnsibleBindingVMStatus{Phase: PhaseFailed}},
	}
	matched := map[string]bool{"web-1": true, "web-2": true, "web-3": true, "web-4": true}

	got := summarize(children, matched)
	want := BindingSummary{
		Total: 4, Succeeded: 1, Failed: 1, Running: 1, Pending: 1,
		FailedVMs: []string{"web-2"}, FirstFailure: "Job 91 failed.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summarize = %+v, want %+v", got, want)
	}
}

// A child that never got as far as a run phase - bad template name,
// unresolvable connection - still has to reach the binding, or a
// misconfiguration is invisible on the object the user is looking at.
func TestSummarizeCountsReconcileFailures(t *testing.T) {
	children := []AnsibleBindingVM{{
		Spec: &AnsibleBindingVMSpec{VMName: "web-1"},
		Status: &AnsibleBindingVMStatus{
			State:   "Failed",
			Message: `Reconciliation failed: template "Nope" does not accept a limit at launch time`,
		},
	}}
	got := summarize(children, map[string]bool{"web-1": true})
	if got.Failed != 1 || got.Pending != 0 {
		t.Fatalf("a child that failed to reconcile should count as failed, got %+v", got)
	}
	if !strings.Contains(got.FirstFailure, "ask_limit") && !strings.Contains(got.FirstFailure, "does not accept a limit") {
		t.Errorf("the child's reason should reach the binding, got %q", got.FirstFailure)
	}
}

func TestSummarizeBoundsFailedNames(t *testing.T) {
	var children []AnsibleBindingVM
	matched := map[string]bool{}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("web-%d", i)
		matched[name] = true
		children = append(children, AnsibleBindingVM{
			Spec:   &AnsibleBindingVMSpec{VMName: name},
			Status: &AnsibleBindingVMStatus{Phase: PhaseFailed},
		})
	}
	got := summarize(children, matched)
	if got.Failed != 20 {
		t.Errorf("failed count = %d, want 20", got.Failed)
	}
	if len(got.FailedVMs) != summaryNameLimit {
		t.Errorf("named %d failing VMs, want it bounded to %d", len(got.FailedVMs), summaryNameLimit)
	}
}

// The single most dangerous bug available in this change: a child
// created during migration without its previous appliedGeneration
// concludes it has never run, and every VM under every binding
// re-launches its playbook at once on upgrade.
func TestMigratedChildDoesNotRelaunch(t *testing.T) {
	legacy := VMStatus{
		Name: "web-1", Phase: PhaseSucceeded,
		AWXHostID: 55, AWXHostCreated: true, AWXHostName: "sup-web-1",
		LastJobID: 91, LastJobStatus: "successful",
		AppliedGeneration: 4, AppliedTrigger: "t1",
	}
	spec := &AnsibleBindingVMSpec{BindingGeneration: 4, BindingTrigger: "t1"}

	seeded := adoptStatusFrom(legacy)
	if needsRun(seeded, spec) {
		t.Fatal("a migrated child must not relaunch: the upgrade would re-run every playbook in the fleet")
	}

	// The same child with no seed at all is exactly the failure above.
	if !needsRun(AnsibleBindingVMStatus{}, spec) {
		t.Fatal("an unseeded child should look like it has never run - this guards the test above")
	}

	// A real change still gets through.
	if !needsRun(seeded, &AnsibleBindingVMSpec{BindingGeneration: 5, BindingTrigger: "t1"}) {
		t.Error("a new binding generation must reach the VM")
	}
	if !needsRun(seeded, &AnsibleBindingVMSpec{BindingGeneration: 4, BindingTrigger: "t2"}) {
		t.Error("a new re-run trigger must reach the VM")
	}
}

func TestPriorVMStateReadsTheAdoptAnnotation(t *testing.T) {
	seed := AnsibleBindingVMStatus{LastJobID: 91, AppliedGeneration: 4, Phase: PhaseSucceeded}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshalling seed: %v", err)
	}
	child := &AnsibleBindingVM{}
	child.Annotations = map[string]string{AdoptStatusAnnotation: string(raw)}

	got, adopted, err := priorVMState(child)
	if err != nil {
		t.Fatalf("priorVMState: %v", err)
	}
	if !adopted {
		t.Error("a child seeded from the annotation should report that it adopted")
	}
	if got.LastJobID != 91 || got.AppliedGeneration != 4 {
		t.Errorf("seed not read: %+v", got)
	}

	// Real status always wins: the annotation is a one-time seed and must
	// never be replayed over state the child has since written itself.
	child.Status = &AnsibleBindingVMStatus{LastJobID: 92, AppliedGeneration: 5, Phase: PhaseRunning}
	got, adopted, err = priorVMState(child)
	if err != nil {
		t.Fatalf("priorVMState: %v", err)
	}
	if adopted || got.LastJobID != 92 {
		t.Errorf("existing status should win over the seed, got %+v (adopted=%v)", got, adopted)
	}
}

func TestChildSpecCopiesBindingState(t *testing.T) {
	ab := &AnsibleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "webserver-config", Generation: 7},
		Spec: &AnsibleBindingSpec{
			AWXConnectionRef: "prod-awx",
			Template:         TemplateRef{Name: "Configure Webserver", Type: TemplateTypeJob},
			HostVariables:    map[string]string{"datacenter": "dc-west"},
			CleanupPolicy:    CleanupPolicyRetain,
		},
	}
	got := childSpecFor(ab, "web-1", "t9")

	if got.VMName != "web-1" || got.BindingName != "webserver-config" {
		t.Errorf("child not bound to the right VM/binding: %+v", got)
	}
	// The child has to be able to finalize after its parent is gone, so
	// everything it needs is copied rather than referenced.
	if got.AWXConnectionRef != "prod-awx" || got.Template.Name != "Configure Webserver" ||
		got.CleanupPolicy != CleanupPolicyRetain || got.HostVariables["datacenter"] != "dc-west" {
		t.Errorf("binding spec not copied down: %+v", got)
	}
	if got.BindingGeneration != 7 || got.BindingTrigger != "t9" {
		t.Errorf("generation/trigger not copied: gen=%d trigger=%q", got.BindingGeneration, got.BindingTrigger)
	}
}

// The whole point of holding the details steady is that an idle object
// produces byte-identical status, so the apply can be skipped and the
// resource stops writing to etcd once per resync forever.
func TestAnsibleBindingVMDetailsCurrent(t *testing.T) {
	st := AnsibleBindingVMStatus{Phase: PhaseSucceeded, LastJobID: 7, AWXHostID: 55}
	prior := st
	prior.State, prior.Message, prior.Ready = "Ready", "Job 7 completed successfully.", true
	prior.LastUpdated = "2026-09-04T10:00:00Z"

	if !ansibleBindingVMDetailsCurrent(&prior, st) {
		t.Error("the engine's own status fields must not make the details look changed")
	}
	if ansibleBindingVMDetailsCurrent(nil, st) {
		t.Error("an object with no status yet is never current")
	}

	changed := st
	changed.Phase = PhaseFailed
	if ansibleBindingVMDetailsCurrent(&prior, changed) {
		t.Error("a changed phase is a real change")
	}

	withHistory := st
	withHistory.History = []VMRunHistoryEntry{{JobID: 7, Status: "successful"}}
	if ansibleBindingVMDetailsCurrent(&prior, withHistory) {
		t.Error("a new history entry is a real change")
	}
}

func TestAnsibleBindingDetailsCurrent(t *testing.T) {
	summary := BindingSummary{Total: 2, Succeeded: 2}
	prior := &AnsibleBindingStatus{ObservedGeneration: 3, LastAppliedTrigger: "t1", Summary: &summary}

	if !ansibleBindingDetailsCurrent(prior, summary, 3, "t1", false, "") {
		t.Error("an identical rollup should be reported as current")
	}
	if ansibleBindingDetailsCurrent(nil, summary, 3, "t1", false, "") {
		t.Error("a binding with no status yet is never current")
	}
	if ansibleBindingDetailsCurrent(prior, summary, 4, "t1", false, "") {
		t.Error("a new generation is not current")
	}
	if ansibleBindingDetailsCurrent(prior, summary, 3, "t2", false, "") {
		t.Error("a new re-run trigger is not current")
	}
	if ansibleBindingDetailsCurrent(prior, BindingSummary{Total: 2, Succeeded: 1, Failed: 1}, 3, "t1", false, "") {
		t.Error("a changed rollup is not current")
	}

	// Legacy entries still present and due to be cleared is itself a
	// reason to write, or the migration never completes.
	withLegacy := &AnsibleBindingStatus{
		ObservedGeneration: 3, LastAppliedTrigger: "t1", Summary: &summary,
		VMs: []VMStatus{{Name: "web-1"}},
	}
	if ansibleBindingDetailsCurrent(withLegacy, summary, 3, "t1", true, "") {
		t.Error("a binding still carrying legacy vms[] is not current when they are due to be cleared")
	}
	if !ansibleBindingDetailsCurrent(withLegacy, summary, 3, "t1", false, "") {
		t.Error("legacy vms[] that are not yet due to be cleared do not force a write")
	}

	// A completed orphan scan has to reach status, or the scan repeats
	// on every pass and its AWX request stops being once per period.
	if ansibleBindingDetailsCurrent(prior, summary, 3, "t1", false, "2026-01-01T00:00:00Z") {
		t.Error("a fresh orphan scan timestamp is not current")
	}
}

func TestGenericStatusCurrent(t *testing.T) {
	obj := func(state, message string, ready bool) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{
				"state": state, "message": message, "ready": ready,
				"lastUpdated": "2026-09-04T10:00:00Z",
			},
		}}
	}
	want := map[string]interface{}{
		"state": "Ready", "message": "All 2 VM(s) completed the requested run successfully.",
		"ready": true, "lastUpdated": metav1.Now(),
	}

	if !genericStatusCurrent(obj("Ready", "All 2 VM(s) completed the requested run successfully.", true), want) {
		t.Error("status matching on everything but lastUpdated should be current")
	}
	if genericStatusCurrent(obj("Failed", "All 2 VM(s) completed the requested run successfully.", true), want) {
		t.Error("a changed state is not current")
	}
	if genericStatusCurrent(obj("Ready", "1 of 2 VM(s) failed their last run: web-1.", true), want) {
		t.Error("a changed message is not current")
	}
	if genericStatusCurrent(obj("Ready", "All 2 VM(s) completed the requested run successfully.", false), want) {
		t.Error("a changed ready flag is not current")
	}
	if genericStatusCurrent(&unstructured.Unstructured{Object: map[string]interface{}{}}, want) {
		t.Error("an object with no status yet is never current")
	}
}

func TestNameListBounded(t *testing.T) {
	got := nameList([]string{"a", "b", "c", "d", "e"})
	want := "a, b, c and 2 more"
	if got != want {
		t.Errorf("nameList = %q, want %q", got, want)
	}
	if got := nameList([]string{"a", "b"}); got != "a, b" {
		t.Errorf("nameList = %q, want %q", got, "a, b")
	}
}

func TestUpsertHistoryUpdatesInPlaceAndBounds(t *testing.T) {
	var h []VMRunHistoryEntry
	h = upsertHistory(h, VMRunHistoryEntry{JobID: 1, Status: "pending", StartedAt: "t0"})
	h = upsertHistory(h, VMRunHistoryEntry{JobID: 1, Status: "successful", FinishedAt: "t1"})
	if len(h) != 1 {
		t.Fatalf("one run should be one entry, got %d", len(h))
	}
	if h[0].Status != "successful" || h[0].StartedAt != "t0" || h[0].FinishedAt != "t1" {
		t.Errorf("entry not merged in place: %+v", h[0])
	}

	for i := 2; i <= historyLimit+5; i++ {
		h = upsertHistory(h, VMRunHistoryEntry{JobID: int64(i), Status: "successful"})
	}
	if len(h) != historyLimit {
		t.Errorf("history len = %d, want %d", len(h), historyLimit)
	}
	if h[0].JobID != int64(historyLimit+5) {
		t.Errorf("most recent run should be first, got job %d", h[0].JobID)
	}
}
