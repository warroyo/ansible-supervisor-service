package main

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// One AnsibleBindingVM per VirtualMachine, whatever selects it. The
// object's name is the claim, so these exercise what happens when two
// bindings want the same VM - and what must not happen to the one that
// already has it.

// claimFor seeds the canonical child for a VM under some other binding,
// as though that binding had got there first.
func (f *reconcileFixture) claimFor(t *testing.T, bindingName, bindingUID, vmName string) *unstructured.Unstructured {
	t.Helper()
	owner := AnsibleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: "ns", UID: types.UID(bindingUID), Generation: 1},
		Spec: &AnsibleBindingSpec{VMSelector: map[string]string{"app": "web"}, AWXConnectionRef: f.conn.Name,
			Template: TemplateRef{Name: "setup", Type: TemplateTypeJob}},
	}
	spec := childSpecFor(&owner, vmName, "")
	vmUID := types.UID("vm-1")
	for _, vm := range f.vms {
		if vm.GetName() == vmName {
			vmUID = vm.GetUID()
		}
	}
	u := fixtureObject(t, AnsibleBindingVM{
		ObjectMeta: metav1.ObjectMeta{
			Name: childName(vmName), Namespace: "ns", UID: types.UID("child-" + vmName), ResourceVersion: "1",
			Labels:          map[string]string{BindingLabel: bindingLabelValue(bindingName)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: vmGVR.GroupVersion().String(), Kind: "VirtualMachine", Name: vmName, UID: vmUID}},
		},
		Spec: &spec,
	}, "AnsibleBindingVM")
	f.children[u.GetName()] = u.DeepCopy()
	return u
}

func bindingStatusOf(t *testing.T, result Result) (AnsibleBinding, *unstructured.Unstructured) {
	t.Helper()
	u, err := toUnstructured(result.Object)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := convertAnsibleBinding(u)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Status == nil || binding.Status.Summary == nil {
		t.Fatalf("no rollup on the binding: %+v", binding.Status)
	}
	return binding, u
}

func TestAVMClaimedByAnotherBindingIsReportedNotStolen(t *testing.T) {
	// Both halves of the lookup: an informer that has seen the other
	// binding's child, and one that has not and only finds out because
	// the create is rejected.
	for _, cached := range []bool{true, false} {
		name := "cache miss, resolved by AlreadyExists"
		if cached {
			name = "seen in the informer cache"
		}
		t.Run(name, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.vms = append(f.vms, fixtureVM("web-2", "vm-2"))
			taken := f.claimFor(t, "platform-base", "other-uid", "web-1")
			if cached {
				f.warmChildCache(t, taken)
			}

			result, err := applyAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete))
			if err != nil {
				// A VM someone else owns is a reportable outcome, not a
				// reconcile failure: as an error it would be buried
				// under "reconciliation failed" and retried hard.
				t.Fatalf("an ownership conflict was returned as an error: %v", err)
			}

			binding, u := bindingStatusOf(t, result)
			s := binding.Status.Summary
			if s.Total != 2 || s.Conflicted != 1 {
				t.Fatalf("rollup does not report the conflict: %+v", s)
			}
			// Conflicted VMs are not also pending: this binding is not
			// waiting for web-1 to start, it will never run it.
			if s.Pending != 1 {
				t.Fatalf("conflicted VM counted as pending too: %+v", s)
			}
			if len(s.ConflictedVMs) != 1 || !strings.Contains(s.ConflictedVMs[0], "web-1 (ns/platform-base)") {
				t.Fatalf("conflict sample does not name the owner: %v", s.ConflictedVMs)
			}

			status := updateAnsibleBindingStatus(u, true, nil)
			if status["state"] != "Conflict" || status["ready"] == true {
				t.Fatalf("binding did not report Conflict: %v", status)
			}
			if result.RequeueAfter <= 0 {
				t.Fatal("nothing will wake this binding when the claim is released")
			}

			// The other binding's child is untouched, and the VM it does
			// own carries on.
			after := f.children[taken.GetName()]
			if after.GetDeletionTimestamp() != nil {
				t.Fatal("deleted another binding's claim")
			}
			ownerName, _, _ := unstructured.NestedString(after.Object, "spec", "bindingName")
			if ownerName != "platform-base" {
				t.Fatalf("rewrote another binding's claim to %q", ownerName)
			}
			if f.children[childName("web-2")] == nil {
				t.Fatal("a conflict on one VM starved an uncontested one")
			}
		})
	}
}

func TestConflictIsReportedOnceRatherThanEveryPass(t *testing.T) {
	f := newReconcileFixture(t)
	f.claimFor(t, "platform-base", "other-uid", "web-1")

	first, err := applyAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 1 {
		t.Fatalf("expected one warning Event on entering a conflict, got %d", len(f.events))
	}

	// The second pass is handed the binding as the first left it, which
	// is what a controller re-reading its own object gets.
	f.requests, f.events = nil, nil
	if _, err := applyAnsibleBinding(context.Background(), f.client, first.Object); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 0 {
		t.Fatalf("an unchanged conflict recorded another Event: %v", f.events)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/status") {
			t.Fatalf("an unchanged conflict rewrote status: %v", f.requests)
		}
	}
}

func TestABindingDoesNotInheritItsPreviousIncarnationsClaim(t *testing.T) {
	f := newReconcileFixture(t)
	// Same name, deleted and recreated: a different object with a
	// different intent, and a child left mid-cleanup by the previous one.
	f.claimFor(t, "bind", "an-older-incarnation", "web-1")

	result, err := applyAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete))
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := bindingStatusOf(t, result)
	if binding.Status.Summary.Conflicted != 1 {
		t.Fatalf("adopted the previous incarnation's claim: %+v", binding.Status.Summary)
	}
	for _, r := range f.requests {
		if strings.HasPrefix(r, "PATCH ") && strings.Contains(r, childName("web-1")) {
			t.Fatalf("wrote to a child it does not own: %v", f.requests)
		}
	}
}

func TestAChildThatIsNotTheClaimNeverReachesAWX(t *testing.T) {
	f := newReconcileFixture(t)
	child := f.claimFor(t, "bind", "bind-uid", "web-1")
	// Renamed by hand: the spec still says web-1, but this object is not
	// what holds the claim on it.
	child.SetName("hand-made")

	_, err := applyAnsibleBindingVM(context.Background(), f.client, child)
	if err == nil {
		t.Fatal("a child under an arbitrary name provisioned a VM whose claim belongs elsewhere")
	}
	if !strings.Contains(err.Error(), childName("web-1")) {
		t.Fatalf("refusal does not say what the claim actually is: %v", err)
	}
	if len(f.awxRequests) != 0 {
		t.Fatalf("talked to AWX first: %v", f.awxRequests)
	}
}

func TestUpgradeGateRefusesToRunAlongsideOlderChildren(t *testing.T) {
	t.Run("clean installation starts", func(t *testing.T) {
		f := newReconcileFixture(t)
		f.claimFor(t, "bind", "bind-uid", "web-1")
		if err := checkForLegacyChildren(context.Background(), f.client); err != nil {
			t.Fatalf("refused to start on children it created itself: %v", err)
		}
	})

	t.Run("a child from the previous scheme blocks", func(t *testing.T) {
		f := newReconcileFixture(t)
		legacy := f.claimFor(t, "bind", "bind-uid", "web-1")
		delete(f.children, legacy.GetName())
		legacy.SetName("bind-web-1-0123456789")
		f.children[legacy.GetName()] = legacy

		err := checkForLegacyChildren(context.Background(), f.client)
		if err == nil {
			t.Fatal("started alongside a child from the previous naming scheme, which is two owners for one VM")
		}
		for _, want := range []string{"bind-web-1-0123456789", "web-1", "roll back"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal does not mention %q: %v", want, err)
			}
		}
		if len(f.awxRequests) != 0 {
			t.Fatalf("reached AWX before refusing: %v", f.awxRequests)
		}
	})

	t.Run("a child with no owning incarnation blocks", func(t *testing.T) {
		f := newReconcileFixture(t)
		child := f.claimFor(t, "bind", "bind-uid", "web-1")
		unstructured.RemoveNestedField(child.Object, "spec", "bindingUID")
		f.children[child.GetName()] = child

		if err := checkForLegacyChildren(context.Background(), f.client); err == nil {
			t.Fatal("accepted a child whose owning binding incarnation is unknown")
		}
	})
}
