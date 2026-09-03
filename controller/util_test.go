package main

import (
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeDiscovery serves a hand-built group list, so version selection is
// exercised without a live API server. resourcesByGV names the versions
// that actually serve "virtualmachines"; a version present in versions
// but absent here is one the group advertises without the resource.
type fakeDiscovery struct {
	versions        []string
	preferred       string
	servesResource  map[string]bool
	groupsErr       error
	resourcesErrFor map[string]bool
}

func (f *fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	if f.groupsErr != nil {
		return nil, f.groupsErr
	}
	if len(f.versions) == 0 {
		return &metav1.APIGroupList{}, nil
	}
	g := metav1.APIGroup{Name: vmGroup}
	for _, v := range f.versions {
		g.Versions = append(g.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: vmGroup + "/" + v,
			Version:      v,
		})
	}
	g.PreferredVersion = metav1.GroupVersionForDiscovery{
		GroupVersion: vmGroup + "/" + f.preferred,
		Version:      f.preferred,
	}
	return &metav1.APIGroupList{Groups: []metav1.APIGroup{g}}, nil
}

func (f *fakeDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	version := strings.TrimPrefix(groupVersion, vmGroup+"/")
	if f.resourcesErrFor[version] {
		return nil, errors.New("the server could not find the requested resource")
	}
	list := &metav1.APIResourceList{GroupVersion: groupVersion}
	if f.servesResource[version] {
		list.APIResources = append(list.APIResources, metav1.APIResource{Name: vmResource, Kind: "VirtualMachine"})
	}
	// Something unrelated is always present, so "found the group version
	// but not the resource" is distinguishable from "found nothing".
	list.APIResources = append(list.APIResources, metav1.APIResource{Name: "virtualmachineimages", Kind: "VirtualMachineImage"})
	return list, nil
}

func TestResolveVMGVR(t *testing.T) {
	// The five versions a VCF 9.x Supervisor serves side by side.
	all := []string{"v1alpha1", "v1alpha2", "v1alpha3", "v1alpha4", "v1alpha5"}
	serves := func(vs ...string) map[string]bool {
		m := map[string]bool{}
		for _, v := range vs {
			m[v] = true
		}
		return m
	}

	tests := []struct {
		name        string
		discovery   *fakeDiscovery
		wantVersion string
		wantErr     string
	}{
		{
			name:        "prefers the newest served version",
			discovery:   &fakeDiscovery{versions: all, preferred: "v1alpha5", servesResource: serves(all...)},
			wantVersion: "v1alpha5",
		},
		{
			// The reason this is discovered rather than pinned: older
			// versions get retired, and a pinned one silently matches
			// no VMs once it's gone.
			name:        "an older Supervisor without the newest versions",
			discovery:   &fakeDiscovery{versions: []string{"v1alpha1", "v1alpha2"}, preferred: "v1alpha2", servesResource: serves("v1alpha1", "v1alpha2")},
			wantVersion: "v1alpha2",
		},
		{
			name:        "falls through when the preferred version doesn't serve the resource",
			discovery:   &fakeDiscovery{versions: all, preferred: "v1alpha5", servesResource: serves("v1alpha3")},
			wantVersion: "v1alpha3",
		},
		{
			name:        "falls through when listing a version's resources errors",
			discovery:   &fakeDiscovery{versions: all, preferred: "v1alpha5", servesResource: serves(all...), resourcesErrFor: map[string]bool{"v1alpha5": true}},
			wantVersion: "v1alpha1",
		},
		{
			name:        "v1alpha1 only, which vmReady has a fallback for",
			discovery:   &fakeDiscovery{versions: []string{"v1alpha1"}, preferred: "v1alpha1", servesResource: serves("v1alpha1")},
			wantVersion: "v1alpha1",
		},
		{
			name:      "VM Service not enabled",
			discovery: &fakeDiscovery{},
			wantErr:   "not found",
		},
		{
			name:      "discovery unavailable",
			discovery: &fakeDiscovery{groupsErr: errors.New("connection refused")},
			wantErr:   "listing server groups",
		},
		{
			name:      "group advertised but no version serves virtualmachines",
			discovery: &fakeDiscovery{versions: all, preferred: "v1alpha5", servesResource: serves()},
			wantErr:   "no served version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveVMGVR(tt.discovery)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %v", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got %q", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Version != tt.wantVersion {
				t.Errorf("version: got %q, want %q", got.Version, tt.wantVersion)
			}
			if got.Group != vmGroup || got.Resource != vmResource {
				t.Errorf("got %v, want group %q resource %q", got, vmGroup, vmResource)
			}
		})
	}
}

// The version resolveVMGVR settles on decides which status shape the
// controller reads back, so vmReady has to cope with every served one.
func TestVMReadyAcrossAPIVersions(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]interface{}
		wantIP string
		wantOK bool
	}{
		{
			name:   "v1alpha2 through v1alpha5 status.network",
			status: map[string]interface{}{"powerState": "PoweredOn", "network": map[string]interface{}{"primaryIP4": "10.0.0.5"}},
			wantIP: "10.0.0.5",
			wantOK: true,
		},
		{
			// v1alpha1 spells the power state lowercase and reports a
			// single flat IP with no network block.
			name:   "v1alpha1 status.vmIp",
			status: map[string]interface{}{"powerState": "poweredOn", "vmIp": "10.0.0.6"},
			wantIP: "10.0.0.6",
			wantOK: true,
		},
		{
			name:   "IPv6 only",
			status: map[string]interface{}{"powerState": "PoweredOn", "network": map[string]interface{}{"primaryIP6": "fd00::5"}},
			wantIP: "fd00::5",
			wantOK: true,
		},
		{
			name:   "powered on but no IP reported yet",
			status: map[string]interface{}{"powerState": "PoweredOn", "network": map[string]interface{}{}},
		},
		{
			name:   "powered off is never ready, whatever IP is left in status",
			status: map[string]interface{}{"powerState": "PoweredOff", "network": map[string]interface{}{"primaryIP4": "10.0.0.5"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := &unstructured.Unstructured{Object: map[string]interface{}{"status": tt.status}}
			ip, ok := vmReady(vm)
			if ok != tt.wantOK || ip != tt.wantIP {
				t.Errorf("got (%q, %v), want (%q, %v)", ip, ok, tt.wantIP, tt.wantOK)
			}
		})
	}
}
