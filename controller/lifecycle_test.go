package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Exercises complete parent/child reconciles through the real dynamic and
// AWX HTTP clients. The fixture implements only the API operations under test.
type reconcileFixture struct {
	mu             sync.Mutex
	client         *dynamic.DynamicClient
	conn           AWXConnection
	vms            []unstructured.Unstructured
	children       map[string]*unstructured.Unstructured
	hosts          *hostStore
	requests       []string
	awxRequests    []string
	rejectCreates  bool
	rejectStatus   bool
	loseHostResult bool
	failHostSync   bool
	jobStatus      string
	launches       int
}

func fixtureObject(t *testing.T, value interface{}, kind string) *unstructured.Unstructured {
	t.Helper()
	m, err := structToMap(value)
	if err != nil {
		t.Fatal(err)
	}
	m["apiVersion"], m["kind"] = ansBindGVR.GroupVersion().String(), kind
	return &unstructured.Unstructured{Object: m}
}

func fixtureVM(name, uid string) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": vmGVR.GroupVersion().String(), "kind": "VirtualMachine",
		"metadata": map[string]interface{}{"name": name, "namespace": "ns", "uid": uid, "labels": map[string]interface{}{"app": "web"}},
		"status":   map[string]interface{}{"powerState": "PoweredOn", "network": map[string]interface{}{"primaryIP4": "192.0.2.1"}},
	}}
	return u
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	f := &reconcileFixture{children: map[string]*unstructured.Unstructured{}, hosts: newHostStore(), jobStatus: "running"}
	f.vms = []unstructured.Unstructured{fixtureVM("web-1", "vm-1")}
	awx := httptest.NewServer(http.HandlerFunc(f.serveAWX))
	t.Cleanup(awx.Close)
	f.conn = AWXConnection{
		// t.Name() carries the subtest path, and "/" is not legal in a
		// resource name.
		ObjectMeta: metav1.ObjectMeta{Name: childName(strings.ReplaceAll(t.Name(), "/", "-"), "connection"), Namespace: "ns", ResourceVersion: "1"},
		Spec:       &AWXConnectionSpec{URL: awx.URL, APIBasePath: APIBasePathLegacy, SecretRef: "token"},
	}
	kube := httptest.NewServer(http.HandlerFunc(f.serveKube))
	t.Cleanup(kube.Close)
	var err error
	f.client, err = dynamic.NewForConfig(&rest.Config{Host: kube.URL})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *reconcileFixture) binding(t *testing.T, policy string) *unstructured.Unstructured {
	return fixtureObject(t, AnsibleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "bind", Namespace: "ns", Generation: 1},
		Spec: &AnsibleBindingSpec{VMSelector: map[string]string{"app": "web"}, AWXConnectionRef: f.conn.Name,
			Template: TemplateRef{Name: "setup", Type: TemplateTypeJob}, CleanupPolicy: policy},
		Status: &AnsibleBindingStatus{LastOrphanScan: nowRFC3339()},
	}, "AnsibleBinding")
}

func (f *reconcileFixture) addChild(t *testing.T, vmName string, st *AnsibleBindingVMStatus) *unstructured.Unstructured {
	t.Helper()
	ac, _ := convertAnsibleBinding(f.binding(t, CleanupPolicyDelete))
	vmUID := types.UID("vm-1")
	for _, vm := range f.vms {
		if vm.GetName() == vmName {
			vmUID = vm.GetUID()
		}
	}
	spec := childSpecFor(&ac, vmName, "")
	u := fixtureObject(t, AnsibleBindingVM{
		ObjectMeta: metav1.ObjectMeta{Name: childName("bind", vmName), Namespace: "ns", UID: types.UID("child-" + vmName), ResourceVersion: "1",
			Labels:          map[string]string{BindingLabel: bindingLabelValue("bind")},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: vmGVR.GroupVersion().String(), Kind: "VirtualMachine", Name: vmName, UID: vmUID}}},
		Spec: &spec, Status: st,
	}, "AnsibleBindingVM")
	f.children[u.GetName()] = u.DeepCopy()
	return u
}

func (f *reconcileFixture) serveAWX(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.awxRequests = append(f.awxRequests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(r.URL.Path, "/inventories/") && f.failHostSync:
		http.Error(w, "temporary host-sync failure", http.StatusServiceUnavailable)
	case strings.HasSuffix(r.URL.Path, "/job_templates/") || strings.HasSuffix(r.URL.Path, "/workflow_job_templates/"):
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": 1, "results": []interface{}{map[string]interface{}{
			"id": 1, "name": "setup", "inventory": 1, "ask_limit_on_launch": true, "ask_variables_on_launch": true,
		}}})
	case strings.HasSuffix(r.URL.Path, "/launch/"):
		f.launches++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 42})
	case strings.Contains(r.URL.Path, "/jobs/") || strings.Contains(r.URL.Path, "/workflow_jobs/"):
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": f.jobStatus})
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/hosts/"):
		var id int
		_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, APIBasePathLegacy), "/hosts/%d/", &id)
		f.hosts.deleted[id] = true
		w.WriteHeader(http.StatusNoContent)
	default:
		f.hosts.handler().ServeHTTP(w, r)
	}
}

func (f *reconcileFixture) serveKube(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	reply := func(v interface{}) { _ = json.NewEncoder(w).Encode(v) }
	fail := func(code int, reason string) {
		w.WriteHeader(code)
		reply(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReason(reason), Code: int32(code)})
	}
	if r.Method == http.MethodGet {
		switch {
		case strings.Contains(r.URL.Path, "/secrets/"):
			reply(map[string]interface{}{"apiVersion": "v1", "kind": "Secret", "data": map[string]interface{}{"token": "dG9rZW4="}})
		case strings.Contains(r.URL.Path, "/awxconnections/"):
			// Through the dynamic client, so it has to carry apiVersion
			// and kind the way the API server sends them.
			conn, err := structToMap(f.conn)
			if err != nil {
				fail(500, "InternalError")
				return
			}
			conn["apiVersion"], conn["kind"] = awxConnGVR.GroupVersion().String(), "AWXConnection"
			reply(conn)
		case strings.HasSuffix(r.URL.Path, "/virtualmachines"):
			reply(&unstructured.UnstructuredList{Object: map[string]interface{}{"apiVersion": vmGVR.GroupVersion().String(), "kind": "VirtualMachineList"}, Items: f.vms})
		case strings.Contains(r.URL.Path, "/virtualmachines/"):
			for _, vm := range f.vms {
				if strings.HasSuffix(r.URL.Path, "/"+vm.GetName()) {
					reply(vm.Object)
					return
				}
			}
			fail(404, "NotFound")
		case strings.HasSuffix(r.URL.Path, "/ansiblebindingvms"):
			items := []unstructured.Unstructured{}
			for _, child := range f.children {
				items = append(items, *child.DeepCopy())
			}
			reply(&unstructured.UnstructuredList{Object: map[string]interface{}{"apiVersion": ansBindVMGVR.GroupVersion().String(), "kind": "AnsibleBindingVMList"}, Items: items})
		default:
			fail(404, "NotFound")
		}
		return
	}
	if strings.Contains(r.URL.Path, "/ansiblebindings/") {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reply(body)
		return
	}
	if r.Method == http.MethodPost {
		if f.rejectCreates {
			fail(403, "Forbidden")
			return
		}
		u := &unstructured.Unstructured{}
		_ = json.NewDecoder(r.Body).Decode(&u.Object)
		if f.children[u.GetName()] != nil {
			fail(409, "AlreadyExists")
			return
		}
		u.SetUID(types.UID("created-" + u.GetName()))
		u.SetResourceVersion("1")
		f.children[u.GetName()] = u
		reply(u.Object)
		return
	}
	parts := strings.Split(r.URL.Path, "/ansiblebindingvms/")
	if len(parts) != 2 {
		fail(404, "NotFound")
		return
	}
	name := strings.Split(parts[1], "/")[0]
	u := f.children[name]
	if u == nil {
		fail(404, "NotFound")
		return
	}
	switch {
	case r.Method == http.MethodDelete:
		var opts metav1.DeleteOptions
		_ = json.NewDecoder(r.Body).Decode(&opts)
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != u.GetUID() {
			fail(409, "Conflict")
			return
		}
		now := metav1.Now()
		u.SetDeletionTimestamp(&now)
	case strings.HasSuffix(r.URL.Path, "/status"):
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st := body["status"].(map[string]interface{})
		if f.rejectStatus || (f.loseHostResult && st["awxHostID"] != nil) {
			fail(500, "InternalError")
			return
		}
		u.Object["status"] = st
	case r.Method == http.MethodPatch:
		if r.Header.Get("Content-Type") != string(types.JSONPatchType) {
			fail(415, "UnsupportedMediaType")
			return
		}
		var patch []struct {
			Op, Path string
			Value    interface{}
		}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		for _, op := range patch {
			if op.Op == "test" {
				var actual string
				switch op.Path {
				case "/metadata/uid":
					actual = string(u.GetUID())
				case "/metadata/resourceVersion":
					actual = u.GetResourceVersion()
				}
				if actual != op.Value {
					fail(409, "Conflict")
					return
				}
			} else if op.Op == "replace" && op.Path == "/spec" {
				u.Object["spec"] = op.Value
			}
		}
	default:
		fail(405, "MethodNotAllowed")
		return
	}
	u.SetResourceVersion(u.GetResourceVersion() + "1")
	reply(u.Object)
}

func TestChildSpecReplacementClearsFieldsAndPreservesMetadata(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.addChild(t, "web-1", &AnsibleBindingVMStatus{LastJobID: 42})
	child, _ := convertAnsibleBindingVM(u)
	original := *child.Spec
	child.Spec.HostName, child.Spec.UseDefaultLimit, child.Spec.BindingTrigger = "old", true, "old-trigger"
	child.Spec.ExtraVars = map[string]string{"old": "value"}
	child.Spec.HostVariables = map[string]string{"old": "value"}
	f.children[u.GetName()] = fixtureObject(t, child, "AnsibleBindingVM")
	if err := updateBindingChildSpec(context.Background(), f.client, &child, original); err != nil {
		t.Fatal(err)
	}
	got, _ := convertAnsibleBindingVM(f.children[u.GetName()])
	if !reflect.DeepEqual(*got.Spec, original) {
		t.Fatalf("spec not replaced: %+v", got.Spec)
	}
	if !reflect.DeepEqual(got.OwnerReferences, child.OwnerReferences) || got.Status.LastJobID != 42 || got.Labels[BindingLabel] == "" {
		t.Fatal("metadata/status changed")
	}
	if err := updateBindingChildSpec(context.Background(), f.client, &child, original); err == nil {
		t.Fatal("stale resourceVersion accepted")
	}
	fresh := got
	f.children[u.GetName()].SetUID("replacement")
	if err := updateBindingChildSpec(context.Background(), f.client, &fresh, original); err == nil {
		t.Fatal("replacement UID accepted")
	}
	delete(f.children, u.GetName())
	if err := updateBindingChildSpec(context.Background(), f.client, &child, original); !apierrors.IsNotFound(err) {
		t.Fatalf("missing child recreated: %v", err)
	}
}

func TestParentCountsMissingAndReplacementVMsAsPending(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			f := newReconcileFixture(t)
			old := f.addChild(t, "web-1", &AnsibleBindingVMStatus{Phase: PhaseSucceeded, AppliedGeneration: 1})
			if replacement {
				f.vms[0].SetUID("replacement-vm")
			} else {
				f.vms = append(f.vms, fixtureVM("web-2", "vm-2"))
			}
			result, err := applyAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete))
			if err != nil {
				t.Fatal(err)
			}
			u, _ := toUnstructured(result.Object)
			binding, _ := convertAnsibleBinding(u)
			if binding.Status.Summary.Total != len(f.vms) || binding.Status.Summary.Pending != 1 || updateAnsibleBindingStatus(u, true, nil)["ready"] == true {
				t.Fatalf("incorrect readiness: %+v", binding.Status.Summary)
			}
			if replacement && f.children[old.GetName()].GetDeletionTimestamp() == nil {
				t.Fatal("old UID child was not retired")
			}
		})
	}
}

func TestQuotaFailuresDoNotPreventObsoleteChildDeletion(t *testing.T) {
	f := newReconcileFixture(t)
	for i := 0; i < 3; i++ {
		f.addChild(t, fmt.Sprintf("old-%d", i), nil)
	}
	f.vms = nil
	for i := 0; i < 3; i++ {
		f.vms = append(f.vms, fixtureVM(fmt.Sprintf("new-%d", i), fmt.Sprintf("uid-%d", i)))
	}
	f.rejectCreates = true
	if _, err := applyAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyDelete)); err == nil {
		t.Fatal("expected quota error")
	}
	for _, child := range f.children {
		if child.GetDeletionTimestamp() == nil {
			t.Fatal("quota-releasing delete was starved")
		}
	}
	deleted := 0
	for _, req := range f.requests {
		if strings.HasPrefix(req, "DELETE ") {
			deleted++
		}
		if strings.HasPrefix(req, "POST ") && deleted != 3 {
			t.Fatal("creates ran before obsolete-child deletions")
		}
	}
}

func TestRetainPropagatesDuringParentFinalization(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		t.Run(fmt.Sprint(terminating), func(t *testing.T) {
			f := newReconcileFixture(t)
			u := f.addChild(t, "web-1", &AnsibleBindingVMStatus{AWXHostID: 1, AWXHostCreated: true})
			if terminating {
				now := metav1.Now()
				f.children[u.GetName()].SetDeletionTimestamp(&now)
			}
			if err := cleanupAnsibleBinding(context.Background(), f.client, f.binding(t, CleanupPolicyRetain)); err == nil {
				t.Fatal("parent must wait for children")
			}
			got := f.children[u.GetName()]
			policy, _, _ := unstructured.NestedString(got.Object, "spec", "cleanupPolicy")
			if policy != CleanupPolicyRetain || got.GetDeletionTimestamp() == nil {
				t.Fatalf("policy/deletion not propagated: %v", got.Object)
			}
			if err := cleanupAnsibleBindingVM(context.Background(), f.client, got); err != nil {
				t.Fatal(err)
			}
			if len(f.awxRequests) != 0 {
				t.Fatal("Retain contacted AWX")
			}
		})
	}
}

func TestRunningJobUsesLaunchConnectionAndType(t *testing.T) {
	f := newReconcileFixture(t)
	launchConnection := *f.conn.Spec
	u := f.addChild(t, "web-1", &AnsibleBindingVMStatus{
		LastJobID: 42, LastJobStatus: "running", Phase: PhaseRunning, AppliedGeneration: 1,
		LastJobType: TemplateTypeJob, LastJobConnection: &launchConnection,
	})
	_ = unstructured.SetNestedField(u.Object, TemplateTypeWorkflow, "spec", "template", "type")
	_ = unstructured.SetNestedField(u.Object, int64(2), "spec", "bindingGeneration")
	var newRequests []string
	var newMu sync.Mutex
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newMu.Lock()
		defer newMu.Unlock()
		newRequests = append(newRequests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/launch/") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 99})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": 1, "results": []interface{}{map[string]interface{}{"id": 9, "name": "setup", "inventory": nil}}})
	}))
	defer next.Close()
	f.conn.Spec.URL = next.URL
	f.conn.ResourceVersion = "2"
	first, err := applyAnsibleBindingVM(context.Background(), f.client, u)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.awxRequests) != 1 || f.awxRequests[0] != "GET /api/v2/jobs/42/" || len(newRequests) != 0 {
		t.Fatalf("wrong job polled: old=%v new=%v", f.awxRequests, newRequests)
	}
	f.jobStatus = "successful"
	second, err := applyAnsibleBindingVM(context.Background(), f.client, first.Object)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := toUnstructured(second.Object)
	child, _ := convertAnsibleBindingVM(result)
	if child.Status.LastJobID != 99 || child.Status.LastJobType != TemplateTypeWorkflow || child.Status.LastJobConnection.URL != next.URL {
		t.Fatalf("next run did not use updated settings: %+v", child.Status)
	}
}

func TestTransientHostFailureDoesNotChangeSuccessfulJobOutcome(t *testing.T) {
	f := newReconcileFixture(t)
	f.hosts.seed("web-1", hostOwnerMarker("ns", "bind"), "{}")
	u := f.addChild(t, "web-1", &AnsibleBindingVMStatus{LastJobID: 42, LastJobStatus: "successful", Phase: PhaseSucceeded, AppliedGeneration: 1})
	f.failHostSync = true
	failed, err := applyAnsibleBindingVM(context.Background(), f.client, u)
	if err == nil {
		t.Fatal("expected host-sync error")
	}
	f.failHostSync = false
	recovered, err := applyAnsibleBindingVM(context.Background(), f.client, failed.Object)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := toUnstructured(recovered.Object)
	if updateAnsibleBindingVMStatus(result, true, nil)["ready"] != true || f.launches != 0 {
		t.Fatal("successful job did not recover without relaunch")
	}
}

func TestHostIntentSurvivesLostResultAndProtectsAdoptedHosts(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(fmt.Sprint(adopted), func(t *testing.T) {
			f := newReconcileFixture(t)
			if adopted {
				f.hosts.seed("web-1", "", "{}")
			}
			u := f.addChild(t, "web-1", nil)
			f.loseHostResult = true
			if _, err := applyAnsibleBindingVM(context.Background(), f.client, u); err == nil {
				t.Fatal("expected lost status write")
			}
			// Simulate restart/deletion using only the last successfully saved
			// API status, not the richer in-memory result of the reconcile.
			saved := f.children[u.GetName()].DeepCopy()
			child, _ := convertAnsibleBindingVM(saved)
			if child.Status == nil || child.Status.AWXHostID != 0 || child.Status.AWXInventoryID != 1 || child.Status.AWXHostName != "web-1" {
				t.Fatalf("missing durable intent: %+v", child.Status)
			}
			if err := cleanupAnsibleBindingVM(context.Background(), f.client, saved); err != nil {
				t.Fatal(err)
			}
			if f.hosts.deleted[1] == adopted {
				t.Fatalf("ownership not respected: adopted=%v deleted=%v", adopted, f.hosts.deleted)
			}
		})
	}
}

func TestFailedIntentWritePreventsExternalHostCreation(t *testing.T) {
	f := newReconcileFixture(t)
	u := f.addChild(t, "web-1", nil)
	f.rejectStatus = true
	if _, err := applyAnsibleBindingVM(context.Background(), f.client, u); err == nil {
		t.Fatal("expected intent write failure")
	}
	if f.hosts.created != 0 || f.launches != 0 {
		t.Fatal("external resources created without durable intent")
	}
}

func TestCleanupWithoutStatusDiscoversOnlyOwnedHost(t *testing.T) {
	for _, marker := range []string{hostOwnerMarker("ns", "bind"), "", hostOwnerMarker("ns", "other")} {
		t.Run(fmt.Sprintf("%x", marker), func(t *testing.T) {
			f := newReconcileFixture(t)
			f.hosts.seed("web-1", marker, "{}")
			u := f.addChild(t, "web-1", nil)
			if err := cleanupAnsibleBindingVM(context.Background(), f.client, u); err != nil {
				t.Fatal(err)
			}
			if f.hosts.deleted[1] != (marker == hostOwnerMarker("ns", "bind")) {
				t.Fatal("cleanup used missing status as proof of absence or ignored ownership")
			}
		})
	}
}
