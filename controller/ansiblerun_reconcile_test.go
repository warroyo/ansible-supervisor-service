package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Drives whole AnsibleRun reconciles through the same fixture the
// binding lifecycle uses - real dynamic and AWX clients over stand-in
// servers - so what is asserted is the requests the controller actually
// made to AWX, not what a mock was told to expect.

// addRun registers a run with the fixture's API server and hands back the
// object as an informer would.
func (f *reconcileFixture) addRun(t *testing.T, name string, spec AnsibleRunSpec, status *AnsibleRunStatus) *unstructured.Unstructured {
	t.Helper()
	if spec.AWXConnectionRef == "" {
		spec.AWXConnectionRef = f.conn.Name
	}
	if spec.Template.Name == "" {
		spec.Template = TemplateRef{Name: "setup", Type: TemplateTypeJob}
	}
	u := fixtureObject(t, AnsibleRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", ResourceVersion: "1",
			CreationTimestamp: metav1.NewTime(time.Now())},
		Spec: &spec, Status: status,
	}, "AnsibleRun")
	f.runs[name] = u.DeepCopy()
	return u
}

// createdAgo backdates the API server's copy of a run and hands it back,
// so a deadline can be made to have already expired. It has to be the
// stored copy: a reconcile re-reads the run before acting on it, so
// anything set only on the caller's copy is not what it sees.
func (f *reconcileFixture) createdAgo(t *testing.T, name string, age time.Duration) *unstructured.Unstructured {
	t.Helper()
	stored := f.runs[name]
	if stored == nil {
		t.Fatalf("AnsibleRun %q is gone", name)
	}
	stored.Object["metadata"].(map[string]interface{})["creationTimestamp"] = metav1.NewTime(time.Now().Add(-age)).UTC().Format(time.RFC3339)
	return stored.DeepCopy()
}

// storedRun is the run as the API server now holds it, which is what the
// next pass would be handed.
func (f *reconcileFixture) storedRun(t *testing.T, name string) (*unstructured.Unstructured, AnsibleRunStatus) {
	t.Helper()
	stored := f.runs[name]
	if stored == nil {
		t.Fatalf("AnsibleRun %q is gone", name)
	}
	run, err := convertAnsibleRun(stored)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status == nil {
		return stored.DeepCopy(), AnsibleRunStatus{}
	}
	return stored.DeepCopy(), *run.Status
}

// awxEndpoint is the fingerprint the fixture's connection produces, so a
// status can be seeded as though it had launched against it - or, by
// using anything else, as though the connection had since been repointed.
func (f *reconcileFixture) awxEndpoint() string {
	return awxEndpointFingerprint(f.conn.Spec.URL, APIBasePathLegacy)
}

func hostTargetSpec() AnsibleRunSpec {
	return AnsibleRunSpec{Hosts: []RunHost{{Name: "db-1", Address: "192.0.2.9"}}}
}

func TestLostLaunchAnswerAdoptsTheJobItStartedRatherThanLaunchingAgain(t *testing.T) {
	// The window that produced two decommission runs from one AnsibleRun:
	// AWX creates the job and the answer never arrives, so nothing
	// records a job id and the next pass launches again.
	f := newReconcileFixture(t)
	f.launchStatus, f.launchGhost = http.StatusBadGateway, true
	u := f.addRun(t, "register", hostTargetSpec(), nil)

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err == nil {
		t.Fatal("a launch whose answer never came back is an error to retry")
	}
	_, status := f.storedRun(t, "register")
	if status.LaunchAttemptedAt == "" {
		t.Fatal("the attempt marker must survive an answer that may have been lost: clearing it is what relaunches")
	}
	if status.JobID != 0 {
		t.Fatalf("no job id came back, so none should be recorded, got %d", status.JobID)
	}

	// AWX is answering again, so a second pass could launch. It must not.
	f.launchStatus, f.launchGhost = 0, false
	next, _ := f.storedRun(t, "register")
	if _, err := applyAnsibleRun(context.Background(), f.client, next); err != nil {
		t.Fatalf("the job AWX did start should have been adopted: %v", err)
	}
	_, status = f.storedRun(t, "register")
	if status.JobID != 42 {
		t.Fatalf("expected the lost launch's own job to be adopted, got job %d", status.JobID)
	}
	if f.launches != 1 {
		t.Fatalf("one AnsibleRun must produce one AWX job, got %d launches", f.launches)
	}
	if status.JobURL == "" || status.StartedAt == "" {
		t.Errorf("an adopted job still has to be traceable: %+v", status)
	}
}

func TestLaunchAWXRefusedOutrightIsRetried(t *testing.T) {
	// The other half of the same decision: AWX answering 400 means it
	// made nothing, and a run stranded on that would never start at all.
	f := newReconcileFixture(t)
	f.launchStatus = http.StatusBadRequest
	u := f.addRun(t, "register", hostTargetSpec(), nil)

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err == nil {
		t.Fatal("a refused launch is an error")
	}
	_, status := f.storedRun(t, "register")
	if status.LaunchAttemptedAt != "" {
		t.Fatal("AWX refusing the request means nothing started, so the run must be free to launch again")
	}
	if status.FinishedAt != "" {
		t.Fatalf("a refused launch is retryable, not terminal: %+v", status)
	}

	f.launchStatus = 0
	next, _ := f.storedRun(t, "register")
	if _, err := applyAnsibleRun(context.Background(), f.client, next); err != nil {
		t.Fatalf("the retry should have launched: %v", err)
	}
	if _, status = f.storedRun(t, "register"); status.JobID == 0 {
		t.Fatal("expected the retry to record a job")
	}
}

func TestLostLaunchWaitsBeforeConcludingNothingStarted(t *testing.T) {
	// AWX writes the job row before it answers, but a controller that
	// looked a millisecond after the answer was lost and found nothing
	// would relaunch on the strength of it. Within the grace period the
	// answer is "not yet", not "no".
	f := newReconcileFixture(t)
	f.launchStatus = http.StatusBadGateway
	u := f.addRun(t, "register", hostTargetSpec(), nil)

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err == nil {
		t.Fatal("expected the failed launch to be reported")
	}
	next, _ := f.storedRun(t, "register")
	_, err := applyAnsibleRun(context.Background(), f.client, next)
	if err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("expected the pass to wait rather than relaunch, got %v", err)
	}
	if f.launches != 0 {
		t.Fatalf("nothing may launch while it is still unknown whether something already did, got %d", f.launches)
	}
	_, status := f.storedRun(t, "register")
	if status.LaunchAttemptedAt == "" {
		t.Fatal("the marker must still be there for the next pass to resolve")
	}
}

func TestRunCleanupWillNotTouchAnotherAWXInstance(t *testing.T) {
	// Host and job ids mean nothing on an instance that did not issue
	// them: deleting host 7 there deletes somebody else's host 7.
	f := newReconcileFixture(t)
	id := f.hosts.seed("db-1", runHostOwnerMarker("ns", "register"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		JobID: 42, JobStatus: "running", AWXEndpoint: "a-different-instance",
		Hosts: []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	result, err := cleanupAnsibleRun(context.Background(), f.client, u)
	if err != nil || !result.Done {
		t.Fatalf("there is nothing reachable left to clean up, so the run must release: %v %+v", err, result)
	}
	if f.hosts.deleted[id] {
		t.Error("deleted a host id on an instance that never issued it")
	}
	if f.canceled[42] {
		t.Error("canceled a job id on an instance that never issued it")
	}
}

func TestRepointedConnectionStopsARunRatherThanPollingTheWrongInstance(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		JobID: 42, JobStatus: "running", AWXEndpoint: "a-different-instance",
	})

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("a terminal outcome is recorded, not returned: %v", err)
	}
	_, status := f.storedRun(t, "register")
	if status.FinishedAt == "" {
		t.Fatal("a run whose ids no longer mean anything cannot make progress, so it has to end")
	}
	if !strings.Contains(status.FailureReason, "different AWX instance") {
		t.Errorf("the reason should say what happened, got %q", status.FailureReason)
	}
	for _, req := range f.awxRequests {
		if strings.Contains(req, "/jobs/42/") {
			t.Errorf("job 42 was polled on the wrong instance: %s", req)
		}
	}
}

func TestExpiredDeadlineCancelsTheJobItLeftRunning(t *testing.T) {
	// A Kubernetes Job terminates its pods when its deadline expires. A
	// run that only relabelled itself Failed would be reporting an end
	// state for a playbook still changing machines.
	f := newReconcileFixture(t)
	spec := hostTargetSpec()
	spec.ActiveDeadlineSeconds = 1
	f.addRun(t, "register", spec, &AnsibleRunStatus{JobID: 42, JobStatus: "running", AWXEndpoint: f.awxEndpoint()})
	u := f.createdAgo(t, "register", time.Hour)

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("expiry is recorded, not returned: %v", err)
	}
	if !f.canceled[42] {
		t.Fatal("the deadline expired with the AWX job left running")
	}
	_, status := f.storedRun(t, "register")
	if status.FinishedAt == "" {
		t.Fatal("an expired run has to reach an end state")
	}
	if !strings.Contains(status.FailureReason, "canceled") {
		t.Errorf("the reason should say what became of the job, got %q", status.FailureReason)
	}
}

func TestExpiredDeadlineSaysSoWhenTheJobCouldNotBeCanceled(t *testing.T) {
	// The deadline exists to reach an end state even when AWX is exactly
	// what cannot be reached, so a cancel that fails must not hold the
	// run open - but it must not be silent either.
	f := newReconcileFixture(t)
	f.conn.Spec.URL = "http://127.0.0.1:1"
	spec := hostTargetSpec()
	spec.ActiveDeadlineSeconds = 1
	f.addRun(t, "register", spec, &AnsibleRunStatus{JobID: 42, JobStatus: "running"})
	u := f.createdAgo(t, "register", time.Hour)

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("expiry is recorded, not returned: %v", err)
	}
	_, status := f.storedRun(t, "register")
	if status.FinishedAt == "" {
		t.Fatal("an unreachable AWX must not stop the deadline finishing the run")
	}
	if !strings.Contains(status.FailureReason, "may still be running") {
		t.Errorf("a human has to be told the job may have outlived the run, got %q", status.FailureReason)
	}
}

func TestDeletingARunCancelsItsJobAndThenItsHosts(t *testing.T) {
	f := newReconcileFixture(t)
	id := f.hosts.seed("db-1", runHostOwnerMarker("ns", "register"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		JobID: 42, JobStatus: "running", AWXEndpoint: f.awxEndpoint(),
		Hosts: []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	result, err := cleanupAnsibleRun(context.Background(), f.client, u)
	if err != nil || !result.Done {
		t.Fatalf("cleanup should have completed: %v %+v", err, result)
	}
	if !f.canceled[42] {
		t.Error("deleting a run left its AWX job running")
	}
	if !f.hosts.deleted[id] {
		t.Error("the inventory host this run created was left behind")
	}
	var cancelAt, deleteAt = -1, -1
	for i, req := range f.awxRequests {
		if strings.HasSuffix(req, "/cancel/") {
			cancelAt = i
		}
		if strings.HasPrefix(req, "DELETE") && strings.Contains(req, "/hosts/") {
			deleteAt = i
		}
	}
	if cancelAt == -1 || deleteAt == -1 || cancelAt > deleteAt {
		t.Errorf("the job has to stop before the host it is running against is deleted: %v", f.awxRequests)
	}
}

func TestRetainStillCancelsAJobLeftRunning(t *testing.T) {
	// cleanupPolicy governs inventory hosts. It does not mean "leave the
	// playbook running after the object asking for it is gone".
	f := newReconcileFixture(t)
	spec := hostTargetSpec()
	spec.CleanupPolicy = CleanupPolicyRetain
	id := f.hosts.seed("db-1", runHostOwnerMarker("ns", "register"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", spec, &AnsibleRunStatus{
		JobID: 42, JobStatus: "running", AWXEndpoint: f.awxEndpoint(),
		Hosts: []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	if result, err := cleanupAnsibleRun(context.Background(), f.client, u); err != nil || !result.Done {
		t.Fatalf("cleanup should have completed: %v %+v", err, result)
	}
	if !f.canceled[42] {
		t.Error("Retain kept the host, which is its job, and also left the job running, which is not")
	}
	if f.hosts.deleted[id] {
		t.Error("Retain must keep the inventory host")
	}
}

func TestFinishedRunIsNotCanceledOnTheWayOut(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		JobID: 42, JobStatus: "successful", FinishedAt: nowRFC3339(), AWXEndpoint: f.awxEndpoint(),
	})

	if result, err := cleanupAnsibleRun(context.Background(), f.client, u); err != nil || !result.Done {
		t.Fatalf("cleanup should have completed: %v %+v", err, result)
	}
	if f.canceled[42] {
		t.Error("a job that already finished must not be canceled")
	}
}

func TestTemporaryTemplateLookupFailureDoesNotFailTheRun(t *testing.T) {
	// An AWX answering 503 for a moment is an outage, not a spec error,
	// and the spec of a run cannot be edited to recover from one.
	f := newReconcileFixture(t)
	f.templateFail = true
	u := f.addRun(t, "register", hostTargetSpec(), nil)

	result, err := applyAnsibleRun(context.Background(), f.client, u)
	if err == nil {
		t.Fatal("a template lookup that failed is an error to retry")
	}
	_, status := f.storedRun(t, "register")
	if status.FinishedAt != "" || status.FailureReason != "" {
		t.Fatalf("a 503 must not end the run: %+v", status)
	}
	if state := updateAnsibleRunStatus(result.Object, false, err); state["state"] != "Pending" {
		t.Errorf("expected the run to still be Pending, got %v", state)
	}

	f.templateFail = false
	next, _ := f.storedRun(t, "register")
	if _, err := applyAnsibleRun(context.Background(), f.client, next); err != nil {
		t.Fatalf("the run should launch once AWX answers again: %v", err)
	}
	if _, status = f.storedRun(t, "register"); status.JobID == 0 {
		t.Fatal("expected the retry to launch")
	}
}

func TestMissingTemplateStillEndsTheRun(t *testing.T) {
	// The other side of the same classification: a template that is not
	// there is a spec error, and an immutable spec cannot be fixed, so
	// retrying it forever tells nobody anything.
	f := newReconcileFixture(t)
	f.templateMissing = true
	u := f.addRun(t, "register", hostTargetSpec(), nil)

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("a terminal outcome is recorded, not returned: %v", err)
	}
	_, status := f.storedRun(t, "register")
	if status.FinishedAt == "" {
		t.Fatalf("a template that does not exist must end the run: %+v", status)
	}
	if !strings.Contains(status.FailureReason, "not found") {
		t.Errorf("the reason should name what was missing, got %q", status.FailureReason)
	}
}

func TestStaleObjectCannotAuthorizeASecondLaunch(t *testing.T) {
	// The informer's copy is handed to every pass, and it can predate the
	// write that recorded a launch. Reconciling it as though it were
	// current is how one AnsibleRun produces two AWX jobs.
	f := newReconcileFixture(t)
	stale := f.addRun(t, "register", hostTargetSpec(), nil)

	if _, err := applyAnsibleRun(context.Background(), f.client, stale.DeepCopy()); err != nil {
		t.Fatalf("the first pass should have launched: %v", err)
	}
	if _, status := f.storedRun(t, "register"); status.JobID == 0 {
		t.Fatal("expected the first pass to record a job")
	}

	// Exactly the object the first pass was given, replayed.
	if _, err := applyAnsibleRun(context.Background(), f.client, stale); err != nil {
		t.Fatalf("the replayed pass should have polled the job it found: %v", err)
	}
	if f.launches != 1 {
		t.Fatalf("a stale read authorized another launch: %d launches", f.launches)
	}
}

func TestLaunchClaimLostToAWriteInBetweenDoesNotLaunch(t *testing.T) {
	// The claim is a conditional write for the same reason: if anything
	// landed on the run between this pass reading it and this pass
	// deciding to launch, the decision was made against a record that has
	// since moved, and forcing it through would overwrite whatever that
	// write recorded - a job id above all.
	f := newReconcileFixture(t)
	f.bumpRunRV = "register"
	u := f.addRun(t, "register", hostTargetSpec(), nil)

	_, err := applyAnsibleRun(context.Background(), f.client, u)
	if err == nil || !strings.Contains(err.Error(), "recording the launch attempt") {
		t.Fatalf("expected the claim to be refused, got %v", err)
	}
	if f.launches != 0 {
		t.Fatalf("nothing may launch on a claim that did not land, got %d launches", f.launches)
	}
	if _, status := f.storedRun(t, "register"); status.LaunchAttemptedAt != "" || status.FinishedAt != "" {
		t.Fatalf("a refused claim leaves the run free to try again: %+v", status)
	}
}

func TestRecoveryWillNotAdoptAJobOlderThanTheAttempt(t *testing.T) {
	// A job AWX created before this run's POST was sent cannot be this
	// run's. Adopting one reports another run's outcome here, and deleting
	// this run would then cancel a job it never started.
	f := newReconcileFixture(t)
	attempted := time.Now().Add(-2 * launchRecoveryGrace)
	f.templateJobs = []map[string]interface{}{{
		"id": 99, "created": attempted.Add(-time.Minute).UTC().Format(time.RFC3339),
		"limit": "db-1", "status": "running",
	}}
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		LaunchAttemptedAt: attempted.UTC().Format(time.RFC3339),
		AWXEndpoint:       f.awxEndpoint(),
		Hosts:             []RunHostStatus{{Name: "db-1", AWXHostID: 1, AWXInventoryID: 1, AWXHostCreated: true}},
	})

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("an unresolvable launch is recorded, not returned: %v", err)
	}
	_, status := f.storedRun(t, "register")
	if status.JobID != 0 {
		t.Fatalf("adopted job %d, which AWX created before this run asked for anything", status.JobID)
	}
	if status.FinishedAt == "" {
		t.Fatalf("a launch that cannot be accounted for has to end the run: %+v", status)
	}
	if f.launches != 0 {
		t.Fatalf("an unresolved launch must not be sent again, got %d launches", f.launches)
	}
}

func TestLostLaunchThatFoundNoJobEndsTheRunRatherThanSendingItAgain(t *testing.T) {
	// An empty job list is not proof that nothing ran: history can be
	// trimmed, the view can be restricted, and the page is one page. The
	// run ends as a failure a human can act on.
	f := newReconcileFixture(t)
	attempted := time.Now().Add(-2 * launchRecoveryGrace)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		LaunchAttemptedAt: attempted.UTC().Format(time.RFC3339),
		AWXEndpoint:       f.awxEndpoint(),
		Hosts:             []RunHostStatus{{Name: "db-1", AWXHostID: 1, AWXInventoryID: 1, AWXHostCreated: true}},
	})

	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("an unresolvable launch is recorded, not returned: %v", err)
	}
	if f.launches != 0 {
		t.Fatalf("a launch whose outcome is unknown must not be sent again, got %d launches", f.launches)
	}
	_, status := f.storedRun(t, "register")
	if status.FinishedAt == "" || !strings.Contains(status.FailureReason, "will not send it again") {
		t.Fatalf("the run has to end saying what a human should check: %+v", status)
	}
	if status.LaunchAttemptedAt == "" {
		t.Error("the attempt has to stay in status: it is what points at the job to look for")
	}
}

func TestRetryingAPartialHostFailureDoesNotDuplicateHostState(t *testing.T) {
	// status.hosts is what cleanup deletes and what the recovery limit is
	// rebuilt from, and the loop that fills it runs once per pass. Two
	// entries for db-1 make the limit "db-1,db-1", which matches no job
	// AWX ever ran.
	f := newReconcileFixture(t)
	f.failHostNamed = "db-2"
	spec := AnsibleRunSpec{Hosts: []RunHost{{Name: "db-1", Address: "192.0.2.9"}, {Name: "db-2", Address: "192.0.2.10"}}}
	u := f.addRun(t, "register", spec, nil)

	for pass := 1; pass <= 2; pass++ {
		if _, err := applyAnsibleRun(context.Background(), f.client, u); err == nil {
			t.Fatalf("pass %d: the second host failed, so the pass failed", pass)
		}
		next, status := f.storedRun(t, "register")
		if len(status.Hosts) != 1 || status.Hosts[0].Name != "db-1" {
			t.Fatalf("pass %d: expected one recorded host, got %+v", pass, status.Hosts)
		}
		if got := runLimit(status.Hosts); got != "db-1" {
			t.Fatalf("pass %d: the recovery limit no longer matches what would have launched: %q", pass, got)
		}
		u = next
	}

	f.failHostNamed = ""
	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("the pass that could reach both hosts should have launched: %v", err)
	}
	_, status := f.storedRun(t, "register")
	if len(status.Hosts) != 2 {
		t.Fatalf("expected both hosts recorded once each, got %+v", status.Hosts)
	}
	if f.launched[0].limit != "db-1,db-2" {
		t.Fatalf("launched with limit %q", f.launched[0].limit)
	}
}

func TestCleanupReadsLiveStateRatherThanACachedCopyWithNoStatus(t *testing.T) {
	// The engine hands finalization the informer's copy, which can predate
	// the write that recorded the job id and the host. Deciding from it
	// that there is nothing to clean up releases the run with its playbook
	// still running and its host still in the inventory.
	f := newReconcileFixture(t)
	id := f.hosts.seed("db-1", runHostOwnerMarker("ns", "register"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		JobID: 42, JobStatus: "running", AWXEndpoint: f.awxEndpoint(),
		Hosts: []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	stale := u.DeepCopy()
	delete(stale.Object, "status")

	result, err := cleanupAnsibleRun(context.Background(), f.client, stale)
	if err != nil || !result.Done {
		t.Fatalf("cleanup should have completed: %v %+v", err, result)
	}
	if !f.canceled[42] {
		t.Error("released the run with its AWX job still running")
	}
	if !f.hosts.deleted[id] {
		t.Error("released the run with the host it created still in the inventory")
	}
}

func TestCleanupWaitsForCancellationBeforeDeletingHosts(t *testing.T) {
	// AWX answers a cancel with 202 Accepted: it has taken the request,
	// not stopped the job. Deleting the inventory host that job is running
	// against on the strength of that answer races the playbook.
	f := newReconcileFixture(t)
	f.cancelHangs = true
	id := f.hosts.seed("db-1", runHostOwnerMarker("ns", "register"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		JobID: 42, JobStatus: "running", AWXEndpoint: f.awxEndpoint(),
		Hosts: []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	result, err := cleanupAnsibleRun(context.Background(), f.client, u)
	if err != nil {
		t.Fatalf("waiting for a cancel is not an error: %v", err)
	}
	if result.Done {
		t.Fatal("released the run before AWX said the job had stopped")
	}
	if result.RequeueAfter <= 0 {
		t.Error("a cleanup that is waiting has to ask to be looked at again")
	}
	if f.hosts.deleted[id] {
		t.Error("deleted the inventory host the job may still be running against")
	}
	if _, status := f.storedRun(t, "register"); status.CancelRequestedAt == "" {
		t.Error("the cancel request has to be recorded, or the next pass sends it again with nothing to bound the wait")
	}

	// AWX finishes stopping it.
	f.cancelHangs = false
	next, _ := f.storedRun(t, "register")
	result, err = cleanupAnsibleRun(context.Background(), f.client, next)
	if err != nil || !result.Done {
		t.Fatalf("the job has stopped, so cleanup should finish: %v %+v", err, result)
	}
	if !f.hosts.deleted[id] {
		t.Error("the host should go once the job it belonged to has stopped")
	}
}

func TestCleanupResolvesAnUnrecordedLaunchBeforeReleasing(t *testing.T) {
	// Deleting a run whose launch answer was lost must not release on the
	// strength of having no job id: the job may be running, and after the
	// finalizer comes off there is nothing left pointing at it.
	f := newReconcileFixture(t)
	attempted := time.Now().Add(-time.Minute)
	f.templateJobs = []map[string]interface{}{{
		"id": 77, "created": attempted.Add(time.Second).UTC().Format(time.RFC3339),
		"limit": "db-1", "status": "running",
	}}
	id := f.hosts.seed("db-1", runHostOwnerMarker("ns", "register"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		LaunchAttemptedAt: attempted.UTC().Format(time.RFC3339),
		AWXEndpoint:       f.awxEndpoint(),
		Hosts:             []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	result, err := cleanupAnsibleRun(context.Background(), f.client, u)
	if err != nil || !result.Done {
		t.Fatalf("cleanup should have completed: %v %+v", err, result)
	}
	if !f.canceled[77] {
		t.Error("the job the lost launch started was left running")
	}
	if _, status := f.storedRun(t, "register"); status.JobID != 77 {
		t.Errorf("the job it found should be recorded before the run goes, got %d", status.JobID)
	}
	if !f.hosts.deleted[id] {
		t.Error("the host this run created should still be cleaned up")
	}
}

func TestCleanupWillNotDeleteAHostThatIsNoLongerThisRunsHost(t *testing.T) {
	// The recorded id says this run created the host. AWX says what holds
	// that id now - a host rebuilt by hand, or one another owner has since
	// claimed, is not this run's to delete.
	f := newReconcileFixture(t)
	id := f.hosts.seed("db-1", hostOwnerMarker("ns", "somebody-else"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		AWXEndpoint: f.awxEndpoint(),
		Hosts:       []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	result, err := cleanupAnsibleRun(context.Background(), f.client, u)
	if err != nil || !result.Done {
		t.Fatalf("cleanup should have completed: %v %+v", err, result)
	}
	if f.hosts.deleted[id] {
		t.Error("deleted a host that now belongs to someone else")
	}
}

func TestCancelFailureKeepsBothTheHostsAndTheFinalizer(t *testing.T) {
	// A playbook that may still be running is the one case where deleting
	// the inventory host it runs against makes things worse, so a cancel
	// that failed stops the pass rather than carrying on to the hosts.
	f := newReconcileFixture(t)
	f.cancelStatus = http.StatusInternalServerError
	id := f.hosts.seed("db-1", runHostOwnerMarker("ns", "register"), `{"ansible_host":"192.0.2.9"}`)
	u := f.addRun(t, "register", hostTargetSpec(), &AnsibleRunStatus{
		JobID: 42, JobStatus: "running", AWXEndpoint: f.awxEndpoint(),
		Hosts: []RunHostStatus{{Name: "db-1", AWXHostID: int64(id), AWXInventoryID: 1, AWXHostCreated: true}},
	})

	result, err := cleanupAnsibleRun(context.Background(), f.client, u)
	if err == nil {
		t.Fatal("a cancel that failed has to hold the finalizer")
	}
	if result.Done {
		t.Error("cleanup reported itself finished with the job uncanceled")
	}
	if f.hosts.deleted[id] {
		t.Error("deleted the inventory host out from under a job that is still running")
	}
}

func TestRunTargetsABindingsHostWithoutTakingItOver(t *testing.T) {
	// The whole point of a one-off run against a managed VM: it executes
	// against the host the binding provisioned, and leaves the inventory
	// entry - variables, address, ownership - exactly as it found it.
	f := newReconcileFixture(t)
	bindingMarker := hostOwnerMarker("ns", "bind")
	vars := `{"ansible_host":"192.0.2.1","tier":"gold"}`
	id := f.hosts.seed("web-1", bindingMarker, vars)

	u := f.addRun(t, "smoke", AnsibleRunSpec{VMRef: &VMRef{Name: "web-1"}}, nil)
	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("a run against a binding's host should launch: %v", err)
	}

	if f.launches != 1 {
		t.Fatalf("expected one launch, got %d", f.launches)
	}
	if f.launched[0].limit != "web-1" {
		t.Errorf("expected the run scoped to the existing host, got limit %q", f.launched[0].limit)
	}
	if f.hosts.patched != 0 {
		t.Errorf("the run wrote to an inventory host it does not own (%d PATCHes)", f.hosts.patched)
	}
	if got := f.hosts.hosts[id]["variables"]; got != vars {
		t.Errorf("the binding's host variables changed: %v", got)
	}
	if got := f.hosts.hosts[id]["description"]; got != bindingMarker {
		t.Errorf("the run took over ownership: %v", got)
	}

	_, status := f.storedRun(t, "smoke")
	if len(status.Hosts) != 1 || status.Hosts[0].AWXHostID != int64(id) {
		t.Fatalf("expected the run to record the binding's own host id %d, got %+v", id, status.Hosts)
	}
	if status.Hosts[0].AWXHostCreated {
		t.Error("a borrowed host must not be recorded as this run's to delete")
	}

	// And deleting the run leaves it where it is.
	next, _ := f.storedRun(t, "smoke")
	if _, err := cleanupAnsibleRun(context.Background(), f.client, next); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if f.hosts.deleted[id] {
		t.Error("deleting the run deleted the binding's host")
	}
}

func TestRunRefusesToWriteOntoAHostItDoesNotOwn(t *testing.T) {
	// Silently dropping the address and variables would run the playbook
	// against an inventory entry the spec asked to change and AWX never
	// saw changed. Refusing says which way out there is.
	f := newReconcileFixture(t)
	vars := `{"ansible_host":"192.0.2.1"}`
	id := f.hosts.seed("db-1", hostOwnerMarker("ns", "bind"), vars)

	spec := AnsibleRunSpec{Hosts: []RunHost{{Name: "db-1", Address: "10.0.0.9"}}}
	u := f.addRun(t, "register", spec, nil)
	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("a terminal refusal is recorded, not returned: %v", err)
	}

	if f.launches != 0 {
		t.Fatalf("launched despite not being able to apply what the spec asked for, %d times", f.launches)
	}
	if f.hosts.patched != 0 {
		t.Error("wrote to the host anyway")
	}
	if got := f.hosts.hosts[id]["variables"]; got != vars {
		t.Errorf("the host's variables changed: %v", got)
	}
	_, status := f.storedRun(t, "register")
	if status.FinishedAt == "" || !strings.Contains(status.FailureReason, "spec.extraVars") {
		t.Fatalf("the run should end pointing at launch variables as the way to do this: %+v", status)
	}
}

func TestRunUsesAnUnmarkedHostWithoutChangingIt(t *testing.T) {
	// A host somebody maintains by hand is the same case as a binding's:
	// executing against a machine is not ownership of the record.
	f := newReconcileFixture(t)
	vars := `{"ansible_host":"10.20.5.11","backup_window":"02:00-04:00"}`
	id := f.hosts.seed("db-1", "", vars)

	u := f.addRun(t, "register", AnsibleRunSpec{Hosts: []RunHost{{Name: "db-1"}}}, nil)
	if _, err := applyAnsibleRun(context.Background(), f.client, u); err != nil {
		t.Fatalf("a run against a hand-made host should launch: %v", err)
	}
	if f.hosts.patched != 0 {
		t.Errorf("the run rewrote a host it did not create (%d PATCHes)", f.hosts.patched)
	}
	if got := f.hosts.hosts[id]["variables"]; got != vars {
		t.Errorf("hand-set variables changed: %v", got)
	}
	_, status := f.storedRun(t, "register")
	if len(status.Hosts) != 1 || status.Hosts[0].AWXHostCreated {
		t.Fatalf("an existing host is borrowed, never owned: %+v", status.Hosts)
	}
}
