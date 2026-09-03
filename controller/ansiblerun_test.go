package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// runWith builds the unstructured form of an AnsibleRun the way the API
// server hands one back, so status conversion is exercised for real
// rather than against a hand-built map.
func runWith(t *testing.T, status AnsibleRunStatus) *unstructured.Unstructured {
	t.Helper()
	ar := AnsibleRun{Status: &status}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ar)
	if err != nil {
		t.Fatalf("building unstructured run: %v", err)
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestUpdateAnsibleRunStatus(t *testing.T) {
	tests := []struct {
		name         string
		status       AnsibleRunStatus
		success      bool
		reconcileErr error
		wantState    string
		wantReady    bool
		wantMessage  string
	}{
		{
			name:      "nothing launched yet",
			success:   true,
			wantState: "Pending",
		},
		{
			name:      "job in flight",
			status:    AnsibleRunStatus{JobID: 7, JobStatus: "running"},
			success:   true,
			wantState: "Running",
		},
		{
			name:      "successful job is Ready",
			status:    AnsibleRunStatus{JobID: 7, JobStatus: "successful", FinishedAt: nowRFC3339()},
			success:   true,
			wantState: "Ready",
			wantReady: true,
		},
		{
			// A job AWX ran and failed is not a reconcile error - the
			// controller did its job - but the run is emphatically not Ready.
			name:      "failed job is not Ready",
			status:    AnsibleRunStatus{JobID: 7, JobStatus: "failed", FinishedAt: nowRFC3339()},
			success:   true,
			wantState: "Failed",
		},
		{
			// The reason has to survive later passes: once the run is
			// terminal, applyAnsibleRun returns nil and the engine calls
			// this with no error at all, so a message derived only from
			// reconcileErr would decay to something useless.
			name: "terminal reason survives a later clean pass",
			status: AnsibleRunStatus{
				JobStatus:     "unknown",
				FinishedAt:    nowRFC3339(),
				FailureReason: "spec.varsFrom[0] reads a Secret",
			},
			success:     true,
			wantState:   "Failed",
			wantMessage: "spec.varsFrom[0] reads a Secret",
		},
		{
			// A terminal outcome outranks a transient error on the same
			// pass, otherwise a cleanup blip could relabel a finished run.
			name:         "terminal outcome outranks a reconcile error",
			status:       AnsibleRunStatus{JobID: 7, JobStatus: "successful", FinishedAt: nowRFC3339()},
			reconcileErr: errors.New("AWX briefly unreachable"),
			wantState:    "Ready",
			wantReady:    true,
		},
		{
			// Failed is reserved for a terminal outcome. A run still being
			// retried is Pending, however loudly this pass errored -
			// otherwise the state means two different things, and the
			// common one is transient.
			name:         "retryable failure before any job is Pending, not Failed",
			reconcileErr: errors.New("AWX unreachable"),
			wantState:    "Pending",
			wantMessage:  "AWX unreachable",
		},
		{
			name:         "retryable failure while a job is in flight stays Running",
			status:       AnsibleRunStatus{JobID: 7, JobStatus: "running"},
			reconcileErr: errors.New("AWX unreachable"),
			wantState:    "Running",
			wantMessage:  "AWX unreachable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := updateAnsibleRunStatus(runWith(t, tc.status), tc.success, tc.reconcileErr)
			if got["state"] != tc.wantState {
				t.Errorf("state = %v, want %v (message: %v)", got["state"], tc.wantState, got["message"])
			}
			if got["ready"] != tc.wantReady {
				t.Errorf("ready = %v, want %v", got["ready"], tc.wantReady)
			}
			if tc.wantMessage != "" {
				msg, _ := got["message"].(string)
				if !strings.Contains(msg, tc.wantMessage) {
					t.Errorf("message = %q, want it to contain %q", msg, tc.wantMessage)
				}
			}
		})
	}
}

func TestTerminalErrorClassification(t *testing.T) {
	// The distinction decides whether a run retries forever or ends, so
	// wrapping has to survive the fmt.Errorf chain it passes through.
	if isTerminalError(errors.New("AWX unreachable")) {
		t.Error("a plain error must not be terminal: retryable failures have to keep retrying")
	}
	if !isTerminalError(terminalf("template %q not found", "nope")) {
		t.Error("terminalf must produce a terminal error")
	}
	wrapped := errors.Join(errors.New("context"), terminalf("bad spec"))
	if !isTerminalError(wrapped) {
		t.Error("terminal must survive wrapping, or a deep validation failure would retry forever")
	}
}

func TestDeadlineExceeded(t *testing.T) {
	runAge := func(age time.Duration, deadline int64) *AnsibleRun {
		return &AnsibleRun{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Now().Add(-age))},
			Spec:       &AnsibleRunSpec{ActiveDeadlineSeconds: deadline},
		}
	}

	if deadlineExceeded(runAge(time.Hour, 0)) {
		t.Error("no deadline set must never expire")
	}
	if deadlineExceeded(runAge(10*time.Second, 600)) {
		t.Error("a run inside its deadline must not expire")
	}
	if !deadlineExceeded(runAge(20*time.Minute, 600)) {
		t.Error("a run past its deadline must expire, or a wedged run never reaches a terminal state")
	}
	// A zero creation timestamp means the object was built by hand rather
	// than read from the API server; treating "now minus zero" as an
	// enormous age would fail such a run instantly.
	noCreation := &AnsibleRun{Spec: &AnsibleRunSpec{ActiveDeadlineSeconds: 1}}
	if deadlineExceeded(noCreation) {
		t.Error("a run with no creation timestamp must not be treated as expired")
	}
}

func TestRunStatusDetailsOmitsEngineOwnedFields(t *testing.T) {
	// state/message/ready/lastUpdated belong to the engine's field
	// manager. Writing them from this one too would make the two applies
	// fight, so the detail patch must not carry them.
	details := runStatusDetails(&AnsibleRunStatus{
		State: "Ready", Message: "done", Ready: true, LastUpdated: nowRFC3339(),
		JobID: 42, JobStatus: "successful",
	})
	if details.State != "" || details.Message != "" || details.Ready || details.LastUpdated != "" {
		t.Errorf("detail patch carries engine-owned fields: %+v", details)
	}
	if details.JobID != 42 || details.JobStatus != "successful" {
		t.Errorf("detail patch dropped fields it owns: %+v", details)
	}
}

func TestRunHostOwnerMarkerIsDistinctFromABinding(t *testing.T) {
	// An AnsibleRun and an AnsibleBinding can share a name in one
	// namespace. Identical markers would have each believe it owned - and
	// could delete - the other's inventory host.
	supervisorID = "sup-test"
	t.Cleanup(func() { supervisorID = "" })

	bind := hostOwnerMarker("ns", "same-name")
	run := runHostOwnerMarker("ns", "same-name")
	if bind == run {
		t.Fatalf("marker collision between kinds: %q", bind)
	}
	for _, m := range []string{bind, run} {
		if !strings.HasPrefix(m, hostMarkerPrefix) {
			t.Errorf("marker %q lost the shared prefix that identifies our hosts", m)
		}
	}
	// The binding's format is what is already stamped on hosts in the
	// field, so it must not have shifted.
	if bind != hostMarkerPrefix+"sup-test:ns/same-name" {
		t.Errorf("binding marker format changed to %q, which would orphan every existing host", bind)
	}
}
