package main

import (
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// bindingWith builds the unstructured form of an AnsibleBinding the way
// the API server hands one back, so status conversion is exercised for
// real rather than against a hand-built map.
func bindingWith(t *testing.T, vms ...VMStatus) *unstructured.Unstructured {
	t.Helper()
	ab := AnsibleBinding{Status: &AnsibleBindingStatus{VMs: vms}}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ab)
	if err != nil {
		t.Fatalf("building unstructured binding: %v", err)
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestUpdateAnsibleBindingStatus(t *testing.T) {
	tests := []struct {
		name      string
		vms       []VMStatus
		wantState string
		wantReady bool
	}{
		{
			name:      "no VMs matched yet",
			wantState: "Pending",
		},
		{
			// The bug this replaced: a job that ran and failed is not a
			// reconcile error, so the binding reported Ready.
			name:      "a failed run is not Ready",
			vms:       []VMStatus{{Name: "web-1", Phase: PhaseFailed}},
			wantState: "Failed",
		},
		{
			name:      "one failure among successes still fails the binding",
			vms:       []VMStatus{{Name: "web-1", Phase: PhaseSucceeded}, {Name: "web-2", Phase: PhaseFailed}},
			wantState: "Failed",
		},
		{
			name:      "an in-flight run is not Ready",
			vms:       []VMStatus{{Name: "web-1", Phase: PhaseSucceeded}, {Name: "web-2", Phase: PhaseRunning}},
			wantState: "Running",
		},
		{
			name:      "a VM waiting on an IP is not Ready",
			vms:       []VMStatus{{Name: "web-1", Phase: PhaseSucceeded}, {Name: "web-2", Phase: PhasePending}},
			wantState: "Pending",
		},
		{
			name:      "a VM with no phase at all is not Ready",
			vms:       []VMStatus{{Name: "web-1"}},
			wantState: "Pending",
		},
		{
			// Failure ranks above running: something needs attention now.
			name:      "failure outranks an in-flight run",
			vms:       []VMStatus{{Name: "web-1", Phase: PhaseFailed}, {Name: "web-2", Phase: PhaseRunning}},
			wantState: "Failed",
		},
		{
			name:      "every VM succeeded",
			vms:       []VMStatus{{Name: "web-1", Phase: PhaseSucceeded}, {Name: "web-2", Phase: PhaseSucceeded}},
			wantState: "Ready",
			wantReady: true,
		},
		{
			// Entries kept only to retry a host deletion are former
			// targets and must not hold the binding back.
			name: "pendingCleanup entries are not targets",
			vms: []VMStatus{
				{Name: "web-1", Phase: PhaseSucceeded},
				{Name: "gone-1", Phase: PhaseFailed, PendingCleanup: true},
			},
			wantState: "Ready",
			wantReady: true,
		},
		{
			name:      "only pendingCleanup entries left",
			vms:       []VMStatus{{Name: "gone-1", Phase: PhaseSucceeded, PendingCleanup: true}},
			wantState: "Pending",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := updateAnsibleBindingStatus(bindingWith(t, tc.vms...), true, nil)
			if got["state"] != tc.wantState {
				t.Errorf("state = %v, want %v (message: %v)", got["state"], tc.wantState, got["message"])
			}
			if got["ready"] != tc.wantReady {
				t.Errorf("ready = %v, want %v", got["ready"], tc.wantReady)
			}
		})
	}
}

func TestUpdateAnsibleBindingStatusReconcileError(t *testing.T) {
	// A reconcile error wins outright: even all-succeeded VMs describe
	// the last run, not the state of the world the controller just
	// failed to observe.
	got := updateAnsibleBindingStatus(
		bindingWith(t, VMStatus{Name: "web-1", Phase: PhaseSucceeded}),
		false, errors.New("AWX unreachable"),
	)
	if got["state"] != "Failed" || got["ready"] != false {
		t.Fatalf("got state=%v ready=%v, want Failed/false", got["state"], got["ready"])
	}
	if !strings.Contains(got["message"].(string), "AWX unreachable") {
		t.Errorf("message %q does not carry the underlying error", got["message"])
	}
}

// The whole point of holding LastUpdated steady is that an idle binding
// produces byte-identical status, so the apply can be skipped and the
// resource stops writing to etcd once per resync forever.
func TestAnsibleBindingDetailsCurrent(t *testing.T) {
	vms := []VMStatus{
		{Name: "web-1", Phase: PhaseSucceeded, LastJobID: 7, LastUpdated: "2026-09-04T10:00:00Z"},
		{Name: "web-2", Phase: PhaseSucceeded, LastJobID: 8, LastUpdated: "2026-09-04T10:00:00Z"},
	}
	prior := &AnsibleBindingStatus{ObservedGeneration: 3, LastAppliedTrigger: "t1", VMs: vms}

	if !ansibleBindingDetailsCurrent(prior, vms, 3, "t1") {
		t.Error("identical details should be reported as current")
	}
	if ansibleBindingDetailsCurrent(nil, vms, 3, "t1") {
		t.Error("a binding with no status yet is never current")
	}
	if ansibleBindingDetailsCurrent(prior, vms, 4, "t1") {
		t.Error("a new generation is not current")
	}
	if ansibleBindingDetailsCurrent(prior, vms, 3, "t2") {
		t.Error("a new re-run trigger is not current")
	}

	changed := []VMStatus{vms[0], {Name: "web-2", Phase: PhaseFailed, LastJobID: 8, LastUpdated: "2026-09-04T10:00:00Z"}}
	if ansibleBindingDetailsCurrent(prior, changed, 3, "t1") {
		t.Error("a VM whose phase changed is not current")
	}

	dropped := []VMStatus{vms[0]}
	if ansibleBindingDetailsCurrent(prior, dropped, 3, "t1") {
		t.Error("a VM that left the selector is not current")
	}
}

func TestVMStatusEqualIgnoresLastUpdated(t *testing.T) {
	a := VMStatus{Name: "web-1", Phase: PhaseRunning, LastUpdated: "2026-09-04T10:00:00Z"}
	b := VMStatus{Name: "web-1", Phase: PhaseRunning, LastUpdated: "2026-09-04T11:00:00Z"}
	if !vmStatusEqual(a, b) {
		t.Error("entries differing only in LastUpdated are equal - it records when the rest changed")
	}

	b.History = []VMRunHistoryEntry{{JobID: 1, Status: "successful"}}
	if vmStatusEqual(a, b) {
		t.Error("a new history entry is a real change")
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
