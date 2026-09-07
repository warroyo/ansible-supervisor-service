package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var eventGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}

// eventSource names this controller in the events it emits.
const eventSource = "ansible-supervisor"

// Event types, as Kubernetes defines them.
const (
	eventNormal  = "Normal"
	eventWarning = "Warning"
)

// recordEvent writes one Kubernetes Event against involved.
//
// This exists because of what deprovision does at the end: it releases
// the finalizer, and the object - along with the status describing what
// the teardown playbook did - is then deleted. An Event is the standard
// record of something that happened to an object that no longer exists,
// and unlike a log line it is visible to whoever ran kubectl delete.
//
// Best effort by design. Failing to record what happened must never be
// the reason a resource cannot finish deleting, so every failure here is
// logged and swallowed.
func recordEvent(ctx context.Context, client dynamic.Interface, involved *unstructured.Unstructured, eventType, reason, message string) {
	if involved == nil {
		return
	}
	namespace := involved.GetNamespace()
	now := metav1.Now()
	event := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata": map[string]interface{}{
			// A name per event rather than the aggregating
			// name.timestamp convention: these are one-shot records of a
			// teardown, not a repeating condition to be counted.
			"generateName": strings.ToLower(involved.GetKind()) + "-" + involved.GetName() + ".",
			"namespace":    namespace,
		},
		"involvedObject": map[string]interface{}{
			"apiVersion": involved.GetAPIVersion(),
			"kind":       involved.GetKind(),
			"name":       involved.GetName(),
			"namespace":  namespace,
			"uid":        string(involved.GetUID()),
		},
		"reason":         reason,
		"message":        message,
		"type":           eventType,
		"firstTimestamp": now.UTC().Format("2006-01-02T15:04:05Z"),
		"lastTimestamp":  now.UTC().Format("2006-01-02T15:04:05Z"),
		"count":          int64(1),
		"source":         map[string]interface{}{"component": eventSource},
	}}

	if _, err := client.Resource(eventGVR).Namespace(namespace).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		log.Printf("[%s/%s/%s] could not record event %s: %v", involved.GetKind(), namespace, involved.GetName(), reason, err)
	}
}

// eventTarget picks the object a teardown record should hang off.
//
// The child is about to be deleted, so an event against it is orphaned
// the moment the finalizer comes off: kubectl get events still lists it
// until the cluster's event TTL expires, but kubectl describe on the
// child shows nothing, because there is no child. The parent
// AnsibleBinding usually outlives the VM - a kubectl delete vm, an Argo
// prune, a scale-in - so the record hangs there instead, where
// describing the binding shows what happened to a VM under it.
//
// The parent is not always there: deleting the binding deletes its
// children, and deleting a namespace takes everything at once. Then the
// child is the best available target, and the controller log is the only
// record that survives the namespace.
func eventTarget(ctx context.Context, client dynamic.Interface, child *unstructured.Unstructured, bindingName string) *unstructured.Unstructured {
	if bindingName == "" {
		return child
	}
	binding, err := client.Resource(ansBindGVR).Namespace(child.GetNamespace()).Get(ctx, bindingName, metav1.GetOptions{})
	if err != nil || binding == nil {
		return child
	}
	if !binding.GetDeletionTimestamp().IsZero() {
		// Going away itself, so an event on it is no more durable than
		// one on the child, and the child at least names the VM.
		return child
	}
	return binding
}

// hookOutcomeMessage is the one-line summary that goes into the log, the
// event and the child's status alike, so the three cannot disagree.
func hookOutcomeMessage(vmName string, st *DeprovisionStatus) string {
	if st == nil {
		return fmt.Sprintf("Deprovision hook for VirtualMachine %q did not run.", vmName)
	}
	outcome := map[string]string{
		PhaseSucceeded: "succeeded",
		PhaseFailed:    "failed",
		PhaseTimedOut:  "timed out",
		PhaseSkipped:   "did not run",
	}[st.Phase]
	if outcome == "" {
		outcome = strings.ToLower(st.Phase)
	}
	msg := fmt.Sprintf("Deprovision hook for VirtualMachine %q %s", vmName, outcome)
	if st.JobID != 0 {
		msg += fmt.Sprintf(" (AWX job %d", st.JobID)
		if st.JobStatus != "" {
			msg += ", status " + st.JobStatus
		}
		if st.JobURL != "" {
			msg += ", " + st.JobURL
		}
		msg += ")"
	}
	if st.Message != "" {
		msg += ". " + st.Message
	}
	return msg
}
