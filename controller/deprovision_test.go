package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

// Exercises the onDeleted hook through the same fixture the rest of the
// lifecycle tests use: real dynamic and AWX clients over stand-in
// servers, so what is asserted is the requests the controller actually
// made.

func hookRef() *DeprovisionHook {
	return &DeprovisionHook{Template: TemplateRef{Name: "decommission", Type: TemplateTypeJob}, TimeoutSeconds: 900}
}

// hookChild is a provisioned child with an onDeleted hook on its spec and
// an owned inventory host behind it.
func (f *reconcileFixture) hookChild(t *testing.T, hook *DeprovisionHook, mutate func(*AnsibleBindingVMStatus)) *unstructured.Unstructured {
	t.Helper()
	f.hosts.seed("web-1", hostOwnerMarker("ns", "bind"), `{"ansible_host":"192.0.2.1"}`)
	st := &AnsibleBindingVMStatus{
		AWXHostID: 1, AWXInventoryID: 1, AWXHostName: "web-1", AWXHostCreated: true,
		ObservedIP: "192.0.2.1", Phase: PhaseSucceeded,
	}
	if mutate != nil {
		mutate(st)
	}
	u := f.addChild(t, "web-1", st)
	if hook != nil {
		m, err := structToMap(hook)
		if err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedMap(u.Object, m, "spec", "onDeleted"); err != nil {
			t.Fatal(err)
		}
	}
	// The finalizer re-reads the child live, so the stored copy is the
	// one that matters.
	f.children[u.GetName()] = u.DeepCopy()
	return u
}

func (f *reconcileFixture) childStatus(t *testing.T, u *unstructured.Unstructured) AnsibleBindingVMStatus {
	t.Helper()
	stored := f.children[u.GetName()]
	if stored == nil {
		t.Fatalf("child %q is gone", u.GetName())
	}
	child, err := convertAnsibleBindingVM(stored)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status == nil {
		return AnsibleBindingVMStatus{}
	}
	return *child.Status
}

// deleteVM is what makes a teardown a deletion rather than a detach.
func (f *reconcileFixture) deleteVM() { f.vms = nil }

func TestDeprovisionHookFiresOnlyForAVMThatIsActuallyGone(t *testing.T) {
	for _, gone := range []bool{false, true} {
		name := "relabelled"
		if gone {
			name = "deleted"
		}
		t.Run(name, func(t *testing.T) {
			f := newReconcileFixture(t)
			u := f.hookChild(t, hookRef(), nil)
			if gone {
				f.deleteVM()
			}

			res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
			if err != nil {
				t.Fatal(err)
			}

			if !gone {
				// A VM that merely stopped matching is still running, so
				// a decommission playbook must not be aimed at it.
				if f.launches != 0 {
					t.Fatalf("hook ran against a live VM: %+v", f.launched)
				}
				if !res.Done || !f.hosts.deleted[1] {
					t.Fatalf("detach should finish immediately and remove the host: done=%v deleted=%v", res.Done, f.hosts.deleted)
				}
				return
			}
			if f.launches != 1 {
				t.Fatalf("expected one hook launch, got %d", f.launches)
			}
			if res.Done {
				t.Fatal("finalizer released while the hook job was still running")
			}
			if f.hosts.deleted[1] {
				t.Fatal("inventory host deleted out from under the running hook")
			}
			if got := f.launched[0].limit; got != "web-1" {
				t.Fatalf("hook launched with limit %q, so it would not have been scoped to this host", got)
			}
		})
	}
}

func TestDeprovisionHookResumesAcrossPassesRatherThanRelaunching(t *testing.T) {
	f := newReconcileFixture(t)
	f.parent = f.binding(t, CleanupPolicyDelete)
	u := f.hookChild(t, hookRef(), nil)
	f.deleteVM()
	f.jobStatusByID[42] = "running"

	for pass := 0; pass < 3; pass++ {
		res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if res.Done {
			t.Fatalf("pass %d released the finalizer while the job was running", pass)
		}
		if f.launches != 1 {
			t.Fatalf("pass %d relaunched the hook: %d launches", pass, f.launches)
		}
		if st := f.childStatus(t, u); st.Deprovision == nil || st.Deprovision.JobID != 42 {
			t.Fatalf("pass %d did not record the job to resume from: %+v", pass, st.Deprovision)
		}
	}

	f.jobStatusByID[42] = "successful"
	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("finalizer held after the hook succeeded")
	}
	if !f.hosts.deleted[1] {
		t.Fatal("inventory host survived a successful teardown")
	}
	st := f.childStatus(t, u)
	if st.Deprovision.Phase != PhaseSucceeded {
		t.Fatalf("phase %q, want %q", st.Deprovision.Phase, PhaseSucceeded)
	}
	if len(f.events) != 1 {
		t.Fatalf("expected one event, got %d", len(f.events))
	}
	involved, _, _ := unstructured.NestedString(f.events[0], "involvedObject", "kind")
	if involved != "AnsibleBinding" {
		t.Fatalf("event recorded against %q, so it dies with the child", involved)
	}
}

func TestDeprovisionHookReleasesOnFailureRatherThanHoldingTheNamespace(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*reconcileFixture, *AnsibleBindingVMStatus)
		wantPhase string
		wantEvent string
	}{
		{
			name: "job failed",
			setup: func(f *reconcileFixture, st *AnsibleBindingVMStatus) {
				st.Deprovision = &DeprovisionStatus{
					Phase: PhaseRunning, JobID: 42, JobType: TemplateTypeJob,
					StartedAt: nowRFC3339(), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				}
				f.jobStatusByID[42] = "failed"
			},
			wantPhase: PhaseFailed,
			wantEvent: "DeprovisionHookFailed",
		},
		{
			name: "deadline passed",
			setup: func(f *reconcileFixture, st *AnsibleBindingVMStatus) {
				st.Deprovision = &DeprovisionStatus{
					Phase: PhaseRunning, JobID: 42, JobType: TemplateTypeJob,
					StartedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
					Deadline:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
				}
				f.jobStatusByID[42] = "running"
			},
			wantPhase: PhaseTimedOut,
			wantEvent: "DeprovisionHookTimedOut",
		},
		{
			name: "launch outcome never recorded",
			setup: func(f *reconcileFixture, st *AnsibleBindingVMStatus) {
				st.Deprovision = &DeprovisionStatus{
					Phase: PhaseLaunching, StartedAt: nowRFC3339(),
					Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				}
			},
			wantPhase: PhaseFailed,
			wantEvent: "DeprovisionHookFailed",
		},
		{
			name: "template would run against the whole inventory",
			setup: func(f *reconcileFixture, st *AnsibleBindingVMStatus) {
				f.hookAskLimit = false
			},
			wantPhase: PhaseFailed,
			wantEvent: "DeprovisionHookFailed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			var u *unstructured.Unstructured
			u = f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) { tc.setup(f, st) })
			f.deleteVM()

			res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Done {
				t.Fatal("a hook that cannot finish must still release the finalizer")
			}
			if !f.hosts.deleted[1] {
				t.Fatal("inventory host leaked after a failed hook")
			}
			st := f.childStatus(t, u)
			if st.Deprovision == nil || st.Deprovision.Phase != tc.wantPhase {
				t.Fatalf("phase %+v, want %q", st.Deprovision, tc.wantPhase)
			}
			if st.Deprovision.Message == "" {
				t.Fatal("no message recorded, so nothing says why")
			}
			if len(f.events) != 1 {
				t.Fatalf("expected one event, got %d", len(f.events))
			}
			reason, _, _ := unstructured.NestedString(f.events[0], "reason")
			kind, _, _ := unstructured.NestedString(f.events[0], "type")
			if reason != tc.wantEvent || kind != eventWarning {
				t.Fatalf("event %q/%q, want %q/%q", kind, reason, eventWarning, tc.wantEvent)
			}
		})
	}
}

func TestDeprovisionHookNeverRelaunchesAfterALostLaunch(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) {
		st.Deprovision = &DeprovisionStatus{
			Phase: PhaseLaunching, StartedAt: nowRFC3339(),
			Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}
	})
	f.deleteVM()

	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	if f.launches != 0 {
		t.Fatal("a decommission playbook whose first launch was unaccounted for was launched again")
	}
}

func TestDeprovisionHookWaitsForAnInFlightProvisioningJob(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) {
		st.LastJobID, st.LastJobStatus, st.LastJobType = 42, "running", TemplateTypeJob
		conn := *f.conn.Spec
		st.LastJobConnection = &conn
		st.Phase = PhaseRunning
	})
	f.deleteVM()
	f.jobStatusByID[42] = "running"

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done || f.launches != 0 {
		t.Fatalf("hook launched against a host a provisioning job is still configuring: done=%v launches=%d", res.Done, f.launches)
	}
	if f.hosts.deleted[1] {
		t.Fatal("host deleted while a provisioning job was still running")
	}

	f.jobStatusByID[42] = "successful"
	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	if f.launches != 1 {
		t.Fatalf("hook did not launch once the provisioning job finished: %d launches", f.launches)
	}
}

func TestDeprovisionHookPinsTheHostToTheControlNodeAndCarriesContext(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.hookChild(t, hookRef(), nil)
	f.deleteVM()

	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	vars, _ := f.hosts.hosts[1]["variables"].(string)
	if !strings.Contains(vars, `"ansible_connection":"local"`) {
		t.Fatalf("host was not pinned to the control node before the hook ran: %s", vars)
	}
	if f.hosts.patched != 1 {
		t.Fatalf("expected exactly one host patch, got %d", f.hosts.patched)
	}
	got := f.launched[0].extraVars
	for k, want := range map[string]string{
		"asb_hook": "onDeleted", "asb_vm_name": "web-1", "asb_binding": "bind",
		"asb_last_known_ip": "192.0.2.1", "asb_vm_uid": "vm-1",
	} {
		if got[k] != want {
			t.Errorf("extra var %s = %v, want %q", k, got[k], want)
		}
	}
}

func TestDeprovisionHookDropsVariablesATemplateWillNotAccept(t *testing.T) {
	f := newReconcileFixture(t)
	f.hookAskVars = false
	u := f.hookChild(t, hookRef(), nil)
	f.deleteVM()

	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	if f.launches != 1 {
		t.Fatalf("expected the hook to launch anyway, got %d launches", f.launches)
	}
	if len(f.launched[0].extraVars) != 0 {
		t.Fatalf("variables sent to a template that would have dropped them: %v", f.launched[0].extraVars)
	}
	if f.launched[0].limit != "web-1" {
		t.Fatal("the limit is a safety property and must still be sent")
	}
	st := f.childStatus(t, u)
	if st.Deprovision == nil || st.Deprovision.Message == "" {
		t.Fatal("nothing recorded to say the variables were dropped")
	}
}

func TestRetainRunsTheHookButKeepsTheInventoryHost(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.hookChild(t, hookRef(), nil)
	if err := unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
		t.Fatal(err)
	}
	f.children[u.GetName()] = u.DeepCopy()
	f.deleteVM()
	f.jobStatusByID[42] = "successful"

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if f.launches != 1 {
		t.Fatalf("Retain suppressed the deregistration playbook: %d launches", f.launches)
	}
	if res.Done {
		t.Fatal("released while the hook was still running")
	}
	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	if f.hosts.deleted[1] {
		t.Fatal("Retain deleted the inventory host")
	}
}

// The five below are regression tests for a review of this feature.

func TestDeprovisionHookLeavesAnotherBindingsHostAlone(t *testing.T) {
	// Provisioning refuses a host another binding owns, so status may
	// never have recorded one: cover both the child that got as far as
	// recording its host and the one that did not.
	for _, name := range []string{"with recorded status", "without recorded status"} {
		t.Run(name, func(t *testing.T) {
			f := newReconcileFixture(t)
			var u *unstructured.Unstructured
			if name == "with recorded status" {
				u = f.hookChild(t, hookRef(), nil)
			} else {
				f.hosts.seed("web-1", hostOwnerMarker("ns", "other"), "{}")
				u = f.addChild(t, "web-1", nil)
				m, err := structToMap(hookRef())
				if err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedMap(u.Object, m, "spec", "onDeleted"); err != nil {
					t.Fatal(err)
				}
				f.children[u.GetName()] = u.DeepCopy()
			}
			f.hosts.hosts[1]["description"] = hostOwnerMarker("ns", "other")
			f.deleteVM()

			res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Done {
				t.Fatal("held the finalizer over a host that is not ours")
			}
			if f.launches != 0 {
				t.Fatalf("ran a decommission playbook against another binding's host: %+v", f.launched)
			}
			if f.hosts.patched != 0 {
				t.Fatal("modified another binding's host")
			}
			if f.hosts.deleted[1] {
				t.Fatal("deleted another binding's host")
			}
			st := f.childStatus(t, u)
			if st.Deprovision == nil || st.Deprovision.Phase != PhaseSkipped || !strings.Contains(st.Deprovision.Message, "another") {
				t.Fatalf("nothing recorded to say why: %+v", st.Deprovision)
			}
		})
	}
}

func TestDeprovisionHookRefusesATemplateInAnotherInventory(t *testing.T) {
	for _, inventory := range []int{999, -1} {
		name := "different inventory"
		if inventory < 0 {
			name = "no inventory of its own"
		}
		t.Run(name, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.hookInventory = inventory
			u := f.hookChild(t, hookRef(), nil)
			f.deleteVM()

			res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
			if err != nil {
				t.Fatal(err)
			}
			if f.launches != 0 {
				// A limit only selects within the job's own inventory, so
				// this would have hit an unrelated host or nothing at all.
				t.Fatalf("launched into the wrong inventory: %+v", f.launched)
			}
			if f.hosts.patched != 0 {
				t.Fatal("modified the host before working out it could not run")
			}
			if !res.Done || !f.hosts.deleted[1] {
				t.Fatalf("teardown did not finish: done=%v deleted=%v", res.Done, f.hosts.deleted)
			}
			st := f.childStatus(t, u)
			if st.Deprovision == nil || st.Deprovision.Phase != PhaseFailed || !strings.Contains(st.Deprovision.Message, "inventory") {
				t.Fatalf("phase/message do not explain the refusal: %+v", st.Deprovision)
			}
		})
	}
}

func TestDeprovisionHookTakesItsConnectionOverrideBackOffASurvivingHost(t *testing.T) {
	cases := []struct {
		name  string
		vars  string
		after string
	}{
		{name: "no prior value", vars: `{"ansible_host":"192.0.2.1"}`, after: ""},
		{name: "explicit prior value", vars: `{"ansible_host":"192.0.2.1","ansible_connection":"ssh"}`, after: "ssh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			u := f.hookChild(t, hookRef(), nil)
			f.hosts.hosts[1]["variables"] = tc.vars
			// Retain keeps the host, so whatever the hook does to it is
			// what the next provisioning run will find.
			if err := unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
				t.Fatal(err)
			}
			f.children[u.GetName()] = u.DeepCopy()
			f.deleteVM()

			if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
				t.Fatal(err)
			}
			if vars, _ := f.hosts.hosts[1]["variables"].(string); !strings.Contains(vars, `"ansible_connection":"local"`) {
				t.Fatalf("the hook did not pin the host while it ran: %s", vars)
			}

			f.jobStatusByID[42] = "successful"
			res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Done || f.hosts.deleted[1] {
				t.Fatalf("Retain did not keep the host: done=%v deleted=%v", res.Done, f.hosts.deleted)
			}
			vars, _ := f.hosts.hosts[1]["variables"].(string)
			var got map[string]interface{}
			if err := json.Unmarshal([]byte(vars), &got); err != nil {
				t.Fatal(err)
			}
			if tc.after == "" {
				if _, present := got["ansible_connection"]; present {
					t.Fatalf("the hook's override outlived it and would send the next run to the control node: %s", vars)
				}
			} else if got["ansible_connection"] != tc.after {
				t.Fatalf("ansible_connection = %v, want %q restored", got["ansible_connection"], tc.after)
			}
			if got["ansible_host"] != "192.0.2.1" {
				t.Fatalf("restoring clobbered the other variables: %s", vars)
			}
		})
	}
}

func TestDeprovisionHookDeadlineSurvivesRepeatedPreparationFailures(t *testing.T) {
	f := newReconcileFixture(t)
	f.hookTemplateFail = true
	u := f.hookChild(t, hookRef(), nil)
	f.deleteVM()

	for pass := 0; pass < 3; pass++ {
		if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err == nil {
			t.Fatalf("pass %d: expected the template lookup to fail", pass)
		}
	}
	st := f.childStatus(t, u)
	if st.Deprovision == nil || st.Deprovision.Deadline == "" {
		t.Fatalf("no deadline persisted, so every retry restarts the clock: %+v", st.Deprovision)
	}
	deadline := st.Deprovision.Deadline

	// Once it passes, the hook has to give up rather than hold the
	// finalizer on an AWX that will never answer.
	stored := f.children[u.GetName()]
	if err := unstructured.SetNestedField(stored.Object, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "status", "deprovision", "deadline"); err != nil {
		t.Fatal(err)
	}
	if deadline == "" {
		t.Fatal("unreachable")
	}

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("an expired hook still held the finalizer")
	}
	if st := f.childStatus(t, u); st.Deprovision.Phase != PhaseTimedOut {
		t.Fatalf("phase %q, want %q", st.Deprovision.Phase, PhaseTimedOut)
	}
}

func TestDeprovisionHookDoesNotReportSuccessWhenAWXNarrowedTheLaunch(t *testing.T) {
	f := newReconcileFixture(t)
	f.ignoreLimit = true
	u := f.hookChild(t, hookRef(), nil)
	f.deleteVM()

	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	st := f.childStatus(t, u)
	if st.Deprovision == nil || st.Deprovision.JobID != 42 {
		t.Fatalf("the job was not recorded, so it cannot be traced: %+v", st.Deprovision)
	}
	if st.Deprovision.LaunchError == "" {
		t.Fatal("AWX dropped the limit and nothing recorded it")
	}

	// The job itself runs fine - against the wrong scope.
	f.jobStatusByID[42] = "successful"
	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	st = f.childStatus(t, u)
	if st.Deprovision.Phase != PhaseFailed {
		t.Fatalf("a launch AWX narrowed reported %q", st.Deprovision.Phase)
	}
	if len(f.events) != 1 {
		t.Fatalf("expected one event, got %d", len(f.events))
	}
	kind, _, _ := unstructured.NestedString(f.events[0], "type")
	if kind != eventWarning {
		t.Fatalf("event type %q, want %q", kind, eventWarning)
	}
}

// The three below are regression tests for the second review, all of
// them recovery paths: a restart mid-pin, a policy change mid-hook, and
// an expired hook whose AWX is unreachable.

func TestDeprovisionHookKeepsItsFirstConnectionSnapshotAcrossARestart(t *testing.T) {
	f := newReconcileFixture(t)
	// The durable state a controller leaves if it dies between pinning
	// the host and recording the launch: the snapshot is saved, and the
	// host already says local.
	u := f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) {
		prior := "ssh"
		st.Deprovision = &DeprovisionStatus{
			Phase: PhasePending, StartedAt: nowRFC3339(),
			Deadline:        time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			HostPinned:      true,
			PriorConnection: &prior,
		}
	})
	f.hosts.hosts[1]["variables"] = `{"ansible_host":"192.0.2.1","ansible_connection":"local"}`
	if err := unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
		t.Fatal(err)
	}
	f.children[u.GetName()] = u.DeepCopy()
	f.deleteVM()

	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	if st := f.childStatus(t, u); st.Deprovision.PriorConnection == nil || *st.Deprovision.PriorConnection != "ssh" {
		t.Fatalf("the restart overwrote the saved connection with the pin itself: %+v", st.Deprovision.PriorConnection)
	}

	f.jobStatusByID[42] = "successful"
	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	var vars map[string]interface{}
	raw, _ := f.hosts.hosts[1]["variables"].(string)
	if err := json.Unmarshal([]byte(raw), &vars); err != nil {
		t.Fatal(err)
	}
	if vars["ansible_connection"] != "ssh" {
		t.Fatalf("ansible_connection = %v, want the original %q back", vars["ansible_connection"], "ssh")
	}
}

func TestDeprovisionHookUndoesItsPinWhenThePolicyBecomesRetainMidHook(t *testing.T) {
	for _, prior := range []string{"", "ssh"} {
		name := "no prior value"
		if prior != "" {
			name = "explicit prior value"
		}
		t.Run(name, func(t *testing.T) {
			f := newReconcileFixture(t)
			u := f.hookChild(t, hookRef(), nil)
			if prior != "" {
				f.hosts.hosts[1]["variables"] = `{"ansible_host":"192.0.2.1","ansible_connection":"` + prior + `"}`
			}
			f.deleteVM()

			// Launches under Delete, so at launch time the host was not
			// expected to survive at all.
			if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
				t.Fatal(err)
			}
			if f.launches != 1 {
				t.Fatalf("expected the hook to launch, got %d", f.launches)
			}

			// The parent copies a policy change down into a terminating
			// child on purpose, so this can happen mid-hook.
			stored := f.children[u.GetName()]
			if err := unstructured.SetNestedField(stored.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
				t.Fatal(err)
			}
			f.jobStatusByID[42] = "successful"

			res, err := cleanupAnsibleBindingVM(context.Background(), f.client, stored)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Done || f.hosts.deleted[1] {
				t.Fatalf("Retain did not take effect: done=%v deleted=%v", res.Done, f.hosts.deleted)
			}
			var vars map[string]interface{}
			raw, _ := f.hosts.hosts[1]["variables"].(string)
			if err := json.Unmarshal([]byte(raw), &vars); err != nil {
				t.Fatal(err)
			}
			if prior == "" {
				if _, present := vars["ansible_connection"]; present {
					t.Fatalf("the override outlived a hook that ended under Retain: %s", raw)
				}
			} else if vars["ansible_connection"] != prior {
				t.Fatalf("ansible_connection = %v, want %q", vars["ansible_connection"], prior)
			}
		})
	}
}

func TestExpiredHookFinishesUnderRetainEvenWhenAWXCannotBeReached(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) {
		st.Deprovision = &DeprovisionStatus{
			Phase: PhaseRunning, JobID: 42, JobType: TemplateTypeJob,
			StartedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
			Deadline:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		}
	})
	// Nothing to delete, and nothing pinned to undo - so nothing here is
	// worth holding the object open for.
	if err := unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
		t.Fatal(err)
	}
	f.children[u.GetName()] = u.DeepCopy()
	f.deleteVM()
	f.failHostSync = true

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatalf("an expired hook was held open by an AWX lookup it did not need: %v", err)
	}
	if !res.Done {
		t.Fatal("an expired hook under Retain did not finish")
	}
	if st := f.childStatus(t, u); st.Deprovision.Phase != PhaseTimedOut {
		t.Fatalf("phase %q, want %q", st.Deprovision.Phase, PhaseTimedOut)
	}
	if f.hosts.deleted[1] {
		t.Fatal("Retain deleted the host")
	}
}

// warmCaches puts the AWXConnection in the informer store the way the
// running controller does, so a measurement counts what a warm process
// actually spends rather than a cold one's credential reads.
func (f *reconcileFixture) warmCaches(t *testing.T) {
	t.Helper()
	conn, err := structToMap(f.conn)
	if err != nil {
		t.Fatal(err)
	}
	conn["apiVersion"], conn["kind"] = awxConnGVR.GroupVersion().String(), "AWXConnection"
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := store.Add(&unstructured.Unstructured{Object: conn}); err != nil {
		t.Fatal(err)
	}
	awxConnStore = store
	t.Cleanup(func() { awxConnStore = nil })
}

func (f *reconcileFixture) counters() (reads, writes, awx int) {
	for _, r := range f.requests {
		if strings.HasPrefix(r, "GET ") {
			reads++
		} else {
			writes++
		}
	}
	return reads, writes, len(f.awxRequests)
}

// runningHook is a child whose hook has launched and is still going.
func (f *reconcileFixture) runningHook(t *testing.T, endpoint string) *unstructured.Unstructured {
	t.Helper()
	u := f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) {
		st.AWXEndpoint = awxEndpointFingerprint(f.conn.Spec.URL, APIBasePathLegacy)
		st.Deprovision = &DeprovisionStatus{
			Phase: PhaseRunning, JobID: 42, JobType: TemplateTypeJob, JobStatus: "running",
			Endpoint:  endpoint,
			StartedAt: nowRFC3339(),
			Deadline:  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}
	})
	f.deleteVM()
	f.jobStatusByID[42] = "running"
	return u
}

func TestRunningHookPollCostsOneReadAndOneAWXRequest(t *testing.T) {
	f := newReconcileFixture(t)
	f.warmCaches(t)
	endpoint := awxEndpointFingerprint(f.conn.Spec.URL, APIBasePathLegacy)
	u := f.runningHook(t, endpoint)

	// First pass warms the AWX client cache the way a running process
	// would already have it.
	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	f.requests, f.awxRequests = nil, nil

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("released while the job was still running")
	}
	reads, writes, awx := f.counters()
	if reads != 1 || writes != 0 || awx != 1 {
		t.Fatalf("an unchanged poll cost %d reads, %d writes, %d AWX requests; want 1/0/1\nkubernetes: %v\nawx: %v",
			reads, writes, awx, f.requests, f.awxRequests)
	}
	// Specifically: no host lookup and no VirtualMachine read.
	for _, r := range f.awxRequests {
		if strings.Contains(r, "/inventories/") || strings.Contains(r, "/hosts/") {
			t.Fatalf("the poll re-discovered the host: %v", f.awxRequests)
		}
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/virtualmachines") {
			t.Fatalf("the poll re-read the VirtualMachine: %v", f.requests)
		}
	}
}

func TestRunningHookPollWillNotFollowAJobToAnotherInstance(t *testing.T) {
	f := newReconcileFixture(t)
	f.warmCaches(t)
	// The launch happened on a different AWX; the connection has since
	// been repointed at this one, where job 42 is somebody else's.
	u := f.runningHook(t, "sha256:not-this-instance")

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("a hook that cannot be followed still held the finalizer")
	}
	for _, r := range f.awxRequests {
		if strings.Contains(r, "/jobs/") {
			t.Fatalf("polled a job number on an instance that did not issue it: %v", f.awxRequests)
		}
	}
	st := f.childStatus(t, u)
	if st.Deprovision.Phase != PhaseFailed || !strings.Contains(st.Deprovision.Message, "repointed") {
		t.Fatalf("outcome does not explain what happened: %+v", st.Deprovision)
	}
}

func TestHookWithNoRecordedEndpointTakesTheLongPath(t *testing.T) {
	// status.awxEndpoint is only written by a provisioning pass, so it
	// can be empty; the launch's own fingerprint is what makes the fast
	// path safe. Without one, the pass must not take the shortcut.
	f := newReconcileFixture(t)
	f.warmCaches(t)
	u := f.runningHook(t, "")

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("released while the job was still running")
	}
	var lookedUpHost bool
	for _, r := range f.awxRequests {
		if strings.Contains(r, "/inventories/") {
			lookedUpHost = true
		}
	}
	if !lookedUpHost {
		t.Fatalf("took the fast path without a recorded launch endpoint: %v", f.awxRequests)
	}
}

func TestHookRequeueIsJitteredAndNeverOutlivesTheDeadline(t *testing.T) {
	far := &DeprovisionStatus{Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		got := hookRequeue(far)
		if got < hookPollInterval || got >= hookPollInterval+hookPollJitter {
			t.Fatalf("requeue %s outside [%s, %s)", got, hookPollInterval, hookPollInterval+hookPollJitter)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("every requeue was identical, so a bulk delete still polls in lockstep")
	}

	near := &DeprovisionStatus{Deadline: time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)}
	if got := hookRequeue(near); got > 2*time.Second {
		t.Fatalf("requeue %s reaches past the deadline, so the hook would expire before being looked at", got)
	}
	past := &DeprovisionStatus{Deadline: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	if got := hookRequeue(past); got < time.Second {
		t.Fatalf("requeue %s is not a sane delay for an already-expired hook", got)
	}
}

// warmChildCache puts the given children in the informer store the
// parent reads, indexed the way the running controller indexes them.
func (f *reconcileFixture) warmChildCache(t *testing.T, children ...*unstructured.Unstructured) {
	t.Helper()
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		childrenByBindingIndex: childrenByBindingIndexFunc,
	})
	for _, child := range children {
		if err := store.Add(child.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}
	ansBindVMStore = store
	t.Cleanup(func() { ansBindVMStore = nil })
}

func (f *reconcileFixture) childListRequests() int {
	var n int
	for _, r := range f.requests {
		if strings.HasPrefix(r, "GET ") && strings.HasSuffix(r, "/ansiblebindingvms") {
			n++
		}
	}
	return n
}

func TestTerminatingBindingWaitsFromTheCache(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.addChild(t, "web-1", &AnsibleBindingVMStatus{AWXHostID: 1, AWXHostCreated: true})
	now := metav1.Now()
	f.children[u.GetName()].SetDeletionTimestamp(&now)
	f.warmChildCache(t, f.children[u.GetName()])

	f.requests = nil
	res, err := cleanupAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete))
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("released while a child was still finalizing")
	}
	if got := f.childListRequests(); got != 0 {
		t.Fatalf("a waiting pass issued %d live child LIST(s): %v", got, f.requests)
	}
	if res.RequeueAfter != childCleanupPollInterval {
		t.Fatalf("requeue %s, want %s", res.RequeueAfter, childCleanupPollInterval)
	}
}

func TestTerminatingBindingConfirmsLiveBeforeReleasing(t *testing.T) {
	f := newReconcileFixture(t)
	// The child exists in the API server but the cache has not seen it -
	// a child created moments ago, or an informer still catching up.
	// Releasing here would abandon it, its AWX host and its hook.
	u := f.addChild(t, "web-1", &AnsibleBindingVMStatus{AWXHostID: 1, AWXHostCreated: true})
	f.warmChildCache(t)

	f.requests = nil
	res, err := cleanupAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete))
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("released a binding on the word of an empty cache")
	}
	if f.childListRequests() == 0 {
		t.Fatal("released without confirming against the API server")
	}
	// And it acted on what it found, rather than only counting it.
	if f.children[u.GetName()].GetDeletionTimestamp() == nil {
		t.Fatal("the child the live list found was not deleted")
	}
}

func TestTerminatingBindingReleasesWhenBothAgree(t *testing.T) {
	f := newReconcileFixture(t)
	f.warmChildCache(t)

	res, err := cleanupAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("a binding with no children left did not release")
	}
	if f.childListRequests() != 1 {
		t.Fatalf("expected exactly one confirming live LIST, got %d: %v", f.childListRequests(), f.requests)
	}
}

// The three hook failures below are independent of which host a hook
// targets: each one is a way the teardown acted on state that was not
// the state it recorded.

func TestRepointedConnectionStopsHostCleanupAndNotOnlyPolling(t *testing.T) {
	f := newReconcileFixture(t)
	// A child whose host was rediscovered rather than remembered has no
	// status.awxEndpoint, so the hook's own fingerprint is the only
	// record of which instance this teardown belongs to.
	u := f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) {
		st.AWXEndpoint = ""
		st.Deprovision = &DeprovisionStatus{
			Phase: PhaseRunning, JobID: 42, JobType: TemplateTypeJob, JobStatus: "running",
			Endpoint:  "https://awx.elsewhere.example|" + APIBasePathLegacy,
			StartedAt: nowRFC3339(),
			Deadline:  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}
	})
	f.deleteVM()

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("a hook that cannot be followed to a conclusion held the finalizer")
	}
	if f.hosts.deleted[1] {
		t.Fatal("deleted a host on the instance the connection now points at, which never issued the id this object recorded")
	}
	if f.hosts.patched != 0 {
		t.Fatalf("wrote to %d host(s) on an instance this object has no state on", f.hosts.patched)
	}
	st := f.childStatus(t, u)
	if st.Deprovision.Phase != PhaseFailed {
		t.Fatalf("phase %q, want %q: the job was launched somewhere and its outcome is unknown", st.Deprovision.Phase, PhaseFailed)
	}
	if !strings.Contains(st.Deprovision.Message, "different AWX instance") {
		t.Fatalf("outcome does not say why: %q", st.Deprovision.Message)
	}
}

func TestTransientHostLookupFailureDoesNotReleaseAnUnstartedHook(t *testing.T) {
	t.Run("deleted VM keeps the hook owed", func(t *testing.T) {
		f := newReconcileFixture(t)
		u := f.hookChild(t, hookRef(), nil)
		if err := unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
			t.Fatal(err)
		}
		f.children[u.GetName()] = u.DeepCopy()
		f.deleteVM()
		f.failHostSync = true

		if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err == nil {
			t.Fatal("released the finalizer on one failed host lookup, so the deregistration playbook never ran")
		}
		if f.launches != 0 {
			t.Fatalf("launched %d hook(s) without finding a host", f.launches)
		}
		// The deadline has to be durable by now, or every retry restarts
		// the clock and timeoutSeconds bounds nothing.
		first := f.childStatus(t, u).Deprovision
		if first == nil || first.Deadline == "" {
			t.Fatalf("no deadline recorded before the lookup that failed: %+v", first)
		}

		f.failHostSync = false
		res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
		if err != nil {
			t.Fatal(err)
		}
		if f.launches != 1 {
			t.Fatalf("expected the hook to launch once AWX answered, got %d launches", f.launches)
		}
		if res.Done {
			t.Fatal("released while the hook job was running")
		}
		if second := f.childStatus(t, u).Deprovision; second.Deadline != first.Deadline {
			t.Fatalf("deadline moved from %q to %q, so a retrying lookup extends the hook", first.Deadline, second.Deadline)
		}
	})

	t.Run("detach is not held open", func(t *testing.T) {
		f := newReconcileFixture(t)
		u := f.hookChild(t, hookRef(), nil)
		if err := unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
			t.Fatal(err)
		}
		f.children[u.GetName()] = u.DeepCopy()
		// VM still present: there was never a hook to run, so a failing
		// lookup is not worth holding a namespace in Terminating for.
		f.failHostSync = true

		res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
		if err != nil {
			t.Fatalf("a detach was held open by a lookup it did not need: %v", err)
		}
		if !res.Done {
			t.Fatal("a detach under Retain did not finish")
		}
		if st := f.childStatus(t, u); st.Deprovision.Phase != PhaseSkipped {
			t.Fatalf("phase %q, want %q", st.Deprovision.Phase, PhaseSkipped)
		}
		if f.launches != 0 {
			t.Fatalf("ran a decommission playbook against a live VM: %+v", f.launched)
		}
	})
}

func TestConnectionRestoreOnlyTouchesTheHostTheHookPinned(t *testing.T) {
	// Both cases finish the same hook under Retain, so the override has
	// to come back off a host that outlives it. They differ only in
	// whether the host still there is the one it went on.
	for _, tc := range []struct {
		name      string
		recreate  bool
		wantPatch int
	}{
		{name: "same host is restored", wantPatch: 1},
		{name: "replacement under the same name is left alone", recreate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			endpoint := awxEndpointFingerprint(f.conn.Spec.URL, APIBasePathLegacy)
			u := f.hookChild(t, hookRef(), func(st *AnsibleBindingVMStatus) {
				st.AWXEndpoint = endpoint
				st.Deprovision = &DeprovisionStatus{
					Phase: PhaseRunning, JobID: 42, JobType: TemplateTypeJob, JobStatus: "running",
					Endpoint:   endpoint,
					StartedAt:  nowRFC3339(),
					Deadline:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					HostPinned: true, PinnedHostID: 1, PinnedHostEndpoint: endpoint,
				}
			})
			if err := unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy"); err != nil {
				t.Fatal(err)
			}
			f.children[u.GetName()] = u.DeepCopy()
			f.deleteVM()
			f.jobStatusByID[42] = "successful"
			// The pin is on the host, as it would be by now.
			f.hosts.hosts[1]["variables"] = `{"ansible_host":"192.0.2.1","ansible_connection":"local"}`
			if tc.recreate {
				// Deleted out of band and recreated during the hook:
				// same name, same marker, different host.
				f.hosts.deleted[1] = true
				f.hosts.seed("web-1", hostOwnerMarker("ns", "bind"), `{"ansible_host":"192.0.2.9"}`)
			}

			res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Done {
				t.Fatal("a finished hook did not release")
			}
			if f.hosts.patched != tc.wantPatch {
				t.Fatalf("patched %d host(s), want %d", f.hosts.patched, tc.wantPatch)
			}
			st := f.childStatus(t, u)
			if st.Deprovision.HostPinned {
				t.Fatal("still claims a pin nothing will take off")
			}
			if !tc.recreate {
				return
			}
			if vars, _ := f.hosts.hosts[2]["variables"].(string); strings.Contains(vars, "ansible_connection") {
				t.Fatalf("wrote a connection variable onto a host that never had one: %s", vars)
			}
			if !strings.Contains(st.Deprovision.Message, "no longer the host the hook pinned") {
				t.Fatalf("outcome does not say the pin was left alone: %q", st.Deprovision.Message)
			}
		})
	}
}

// onDeleted targeting. ManagedHost aims the hook at this VM's inventory
// host and is what a manifest that says nothing gets; Template leaves
// the aiming to the AWX template, for a decommission whose records live
// somewhere other than the machine.

func templateHookRef() *DeprovisionHook {
	hook := hookRef()
	hook.Targeting = TargetingTemplate
	return hook
}

func TestOmittedTargetingIsManagedHost(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.hookChild(t, hookRef(), nil)
	f.deleteVM()

	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
		t.Fatal(err)
	}
	if f.launches != 1 {
		t.Fatalf("expected one launch, got %d", f.launches)
	}
	if got := f.launched[0].limit; got != "web-1" {
		t.Fatalf("limit %q, want the managed host: a manifest written before targeting existed must behave as it did", got)
	}
	st := f.childStatus(t, u)
	if st.Deprovision.Targeting != TargetingManagedHost {
		t.Fatalf("recorded targeting %q, want %q", st.Deprovision.Targeting, TargetingManagedHost)
	}
	if !st.Deprovision.HostPinned {
		t.Fatal("ManagedHost did not pin the host it aimed the playbook at")
	}
}

func TestTemplateTargetingLaunchesWithNeitherLimitNorInventory(t *testing.T) {
	f := newReconcileFixture(t)
	// A workflow with no inventory of its own and no Prompt on Launch
	// for Limit: refused under ManagedHost, ordinary under Template.
	f.hookInventory, f.hookAskLimit = -1, false
	u := f.hookChild(t, templateHookRef(), nil)
	f.deleteVM()

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if f.launches != 1 {
		t.Fatalf("expected one launch, got %d", f.launches)
	}
	if got := f.launched[0]; got.limit != "" || got.sentInventory {
		t.Fatalf("launch narrowed the run (limit %q, inventory sent %v); Template targeting must supply neither", got.limit, got.sentInventory)
	}
	if f.hosts.patched != 0 {
		t.Fatal("pinned ansible_connection on a host the hook was never aimed at")
	}
	if res.Done {
		t.Fatal("released while the hook job was still running")
	}
	st := f.childStatus(t, u)
	if st.Deprovision.Targeting != TargetingTemplate {
		t.Fatalf("recorded targeting %q, want %q", st.Deprovision.Targeting, TargetingTemplate)
	}
	if st.Deprovision.HostPinned {
		t.Fatal("recorded a pin it never made")
	}
	// The deletion context does not come from the host, so it is there
	// even though nothing was resolved against the inventory.
	if vars := f.launched[0].extraVars; vars["asb_vm_name"] != "web-1" || vars["asb_hook"] != "onDeleted" {
		t.Fatalf("deletion context missing from a Template-targeted launch: %v", vars)
	}
}

func TestTemplateTargetingDoesNotDependOnAManagedHost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*reconcileFixture)
	}{
		{name: "no inventory host at all", setup: func(f *reconcileFixture) { f.hosts.deleted[1] = true }},
		{name: "host owned by another binding", setup: func(f *reconcileFixture) {
			f.hosts.hosts[1]["description"] = hostOwnerMarker("ns", "other-binding")
		}},
		{name: "host lookup failing transiently", setup: func(f *reconcileFixture) { f.failHostSync = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{TargetingManagedHost, TargetingTemplate} {
				t.Run(mode, func(t *testing.T) {
					f := newReconcileFixture(t)
					hook := hookRef()
					hook.Targeting = mode
					u := f.hookChild(t, hook, nil)
					f.deleteVM()
					tc.setup(f)

					_, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)

					if mode == TargetingTemplate {
						if f.launches != 1 {
							t.Fatalf("a Template-targeted hook did not run: %d launches, err %v", f.launches, err)
						}
						if f.hosts.patched != 0 || f.hosts.deleted[2] {
							t.Fatal("controller touched a host it does not own")
						}
						return
					}
					// ManagedHost has nothing it may aim at, so it does
					// not guess: it records why and stops.
					if f.launches != 0 {
						t.Fatalf("ManagedHost launched without a host of its own: %+v", f.launched)
					}
				})
			}
		})
	}
}

func TestTemplateTargetingKeepsItsOutcomeWhenHostCleanupStillFails(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.hookChild(t, templateHookRef(), nil)
	f.deleteVM()
	f.failHostSync = true

	// The hook runs through a lookup it does not need.
	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("released while the hook job was still running")
	}
	if f.launches != 1 {
		t.Fatalf("expected the hook to launch anyway, got %d", f.launches)
	}
	first := f.childStatus(t, u).Deprovision
	if first == nil || first.JobID == 0 {
		t.Fatalf("launch was not recorded: %+v", first)
	}

	// The job finishes, but the host this policy still has to delete
	// cannot be found, so the pass fails and is retried.
	f.jobStatusByID[int(first.JobID)] = "successful"
	if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err == nil {
		t.Fatal("released the finalizer while the inventory host was still there to delete")
	}
	// The outcome is written down before that retry, or the next pass
	// finds a hook that never ran and launches a second one.
	if settled := f.childStatus(t, u).Deprovision; settled.Phase != PhaseSucceeded {
		t.Fatalf("terminal outcome not recorded before the host-cleanup retry: %+v", settled)
	}

	f.failHostSync = false
	res, err = cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if f.launches != 1 {
		t.Fatalf("relaunched the hook after a host-cleanup retry: %d launches", f.launches)
	}
	if !res.Done || !f.hosts.deleted[1] {
		t.Fatalf("teardown did not finish: done=%v deleted=%v", res.Done, f.hosts.deleted)
	}
	if st := f.childStatus(t, u); st.Deprovision.Phase != PhaseSucceeded {
		t.Fatalf("phase %q, want %q", st.Deprovision.Phase, PhaseSucceeded)
	}
}

func TestTargetingIsFixedWhenTheHookStarts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recorded  string
		spec      string
		wantLimit string
	}{
		{name: "edited to Template mid-hook", recorded: TargetingManagedHost, spec: TargetingTemplate, wantLimit: "web-1"},
		{name: "started before targeting was recorded", recorded: "", spec: TargetingTemplate, wantLimit: "web-1"},
		{name: "edited to ManagedHost mid-hook", recorded: TargetingTemplate, spec: TargetingManagedHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			hook := hookRef()
			hook.Targeting = tc.spec
			u := f.hookChild(t, hook, func(st *AnsibleBindingVMStatus) {
				// Started on a previous pass, not yet launched.
				st.Deprovision = &DeprovisionStatus{
					Phase: PhasePending, Targeting: tc.recorded,
					StartedAt: nowRFC3339(),
					Deadline:  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				}
			})
			f.deleteVM()

			if _, err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
				t.Fatal(err)
			}
			if f.launches != 1 {
				t.Fatalf("expected one launch, got %d", f.launches)
			}
			if got := f.launched[0].limit; got != tc.wantLimit {
				t.Fatalf("launched with limit %q, want %q: an edit must not re-aim a hook that already started", got, tc.wantLimit)
			}
		})
	}
}

func TestUnknownTargetingIsRefusedRatherThanGuessed(t *testing.T) {
	f := newReconcileFixture(t)
	hook := hookRef()
	hook.Targeting = "Everything"
	u := f.hookChild(t, hook, nil)
	f.deleteVM()

	res, err := cleanupAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if f.launches != 0 {
		t.Fatalf("launched under a targeting mode nobody asked for: %+v", f.launched)
	}
	if !res.Done {
		t.Fatal("a hook that will never run held the finalizer")
	}
	st := f.childStatus(t, u)
	if st.Deprovision.Phase != PhaseFailed || !strings.Contains(st.Deprovision.Message, "Everything") {
		t.Fatalf("refusal does not say what was wrong: %+v", st.Deprovision)
	}
}
