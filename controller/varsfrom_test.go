package main

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// testMapper maps the handful of kinds these tests use. A real
// RESTMapper needs a discovery client; the mapping itself is not what is
// under test here.
type testMapper struct {
	clusterScoped map[string]bool
}

func (m testMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	known := map[schema.GroupKind]string{
		{Group: "", Kind: "ConfigMap"}:                           "configmaps",
		{Group: "", Kind: "Service"}:                             "services",
		{Group: "", Kind: "Secret"}:                              "secrets",
		{Group: "", Kind: "Node"}:                                "nodes",
		{Group: "vmoperator.vmware.com", Kind: "VirtualMachine"}: "virtualmachines",
	}
	resource, ok := known[gk]
	if !ok {
		return nil, &meta.NoKindMatchError{GroupKind: gk}
	}
	version := "v1"
	if len(versions) > 0 {
		version = versions[0]
	}
	scope := meta.RESTScopeNamespace
	if m.clusterScoped[gk.Kind] {
		scope = meta.RESTScopeRoot
	}
	return &meta.RESTMapping{
		Resource: schema.GroupVersionResource{Group: gk.Group, Version: version, Resource: resource},
		Scope:    scope,
	}, nil
}

func (m testMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, nil
}
func (m testMapper) KindsFor(schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return nil, nil
}
func (m testMapper) ResourceFor(schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, nil
}
func (m testMapper) ResourcesFor(schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return nil, nil
}
func (m testMapper) RESTMappings(schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	return nil, nil
}
func (m testMapper) ResourceSingularizer(resource string) (string, error) { return resource, nil }

func obj(apiVersion, kind, name string, content map[string]interface{}) *unstructured.Unstructured {
	o := map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "ns"},
	}
	for k, v := range content {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

func newVarsFromClient(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "configmaps"}:                                 "ConfigMapList",
		{Group: "", Version: "v1", Resource: "services"}:                                   "ServiceList",
		{Group: "", Version: "v1", Resource: "secrets"}:                                    "SecretList",
		{Group: "", Version: "v1", Resource: "nodes"}:                                      "NodeList",
		{Group: "vmoperator.vmware.com", Version: "v1alpha5", Resource: "virtualmachines"}: "VirtualMachineList",
	}
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, runtimeObjs...)
}

// withVarsFrom sets the allowlist for one test, in the same
// "<group>/<resource>" form the vars_from_resources package value and the
// ClusterRole use. Nothing is ever granted by wildcard, so a test that
// reads a kind has to name it.
func withVarsFrom(t *testing.T, entries ...string) {
	t.Helper()
	previous := allowedVarsFromResources
	allowedVarsFromResources = parseVarsFromResources(strings.Join(entries, ","))
	t.Cleanup(func() { allowedVarsFromResources = previous })
}

// withDefaultVarsFrom grants what a deployed controller is granted out of
// the box.
func withDefaultVarsFrom(t *testing.T) {
	t.Helper()
	withVarsFrom(t, defaultVarsFromResources)
}

func source(apiVersion, kind, name string, vars map[string]string) VarsFromSource {
	return VarsFromSource{Resource: ResourceRef{APIVersion: apiVersion, Kind: kind, Name: name}, Vars: vars}
}

func TestResolveVarsFromReadsFieldsOffALiveObject(t *testing.T) {
	withDefaultVarsFrom(t)

	vm := obj("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-1", map[string]interface{}{
		"status": map[string]interface{}{
			"network": map[string]interface{}{"primaryIP4": "10.20.30.41"},
		},
	})
	client := newVarsFromClient(vm)

	resolved, names, err := resolveVarsFrom(context.Background(), client, testMapper{}, "ns",
		[]VarsFromSource{source("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-1", map[string]string{
			"record_name": "{.metadata.name}",
			"record_ip":   "{.status.network.primaryIP4}",
		})}, nil)
	if err != nil {
		t.Fatalf("resolveVarsFrom: %v", err)
	}
	if resolved["record_name"] != "web-1" || resolved["record_ip"] != "10.20.30.41" {
		t.Errorf("resolved = %v", resolved)
	}
	// Names are sorted so status doesn't churn between reconciles.
	if strings.Join(names, ",") != "record_ip,record_name" {
		t.Errorf("names = %v, want sorted", names)
	}
}

func TestResolveVarsFromRefusesSecrets(t *testing.T) {
	// extra_vars are echoed in AWX job output and kept in the job's stored
	// launch parameters, so this must be refused even though the core
	// group is allowed and the controller can technically read the Secret.
	withDefaultVarsFrom(t)

	client := newVarsFromClient(obj("v1", "Secret", "creds", map[string]interface{}{
		"data": map[string]interface{}{"password": "aHVudGVyMg=="},
	}))

	_, _, err := resolveVarsFrom(context.Background(), client, testMapper{}, "ns",
		[]VarsFromSource{source("v1", "Secret", "creds", map[string]string{"pw": "{.data.password}"})}, nil)
	if err == nil {
		t.Fatal("reading a Secret into extra_vars must be refused")
	}
	if !isTerminalError(err) {
		t.Error("refusing a Secret must be terminal: retrying will never make it allowed")
	}
	if !strings.Contains(err.Error(), "Credential") {
		t.Errorf("error should point at AWX Credentials as the alternative, got: %v", err)
	}
}

func TestResolveVarsFromEnforcesTheGroupAllowlist(t *testing.T) {
	withVarsFrom(t, "core/configmaps")

	client := newVarsFromClient(obj("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-1", nil))

	_, _, err := resolveVarsFrom(context.Background(), client, testMapper{}, "ns",
		[]VarsFromSource{source("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-1",
			map[string]string{"n": "{.metadata.name}"})}, nil)
	if err == nil || !isTerminalError(err) {
		t.Fatalf("a group outside the allowlist must be refused terminally, got: %v", err)
	}
	if !strings.Contains(err.Error(), "vars_from_resources") {
		t.Errorf("error should name the value that widens it, got: %v", err)
	}
}

func TestResolveVarsFromRefusesClusterScopedKinds(t *testing.T) {
	// varsFrom is namespace-pinned so a tenant cannot read a neighbour's
	// data; a cluster-scoped kind has no namespace to pin it to.
	withDefaultVarsFrom(t)

	client := newVarsFromClient()
	mapper := testMapper{clusterScoped: map[string]bool{"Node": true}}

	_, _, err := resolveVarsFrom(context.Background(), client, mapper, "ns",
		[]VarsFromSource{source("v1", "Node", "node-1", map[string]string{"n": "{.metadata.name}"})}, nil)
	if err == nil || !isTerminalError(err) {
		t.Fatalf("a cluster-scoped kind must be refused terminally, got: %v", err)
	}
}

func TestResolveVarsFromMissingObjectIsRetryable(t *testing.T) {
	// An orchestrator may create the run before the object it names has
	// settled. Failing here would make ordering a race;
	// activeDeadlineSeconds is what bounds the wait instead.
	withDefaultVarsFrom(t)

	client := newVarsFromClient()

	_, _, err := resolveVarsFrom(context.Background(), client, testMapper{}, "ns",
		[]VarsFromSource{source("v1", "ConfigMap", "not-yet", map[string]string{"n": "{.metadata.name}"})}, nil)
	if err == nil {
		t.Fatal("a missing object should be an error")
	}
	if isTerminalError(err) {
		t.Error("a missing object must stay retryable so it can appear later")
	}
}

func TestResolveVarsFromWaitsForAFieldThatIsNotPopulatedYet(t *testing.T) {
	// The commonest shape of this: a run created alongside the VM it is
	// about, reading the address the guest has not reported yet. The
	// object is there, so nothing retries the read; failing the path
	// made the run permanently unrunnable seconds before the value
	// appeared. spec.activeDeadlineSeconds is what bounds the wait.
	withVarsFrom(t, "vmoperator.vmware.com/virtualmachines")

	booting := obj("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-1", map[string]interface{}{
		"status": map[string]interface{}{"powerState": "PoweredOn"},
	})

	for _, path := range []string{"{.status.network.primaryIP4}", "{.status.network}"} {
		_, _, err := resolveVarsFrom(context.Background(), newVarsFromClient(booting), testMapper{}, "ns",
			[]VarsFromSource{source("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-1",
				map[string]string{"ip": path})}, nil)
		if err == nil {
			t.Fatalf("%s: a field that is not there yet is still an error to retry", path)
		}
		if isTerminalError(err) {
			t.Errorf("%s: a field its own controller has yet to fill in must stay retryable, got: %v", path, err)
		}
	}

	// A path that resolved to something of the wrong shape is a settled
	// fact about an immutable spec, and stays terminal.
	shaped := obj("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-2", map[string]interface{}{
		"status": map[string]interface{}{"volumes": []interface{}{map[string]interface{}{"name": "disk"}}},
	})
	_, _, err := resolveVarsFrom(context.Background(), newVarsFromClient(shaped), testMapper{}, "ns",
		[]VarsFromSource{source("vmoperator.vmware.com/v1alpha5", "VirtualMachine", "web-2",
			map[string]string{"vols": "{.status.volumes}"})}, nil)
	if err == nil || !isTerminalError(err) {
		t.Fatalf("a path resolving to a list is a spec error, not something to wait out, got: %v", err)
	}
}

func TestResolveVarsFromRejectsCollisions(t *testing.T) {
	withDefaultVarsFrom(t)

	cm := obj("v1", "ConfigMap", "cfg", map[string]interface{}{
		"data": map[string]interface{}{"zone": "corp.example.com"},
	})

	t.Run("with extraVars", func(t *testing.T) {
		_, _, err := resolveVarsFrom(context.Background(), newVarsFromClient(cm), testMapper{}, "ns",
			[]VarsFromSource{source("v1", "ConfigMap", "cfg", map[string]string{"zone": "{.data.zone}"})},
			map[string]string{"zone": "already-set"})
		if err == nil || !isTerminalError(err) {
			t.Fatalf("a key already in extraVars must be refused rather than silently winning, got: %v", err)
		}
	})

	t.Run("between sources", func(t *testing.T) {
		src := source("v1", "ConfigMap", "cfg", map[string]string{"zone": "{.data.zone}"})
		_, _, err := resolveVarsFrom(context.Background(), newVarsFromClient(cm), testMapper{}, "ns",
			[]VarsFromSource{src, src}, nil)
		if err == nil || !isTerminalError(err) {
			t.Fatalf("the same key from two sources must be refused, got: %v", err)
		}
	})
}

func TestEvalJSONPathScalarCoercion(t *testing.T) {
	// unstructured decodes every JSON number as float64, so an integer
	// port must not reach the playbook as "8080.000000".
	o := obj("v1", "Service", "frontend", map[string]interface{}{
		"spec": map[string]interface{}{
			"clusterIP": "10.96.0.1",
			"ports":     []interface{}{map[string]interface{}{"port": float64(8080)}},
			"replicas":  float64(3),
			"headless":  true,
		},
	})

	tests := []struct {
		name string
		path string
		want string
	}{
		{"string", "{.spec.clusterIP}", "10.96.0.1"},
		{"whole float prints as an integer", "{.spec.replicas}", "3"},
		{"bool", "{.spec.headless}", "true"},
		{"through a list index", "{.spec.ports[0].port}", "8080"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalJSONPath(o, tc.path)
			if err != nil {
				t.Fatalf("evalJSONPath(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("evalJSONPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestEvalJSONPathRejectsNonScalars(t *testing.T) {
	// extraVars is map[string]string. Silently JSON-encoding structure
	// into it would hand the playbook a string where it expected a list.
	o := obj("v1", "Service", "frontend", map[string]interface{}{
		"spec": map[string]interface{}{
			"ports":    []interface{}{map[string]interface{}{"port": float64(80)}},
			"selector": map[string]interface{}{"app": "web"},
		},
	})

	for _, path := range []string{"{.spec.ports}", "{.spec.selector}"} {
		if got, err := evalJSONPath(o, path); err == nil {
			t.Errorf("evalJSONPath(%q) = %q, want an error: extra variables must be scalars", path, got)
		}
	}
	if _, err := evalJSONPath(o, "{.spec.nope}"); err == nil {
		t.Error("a path matching nothing must error rather than yield an empty variable")
	}
	if _, err := evalJSONPath(o, "{.spec[}"); err == nil {
		t.Error("a malformed JSONPath must error")
	}
}

func TestParseVarsFromResources(t *testing.T) {
	// "core" is the config spelling; RBAC and the API want "".
	got := parseVarsFromResources(" core/configmaps , vmoperator.vmware.com/virtualmachines ,")
	want := []varsFromResource{
		{Group: "", Resource: "configmaps"},
		{Group: "vmoperator.vmware.com", Resource: "virtualmachines"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseVarsFromResources = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseVarsFromResources = %#v, want %#v", got, want)
		}
	}
	// An empty setting must allow nothing, and an entry naming a group
	// with no resource must be dropped rather than read as "everything in
	// that group" - the whole point is that nothing is a wildcard.
	for _, csv := range []string{"", "core", "core/", "/configmaps"} {
		if n := len(parseVarsFromResources(csv)); n != 0 {
			t.Errorf("parseVarsFromResources(%q) allowed %d entries, want none", csv, n)
		}
	}
}

var _ = metav1.GetOptions{}

// Nothing is granted by wildcard: the ClusterRole names every kind
// varsFrom may read, and the controller is handed the same list. A kind
// outside it has to be refused here, with an explanation - the read
// would otherwise go out and come back Forbidden, which says nothing
// about how to fix it. An allowed group is not enough on its own, which
// is what this checks: core is allowed, for ConfigMaps only.
func TestResolveVarsFromEnforcesTheResourceAllowlist(t *testing.T) {
	withVarsFrom(t, "core/configmaps", "vmoperator.vmware.com/virtualmachines")

	svc := obj("v1", "Service", "web", map[string]interface{}{
		"spec": map[string]interface{}{"clusterIP": "10.96.0.5"},
	})
	client := newVarsFromClient(svc)

	_, _, err := resolveVarsFrom(context.Background(), client, testMapper{}, "ns",
		[]VarsFromSource{source("v1", "Service", "web", map[string]string{"ip": "{.spec.clusterIP}"})}, nil)
	if err == nil {
		t.Fatal("expected a Service read to be refused: the ClusterRole grants core/configmaps, not every core kind")
	}
	if !isTerminalError(err) {
		t.Errorf("a resource this service is not granted is a settled fact, not something to retry: %v", err)
	}
	for _, want := range []string{"core/services", "vars_from_resources", "core/configmaps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the refusal to name %q, got: %v", want, err)
		}
	}
}

// Widening it is what the package value is for, and the gate has to
// follow the grant rather than a compiled-in list.
func TestResolveVarsFromReadsAWidenedResource(t *testing.T) {
	withVarsFrom(t, "core/configmaps", "core/services")

	svc := obj("v1", "Service", "web", map[string]interface{}{
		"spec": map[string]interface{}{"clusterIP": "10.96.0.5"},
	})
	client := newVarsFromClient(svc)

	resolved, _, err := resolveVarsFrom(context.Background(), client, testMapper{}, "ns",
		[]VarsFromSource{source("v1", "Service", "web", map[string]string{"ip": "{.spec.clusterIP}"})}, nil)
	if err != nil {
		t.Fatalf("a kind named in vars_from_resources must be readable: %v", err)
	}
	if resolved["ip"] != "10.96.0.5" {
		t.Errorf("expected ip=10.96.0.5, got %q", resolved["ip"])
	}
}
