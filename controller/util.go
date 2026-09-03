package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
var nsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

// vmGVR targets VM Service VirtualMachines. v1alpha2 is current as of
// vSphere Supervisor 8.0u2+; verify against the target Supervisor's
// installed vmoperator.vmware.com CRD version if VMs aren't found.
var vmGVR = schema.GroupVersionResource{Group: "vmoperator.vmware.com", Version: "v1alpha2", Resource: "virtualmachines"}

var awxConnGVR = schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "awxconnections"}
var ansBindGVR = schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "ansiblebindings"}

// ReconcileRequestedAtAnnotation is the annotation a user bumps to force
// a re-run of an AnsibleBinding that's already up to date
// (controller-runtime/Flux "reconcile requested at" convention).
const ReconcileRequestedAtAnnotation = "ansible.field.vmware.com/reconcile-requested-at"

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
func resolveSupervisorID(client *dynamic.DynamicClient, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	ns, err := client.Resource(nsGVR).Get(context.Background(), "kube-system", metav1.GetOptions{})
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

// getSecretValue reads a base64-encoded key out of a Secret's data map.
// Read via the dynamic client, Secret data values arrive base64-encoded
// as-is (unlike the typed corev1.Secret client, which decodes them for
// you), so this decodes explicitly.
func getSecretValue(client *dynamic.DynamicClient, namespace, name, key string) (string, error) {
	secret, err := client.Resource(secretGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("fetching secret %q: %w", name, err)
	}
	encoded, found, err := unstructured.NestedString(secret.Object, "data", key)
	if err != nil {
		return "", err
	}
	if !found || encoded == "" {
		return "", fmt.Errorf("secret %q has no data key %q", name, key)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding secret %q key %q: %w", name, key, err)
	}
	// Trim surrounding whitespace: the value goes straight into an
	// Authorization header, and a token stored the obvious way (echo
	// "$TOKEN" > token, or a heredoc) carries a trailing newline. Go's
	// net/http rejects that with `invalid header field value for
	// "Authorization"`, which names the header and not the newline.
	trimmed := strings.TrimSpace(string(decoded))
	if trimmed == "" {
		return "", fmt.Errorf("secret %q key %q is empty", name, key)
	}
	return trimmed, nil
}

// structToMap round-trips v through JSON so it can be embedded in an
// unstructured status patch.
func structToMap(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func listVirtualMachines(client *dynamic.DynamicClient, namespace string, selector map[string]string) ([]unstructured.Unstructured, error) {
	listOpts := metav1.ListOptions{}
	if len(selector) > 0 {
		listOpts.LabelSelector = labels.SelectorFromSet(selector).String()
	}
	list, err := client.Resource(vmGVR).Namespace(namespace).List(context.Background(), listOpts)
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

// hostVarsHash fingerprints what was last pushed to AWX for a host, so
// an unchanged host isn't re-PATCHed on every resync.
func hostVarsHash(hostName string, vars map[string]string) string {
	payload := struct {
		Host string            `json:"host"`
		Vars map[string]string `json:"vars"`
	}{hostName, vars}
	b, err := json.Marshal(payload) // map keys are marshaled sorted, so this is stable
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
