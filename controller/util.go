package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
var nsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

const vmGroup = "vmoperator.vmware.com"
const vmResource = "virtualmachines"

// vmGVR targets VM Service VirtualMachines. VM Operator serves several
// versions of this CRD side by side (v1alpha1 through v1alpha5 on VCF
// 9.x) and retires the oldest as it adds new ones, so the version is
// discovered at startup by resolveVMGVR rather than compiled in - a
// pinned version becomes a silent "no VMs ever match" the day it stops
// being served. This value is only the fallback if discovery fails.
var vmGVR = schema.GroupVersionResource{Group: vmGroup, Version: "v1alpha2", Resource: vmResource}

// versionDiscoverer is the slice of discovery.DiscoveryInterface
// resolveVMGVR needs, kept narrow so it can be faked in tests.
type versionDiscoverer interface {
	ServerGroups() (*metav1.APIGroupList, error)
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// resolveVMGVR picks a served version of the VirtualMachine resource,
// preferring the API server's own preferred version (the newest, which
// for a CRD is what the storage version sorts to) and falling back
// through the rest.
//
// Reading VMs across versions is safe because the controller only ever
// reads labels and a couple of status fields, and the API server
// converts between served versions - it never writes a VM, so no version
// carries a field it could drop. The one genuine difference is v1alpha1's
// flat status.vmIp, which vmReady handles.
func resolveVMGVR(d versionDiscoverer) (schema.GroupVersionResource, error) {
	groups, err := d.ServerGroups()
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("listing server groups: %w", err)
	}

	var candidates []string
	for _, g := range groups.Groups {
		if g.Name != vmGroup {
			continue
		}
		// PreferredVersion first, then everything else in the order the
		// API server returned it, which is already priority-sorted.
		if g.PreferredVersion.Version != "" {
			candidates = append(candidates, g.PreferredVersion.Version)
		}
		for _, v := range g.Versions {
			if v.Version != g.PreferredVersion.Version {
				candidates = append(candidates, v.Version)
			}
		}
	}
	if len(candidates) == 0 {
		return schema.GroupVersionResource{}, fmt.Errorf("api group %q not found: is VM Service enabled on this supervisor?", vmGroup)
	}

	// A version being in the group list doesn't guarantee it serves this
	// particular resource, so confirm before settling on one.
	for _, version := range candidates {
		gv := schema.GroupVersion{Group: vmGroup, Version: version}
		resources, err := d.ServerResourcesForGroupVersion(gv.String())
		if err != nil {
			continue
		}
		for _, r := range resources.APIResources {
			if r.Name == vmResource {
				return gv.WithResource(vmResource), nil
			}
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("no served version of %s.%s found (tried %v)", vmResource, vmGroup, candidates)
}

var awxConnGVR = schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "awxconnections"}
var ansBindGVR = schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "ansiblebindings"}
var ansBindVMGVR = schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "ansiblebindingvms"}

// ReconcileRequestedAtAnnotation is the annotation a user bumps to force
// a re-run of an AnsibleBinding that's already up to date
// (controller-runtime/Flux "reconcile requested at" convention).
const ReconcileRequestedAtAnnotation = "ansible.field.vmware.com/reconcile-requested-at"

// errPermanentConfig marks an error that retrying cannot fix: the
// referenced object exists but is malformed. Deletion paths use it to
// tell "come back later" apart from "this will never succeed", so a
// transient outage never causes an AWX host to be abandoned.
var errPermanentConfig = errors.New("permanent configuration error")

// isPermanent reports whether err is one retrying will never resolve:
// either a referenced object is genuinely gone, or it is malformed.
func isPermanent(err error) bool {
	return apierrors.IsNotFound(err) || errors.Is(err, errPermanentConfig)
}

// hostMarkerPrefix identifies AWX inventory hosts managed by some
// ansible-supervisor deployment. The full marker also names the
// supervisor and the owning AnsibleBinding, and lives in the AWX
// host's description field - the only free-text field on an AWX Host
// (there are no labels or tags), and unlike host variables it never
// leaks into playbooks. Keeping ownership in AWX rather than only in CR
// status means it survives the CR being deleted and recreated, and lets
// one AWX instance be shared by several supervisors safely.
const hostMarkerPrefix = "ansible-supervisor:"

// supervisorID identifies this supervisor in host ownership markers. Set
// once at startup: either the operator-supplied value or the kube-system
// namespace UID, which is stable and unique per cluster.
var supervisorID string

func hostOwnerMarker(namespace, name string) string {
	return fmt.Sprintf("%s%s:%s/%s", hostMarkerPrefix, supervisorID, namespace, name)
}

// resolveSupervisorID returns the configured identity, falling back to
// the kube-system namespace UID.
func resolveSupervisorID(ctx context.Context, client *dynamic.DynamicClient, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	ns, err := client.Resource(nsGVR).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("deriving a supervisor id from the kube-system namespace UID (set --supervisor-id to skip this): %w", err)
	}
	uid := string(ns.GetUID())
	if uid == "" {
		return "", fmt.Errorf("kube-system namespace has no UID to derive a supervisor id from")
	}
	return uid, nil
}

func convertAWXConnection(u *unstructured.Unstructured) (AWXConnection, error) {
	var c AWXConnection
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &c)
	return c, err
}

func convertAnsibleBinding(u *unstructured.Unstructured) (AnsibleBinding, error) {
	var c AnsibleBinding
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &c)
	return c, err
}

func convertAnsibleBindingVM(u *unstructured.Unstructured) (AnsibleBindingVM, error) {
	var c AnsibleBindingVM
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &c)
	return c, err
}

// hostCheckPeriod is how often each child reconciles its AWX inventory
// host against AWX itself. It is the worst case for repairing a host
// deleted or edited by hand in the AWX UI, and - since everything else
// in a steady-state pass is now a cache read - it is also the only thing
// setting the controller's AWX request rate. Set at startup from
// --host-check-period.
var hostCheckPeriod = 10 * time.Minute

// orphanScanPeriod is how often a binding lists the AWX hosts it owns
// looking for ones no child claims. Deliberately several host-check
// periods: a leaked host comes from a controller killed mid-cleanup,
// which is far rarer than a host edited by hand in the AWX UI, and the
// scan is one AWX request per binding rather than per VM. Derived rather
// than configured so there is still only one period a user ever sees.
func orphanScanPeriod() time.Duration { return 4 * hostCheckPeriod }

// dueFor reports whether at least period has elapsed since the RFC3339
// timestamp ts, and how long is left if it has not. An empty or
// unparseable timestamp means the work has never been done, which is
// always due.
func dueFor(ts string, period time.Duration) (due bool, remaining time.Duration) {
	if ts == "" {
		return true, 0
	}
	last, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true, 0
	}
	elapsed := time.Since(last)
	if elapsed >= period {
		return true, 0
	}
	// A clock that has gone backwards (or a timestamp written by a
	// controller whose clock was ahead) would otherwise park the work
	// arbitrarily far into the future.
	if elapsed < 0 {
		return true, 0
	}
	return false, period - elapsed
}

// Child names are DNS subdomains, so 253 characters is the hard ceiling.
// Each half gets a bounded share of it and the hash below takes the rest.
const (
	childNameHalfLimit = 110
	childNameHashLen   = 10
)

// childName is the deterministic name of the AnsibleBindingVM a binding
// creates for one VM. Deterministic so the parent can create it blind
// and let AlreadyExists mean "nothing to do", rather than listing first
// and racing with its own previous pass.
//
// Plain concatenation was ambiguous - binding "a-b" with VM "c" and
// binding "a" with VM "b-c" produce the same string - and unbounded, so
// a long binding name plus a long VM name simply failed to create. The
// hash of the exact pair resolves both: it disambiguates the join, and
// it stays stable when the readable halves are truncated. It is the same
// construction EndpointSlice and Job-owned Pod names use.
//
// generateName would also have solved it, but it makes creation
// non-idempotent: a create whose response is lost leaks a duplicate
// child the next pass cannot recognise.
func childName(bindingName, vmName string) string {
	sum := sha256.Sum256([]byte(bindingName + "/" + vmName))
	suffix := hex.EncodeToString(sum[:])[:childNameHashLen]
	return clipNameSegment(bindingName) + "-" + clipNameSegment(vmName) + "-" + suffix
}

// clipNameSegment bounds one half of a child name and makes sure the
// truncation cannot leave a character a DNS subdomain may not end on.
func clipNameSegment(s string) string {
	if len(s) > childNameHalfLimit {
		s = s[:childNameHalfLimit]
	}
	return strings.TrimRight(s, "-.")
}

// getSecretValue reads a base64-encoded key out of a Secret's data map.
// Read via the dynamic client, Secret data values arrive base64-encoded
// as-is (unlike the typed corev1.Secret client, which decodes them for
// you), so this decodes explicitly.
func getSecretValue(ctx context.Context, client *dynamic.DynamicClient, namespace, name, key string) (string, error) {
	secret, err := client.Resource(secretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("fetching secret %q: %w", name, err)
	}
	encoded, found, err := unstructured.NestedString(secret.Object, "data", key)
	if err != nil {
		return "", err
	}
	if !found || encoded == "" {
		return "", fmt.Errorf("secret %q has no data key %q: %w", name, key, errPermanentConfig)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding secret %q key %q: %w: %w", name, key, err, errPermanentConfig)
	}
	// Trim surrounding whitespace: the value goes straight into an
	// Authorization header, and a token stored the obvious way (echo
	// "$TOKEN" > token, or a heredoc) carries a trailing newline. Go's
	// net/http rejects that with `invalid header field value for
	// "Authorization"`, which names the header and not the newline.
	trimmed := strings.TrimSpace(string(decoded))
	if trimmed == "" {
		return "", fmt.Errorf("secret %q key %q is empty: %w", name, key, errPermanentConfig)
	}
	return trimmed, nil
}

// structToMap round-trips v through JSON so it can be embedded in an
// unstructured status patch.
//
// Whole numbers come back as int64, not the float64 encoding/json would
// hand back by default. An unstructured object is meant to hold int64,
// and the accessors enforce it: a float64 in status.lastJobID reads back
// as zero through NestedInt64, and makes the runtime converter refuse
// the object outright.
func structToMap(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	normalized, ok := normalizeJSONNumbers(m).(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("encoding %T: expected a JSON object", v)
	}
	return normalized, nil
}

// normalizeJSONNumbers walks a decoded JSON value turning every
// json.Number into the int64 or float64 an unstructured object may hold.
func normalizeJSONNumbers(v interface{}) interface{} {
	switch typed := v.(type) {
	case map[string]interface{}:
		for k, inner := range typed {
			typed[k] = normalizeJSONNumbers(inner)
		}
		return typed
	case []interface{}:
		for i, inner := range typed {
			typed[i] = normalizeJSONNumbers(inner)
		}
		return typed
	case json.Number:
		if i, err := typed.Int64(); err == nil {
			return i
		}
		f, err := typed.Float64()
		if err != nil {
			return typed.String()
		}
		return f
	default:
		return v
	}
}

func listVirtualMachines(ctx context.Context, client *dynamic.DynamicClient, namespace string, selector map[string]string) ([]unstructured.Unstructured, error) {
	listOpts := metav1.ListOptions{}
	if len(selector) > 0 {
		listOpts.LabelSelector = labels.SelectorFromSet(selector).String()
	}
	list, err := client.Resource(vmGVR).Namespace(namespace).List(ctx, listOpts)
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// vmReady reports whether a VM Service VirtualMachine is powered on and
// has reported a guest IP - the same precondition AWX itself needs before
// it can SSH in.
func vmReady(vm *unstructured.Unstructured) (ip string, ready bool) {
	powerState, _, _ := unstructured.NestedString(vm.Object, "status", "powerState")
	if !strings.EqualFold(powerState, "PoweredOn") {
		return "", false
	}
	if v, found, _ := unstructured.NestedString(vm.Object, "status", "network", "primaryIP4"); found && v != "" {
		return v, true
	}
	if v, found, _ := unstructured.NestedString(vm.Object, "status", "network", "primaryIP6"); found && v != "" {
		return v, true
	}
	// v1alpha1 reported a single flat status.vmIp; the status.network
	// block replaced it in v1alpha2 and is unchanged through v1alpha5.
	// Only reached on a Supervisor old enough that v1alpha1 is all
	// resolveVMGVR had to choose from.
	if v, found, _ := unstructured.NestedString(vm.Object, "status", "vmIp"); found && v != "" {
		return v, true
	}
	return "", false
}

// upsertHistory records a run in a VM's history, bounded to
// historyLimit. An entry for a job already in the history is updated in
// place (a run produces one entry that gains a status and finish time,
// not one entry per state transition).
func upsertHistory(existing []VMRunHistoryEntry, entry VMRunHistoryEntry) []VMRunHistoryEntry {
	updated := append([]VMRunHistoryEntry(nil), existing...)
	for i := range updated {
		if updated[i].JobID == entry.JobID {
			if entry.Status != "" {
				updated[i].Status = entry.Status
			}
			if entry.StartedAt != "" {
				updated[i].StartedAt = entry.StartedAt
			}
			if entry.FinishedAt != "" {
				updated[i].FinishedAt = entry.FinishedAt
			}
			return updated
		}
	}
	updated = append([]VMRunHistoryEntry{entry}, updated...)
	if len(updated) > historyLimit {
		updated = updated[:historyLimit]
	}
	return updated
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// mapAWXStatus collapses AWX's job status vocabulary down to our four
// VM run phases.
func mapAWXStatus(status string) string {
	switch status {
	case "successful":
		return PhaseSucceeded
	case "failed", "error", "canceled":
		return PhaseFailed
	default: // "new", "pending", "waiting", "running"
		return PhaseRunning
	}
}

// isTerminalAWXStatus reports whether an AWX job has finished. Run
// tracking keys off this rather than off our own phase, which can be
// overwritten by VM-level conditions (a VM powering off mid-run must not
// lose track of the job that's still executing).
func isTerminalAWXStatus(status string) bool {
	switch status {
	case "successful", "failed", "error", "canceled":
		return true
	}
	return false
}

func pollJobStatus(ctx context.Context, client *AWXClient, templateType string, jobID int64) (string, error) {
	if templateType == TemplateTypeWorkflow {
		return client.GetWorkflowJobStatus(ctx, int(jobID))
	}
	return client.GetJobStatus(ctx, int(jobID))
}
