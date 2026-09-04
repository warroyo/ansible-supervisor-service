package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// StatusFieldManager owns the generic state/message/ready/lastUpdated
// fields that every CRD in this service exposes. AnsibleBinding's
// per-VM details are written under a different field manager (see
// detailsFieldManager in ansiblebinding.go) so the two patches
// merge via server-side apply instead of one clobbering the other.
const StatusFieldManager = "status-controller"

const numWorkers = 2

// StatusUpdater computes the generic status fields for a resource, given
// whether provisioning just succeeded and any error it returned.
type StatusUpdater func(*unstructured.Unstructured, bool, error) map[string]interface{}

// Controller is a generic reconcile loop for one CRD kind: fetch by key,
// manage a cleanup finalizer, call provisionFunc/cleanupFunc, patch
// status. Every kind this service manages (AWXConnection,
// AnsibleBinding) is driven by one of these.
type Controller struct {
	client *dynamic.DynamicClient
	gvr    schema.GroupVersionResource
	// finalizerName is the finalizer this controller manages. Empty means
	// this kind needs no finalizer - nothing outside Kubernetes is
	// created for it, so there is nothing to clean up before deletion.
	finalizerName string
	// staleFinalizers are finalizers this controller used to set and no
	// longer does. They are stripped on sight, so resources created by an
	// older version of the controller stay deletable after an upgrade.
	staleFinalizers  []string
	provisionFunc    func(context.Context, *dynamic.DynamicClient, interface{}) error
	cleanupFunc      func(context.Context, *dynamic.DynamicClient, interface{}) error
	updateStatusFunc StatusUpdater

	Queue workqueue.RateLimitingInterface
}

// errCleanupPending wraps a finalization failure. The workqueue never
// gives up on one: dropping a key mid-finalization leaves the resource
// stuck in Terminating with its external state leaked, and unlike a
// normal reconcile a deleted object may get no further events to
// rediscover it from.
type errCleanupPending struct{ err error }

func (e *errCleanupPending) Error() string { return e.err.Error() }
func (e *errCleanupPending) Unwrap() error { return e.err }

func isCleanupPending(err error) bool {
	var e *errCleanupPending
	return errors.As(err, &e)
}

// updateGenericStatus is the default StatusUpdater: it only ever writes
// state/message/ready/lastUpdated, so it's safe to share across CRDs that
// also maintain their own extra status fields under a different field
// manager.
func updateGenericStatus(u *unstructured.Unstructured, success bool, reconcileErr error) map[string]interface{} {
	if reconcileErr != nil {
		return map[string]interface{}{
			"state":       "Failed",
			"message":     fmt.Sprintf("Reconciliation failed: %s", reconcileErr.Error()),
			"ready":       false,
			"lastUpdated": metav1.Now(),
		}
	}
	if success {
		return map[string]interface{}{
			"state":       "Ready",
			"message":     "Resource provisioned successfully.",
			"ready":       true,
			"lastUpdated": metav1.Now(),
		}
	}
	return map[string]interface{}{
		"state":       "Pending",
		"message":     "Provisioning not yet run.",
		"ready":       false,
		"lastUpdated": metav1.Now(),
	}
}

// genericStatusCurrent reports whether the object already carries the
// state/message/ready this pass computed.
//
// lastUpdated is deliberately not compared: it is set to now on every
// call, so comparing it would make every reconcile look like a change.
// Skipping the apply when the rest matches is what stops an idle
// resource writing to etcd once per resync forever - server-side apply
// would collapse to a no-op anyway, but only after the round trip.
func genericStatusCurrent(u *unstructured.Unstructured, statusMap map[string]interface{}) bool {
	for _, field := range []string{"state", "message"} {
		want, _ := statusMap[field].(string)
		got, found, err := unstructured.NestedString(u.Object, "status", field)
		if err != nil || !found || got != want {
			return false
		}
	}
	want, _ := statusMap["ready"].(bool)
	got, found, err := unstructured.NestedBool(u.Object, "status", "ready")
	if err != nil || !found {
		return false
	}
	return got == want
}

// key formats a namespace/name pair the way the workqueue does.
func key(namespace, name string) string { return namespace + "/" + name }

func toUnstructured(obj interface{}) (*unstructured.Unstructured, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u, nil
	}
	return nil, fmt.Errorf("object is not an unstructured object")
}

// patchStatus server-side-applies statusData under fieldManager. Different
// field managers can own disjoint sets of status fields on the same
// object without clobbering each other.
func patchStatus(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, statusData map[string]interface{}, fieldManager string) error {
	statusObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": obj.GetAPIVersion(),
			"kind":       obj.GetKind(),
			"metadata": map[string]interface{}{
				"name":      obj.GetName(),
				"namespace": obj.GetNamespace(),
			},
			"status": statusData,
		},
	}

	const maxRetries = 5
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		_, err := client.Resource(gvr).Namespace(obj.GetNamespace()).ApplyStatus(
			ctx,
			statusObj.GetName(),
			statusObj,
			metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
		)
		if err == nil {
			return nil
		}
		if apierrors.IsNotFound(err) {
			log.Printf("Warning: skipped status patch for deleted resource %s/%s\n", obj.GetNamespace(), obj.GetName())
			return nil
		}
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			lastErr = err
			log.Printf("Status apply conflict (%d/%d) on %s/%s. Retrying...\n", i+1, maxRetries, obj.GetNamespace(), obj.GetName())
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return fmt.Errorf("failed to apply status for %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return fmt.Errorf("failed to apply status for %s/%s after %d attempts: %w", obj.GetNamespace(), obj.GetName(), maxRetries, lastErr)
}

// patchFinalizer adds or removes the finalizer using a JSON Merge Patch.
// A merge patch on metadata.finalizers must carry the whole list, since
// the array is replaced wholesale - which means a list read even
// slightly out of date would silently drop another controller's
// finalizer. Sending resourceVersion alongside it makes the API server
// reject the patch with a conflict instead, and the reconcile retries
// against a fresh read.
func patchFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, finalizerName string, finalizers []string) error {
	patchPayload := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers":      finalizers,
			"resourceVersion": obj.GetResourceVersion(),
		},
	}
	patchData, err := json.Marshal(patchPayload)
	if err != nil {
		return fmt.Errorf("marshaling finalizer patch: %w", err)
	}
	_, err = client.Resource(gvr).Namespace(obj.GetNamespace()).Patch(
		ctx, obj.GetName(), types.MergePatchType, patchData, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patching finalizer %s on %s/%s: %w", finalizerName, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

func containsFinalizer(obj *unstructured.Unstructured, finalizerName string) bool {
	for _, f := range obj.GetFinalizers() {
		if f == finalizerName {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return
}

// desiredFinalizers is metadata.finalizers as this controller wants it:
// its own finalizer present, and any finalizer it no longer manages
// removed. Returns false if the list is already correct.
func (c *Controller) desiredFinalizers(u *unstructured.Unstructured) ([]string, bool) {
	desired := u.GetFinalizers()
	changed := false
	for _, stale := range c.staleFinalizers {
		if containsFinalizer(u, stale) {
			desired = removeString(desired, stale)
			changed = true
		}
	}
	if c.finalizerName != "" && !containsFinalizer(u, c.finalizerName) {
		desired = append(desired, c.finalizerName)
		changed = true
	}
	return desired, changed
}

func (c *Controller) Reconcile(ctx context.Context, obj interface{}) (reconcileResult error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return fmt.Errorf("error converting to unstructured: %w", err)
	}

	name := u.GetName()
	namespace := u.GetNamespace()
	kind := u.GetKind()
	logPrefix := fmt.Sprintf("[%s/%s/%s]", kind, namespace, name)

	var reconcileErr error
	defer func() {
		if reconcileErr != nil {
			log.Printf("%s DEFER STATUS UPDATE: patching status after error: %v\n", logPrefix, reconcileErr)

			latestU, getErr := c.client.Resource(c.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				if apierrors.IsNotFound(getErr) {
					log.Printf("%s Status patch skipped: resource was deleted during defer execution.\n", logPrefix)
					reconcileResult = reconcileErr
					return
				}
				log.Printf("%s CRITICAL: failed to re-fetch object for status patch: %v. Returning original error.\n", logPrefix, getErr)
				reconcileResult = reconcileErr
				return
			}

			// An error that repeats every pass - AWX down, a bad template
			// name - would otherwise rewrite the same message forever.
			statusMap := c.updateStatusFunc(latestU, false, reconcileErr)
			if !genericStatusCurrent(latestU, statusMap) {
				if statusPatchErr := patchStatus(ctx, c.client, c.gvr, latestU, statusMap, StatusFieldManager); statusPatchErr != nil {
					if !apierrors.IsNotFound(statusPatchErr) && !apierrors.IsConflict(statusPatchErr) {
						log.Printf("%s CRITICAL: failed to patch status after error: %v\n", logPrefix, statusPatchErr)
					}
				}
			}

			reconcileResult = reconcileErr
		}
	}()

	if !u.GetDeletionTimestamp().IsZero() {
		log.Printf("%s DeletionTimestamp detected. Initiating finalization.\n", logPrefix)

		// Everything this controller holds on the object comes off in one
		// patch: its own finalizer once cleanup has actually succeeded,
		// plus any finalizer an older version left behind.
		remaining := u.GetFinalizers()
		releasing := false

		if c.finalizerName != "" && containsFinalizer(u, c.finalizerName) {
			log.Printf("%s Finalizer %s is present. Starting cleanup...\n", logPrefix, c.finalizerName)

			if c.cleanupFunc != nil {
				if cleanupErr := c.cleanupFunc(ctx, c.client, obj); cleanupErr != nil {
					log.Printf("%s CLEANUP FAILED: %v. Will retry.\n", logPrefix, cleanupErr)
					reconcileErr = &errCleanupPending{fmt.Errorf("cleanup failed: %w", cleanupErr)}
					return reconcileErr
				}
			}
			remaining = removeString(remaining, c.finalizerName)
			releasing = true
		}
		for _, stale := range c.staleFinalizers {
			if containsFinalizer(u, stale) {
				remaining = removeString(remaining, stale)
				releasing = true
			}
		}

		if !releasing {
			log.Printf("%s Finalizer not present. Deletion complete/in progress by Kubernetes.\n", logPrefix)
			return nil
		}

		if err := patchFinalizer(ctx, c.client, c.gvr, u, c.finalizerName, remaining); err != nil {
			log.Printf("%s ERROR patching to remove finalizer: %v\n", logPrefix, err)
			reconcileErr = &errCleanupPending{fmt.Errorf("finalizer removal patch failed: %w", err)}
			return reconcileErr
		}
		log.Printf("%s Finalizer(s) removed successfully. Deletion will now complete.\n", logPrefix)
		return nil
	}

	if desired, changed := c.desiredFinalizers(u); changed {
		log.Printf("%s Updating finalizers to %v.\n", logPrefix, desired)

		if err := patchFinalizer(ctx, c.client, c.gvr, u, c.finalizerName, desired); err != nil {
			log.Printf("%s ERROR patching finalizers: %v\n", logPrefix, err)
			reconcileErr = fmt.Errorf("finalizer patch failed: %w", err)
			return reconcileErr
		}

		statusMap := c.updateStatusFunc(u, false, nil)
		if statusPatchErr := patchStatus(ctx, c.client, c.gvr, u, statusMap, StatusFieldManager); statusPatchErr != nil {
			log.Printf("%s Warning: failed to patch status after adding finalizer: %v\n", logPrefix, statusPatchErr)
		}

		// The finalizer has to be in place before anything external is
		// created, and the copy in hand predates the patch. Re-read
		// rather than provisioning against it - and rather than waiting
		// for the resulting event, which is a metadata-only change the
		// informer's update filter deliberately drops.
		refreshed, getErr := c.client.Resource(c.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				log.Printf("%s Resource was deleted right after its finalizers were set.\n", logPrefix)
				return nil
			}
			reconcileErr = fmt.Errorf("re-fetching %s after the finalizer patch: %w", key(namespace, name), getErr)
			return reconcileErr
		}
		u = refreshed
		obj = refreshed
		log.Printf("%s Finalizers set. Continuing from the updated object.\n", logPrefix)
	}

	log.Printf("%s Running normal reconciliation.\n", logPrefix)
	if provisionErr := c.provisionFunc(ctx, c.client, obj); provisionErr != nil {
		reconcileErr = fmt.Errorf("provisioning failed: %w", provisionErr)
		return reconcileErr // defer handles the status update and returns the error for retry
	}

	// Re-read before computing the aggregate status: provisioning writes
	// the detail fields a StatusUpdater derives state from (per-VM run
	// outcomes, say), and the copy this reconcile started from predates
	// them.
	latestU, getErr := c.client.Resource(c.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			log.Printf("%s Status patch skipped: resource was deleted during reconciliation.\n", logPrefix)
			return nil
		}
		return fmt.Errorf("re-fetching %s for status: %w", key(namespace, name), getErr)
	}

	statusMap := c.updateStatusFunc(latestU, true, nil)
	if !genericStatusCurrent(latestU, statusMap) {
		if statusPatchErr := patchStatus(ctx, c.client, c.gvr, latestU, statusMap, StatusFieldManager); statusPatchErr != nil {
			log.Printf("%s Warning: failed to patch status after successful provisioning: %v. Requeuing...\n", logPrefix, statusPatchErr)
			return statusPatchErr
		}
	}

	log.Printf("%s Reconciliation complete and status updated.\n", logPrefix)
	return nil
}

func setupInformer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, controller *Controller, resyncPeriod time.Duration) cache.SharedIndexInformer {
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return client.Resource(gvr).Namespace("").List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return client.Resource(gvr).Namespace("").Watch(ctx, options)
			},
		},
		&unstructured.Unstructured{},
		resyncPeriod,
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				log.Printf("--- %s ADD event. queuing %s ---\n", controller.gvr.Resource, key)
				controller.Queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldU, err := toUnstructured(oldObj)
			if err != nil {
				log.Printf("Error converting old object for update filter: %v\n", err)
				return
			}
			newU, err := toUnstructured(newObj)
			if err != nil {
				log.Printf("Error converting new object for update filter: %v\n", err)
				return
			}

			generationChanged := oldU.GetGeneration() != newU.GetGeneration()
			deletionRequested := !newU.GetDeletionTimestamp().IsZero()
			isResync := oldU.GetResourceVersion() == newU.GetResourceVersion()
			annotationsChanged := oldU.GetAnnotations()[ReconcileRequestedAtAnnotation] != newU.GetAnnotations()[ReconcileRequestedAtAnnotation]

			if generationChanged || deletionRequested || isResync || annotationsChanged {
				key, err := cache.MetaNamespaceKeyFunc(newObj)
				if err == nil {
					controller.Queue.Add(key)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				log.Printf("--- %s DELETE event. queuing %s ---\n", controller.gvr.Resource, key)
				controller.Queue.Add(key)
			}
		},
	})
	return informer
}
