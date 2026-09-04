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

// numWorkers is the per-controller worker pool. Raised from 2 with the
// per-VM split: the work that used to be one long reconcile over N VMs
// is now N independent items, and the pool is what turns that into
// parallelism rather than a longer queue.
const numWorkers = 8

// StatusUpdater computes the generic status fields for a resource, given
// whether provisioning just succeeded and any error it returned.
type StatusUpdater func(*unstructured.Unstructured, bool, error) map[string]interface{}

// Result is what a provisionFunc hands back to the engine.
//
// Object, when set, is the resource as provisioning left it: the copy it
// was given, with the status it just wrote merged in. The engine derives
// the generic state/message/ready from that rather than re-reading the
// object from the API server, which is one round trip per resource per
// pass that bought nothing - provisioning already knows what it wrote.
//
// RequeueAfter, when non-zero, asks for another pass after that delay.
// The queue is a RateLimitingInterface, so AddAfter is already there;
// this is the only thing that was missing to let a reconcile say "come
// back in ten minutes" instead of needing its own resync period.
type Result struct {
	Object       *unstructured.Unstructured
	RequeueAfter time.Duration
}

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
	provisionFunc    func(context.Context, *dynamic.DynamicClient, interface{}) (Result, error)
	cleanupFunc      func(context.Context, *dynamic.DynamicClient, interface{}) error
	updateStatusFunc StatusUpdater

	// indexer is this kind's informer store. Reads below the workqueue
	// go through it rather than to the API server: the informer already
	// holds every object of this kind, kept current by watch, and a
	// reconcile that re-derives the world from it is exactly as
	// level-triggered as one that re-fetches - the guard against acting
	// on a stale read is the conflict on the write (resourceVersion on
	// the finalizer patch, retry in patchStatus), not the freshness of
	// the read.
	indexer cache.Indexer

	Queue workqueue.RateLimitingInterface
}

// cachedGet reads one object of this controller's kind from the informer
// store. A miss means the object is gone as far as this controller is
// concerned; there is nothing to reconcile and nothing to patch.
//
// The returned object is a deep copy: the store's copy is shared with
// every other reader of the informer and must never be mutated.
func (c *Controller) cachedGet(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	if c.indexer == nil {
		// No informer wired up (unit tests, and any future controller
		// that runs without one) - fall back to the API server.
		u, err := c.client.Resource(c.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return u, err
	}
	obj, exists, err := c.indexer.GetByKey(key(namespace, name))
	if err != nil {
		return nil, fmt.Errorf("reading %s from the informer cache: %w", key(namespace, name), err)
	}
	if !exists {
		return nil, nil
	}
	u, err := toUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return u.DeepCopy(), nil
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

// patchFinalizer rewrites metadata.finalizers, applying mutate to the
// list the object currently carries, using a JSON Merge Patch.
//
// A merge patch on metadata.finalizers must carry the whole list, since
// the array is replaced wholesale - which means a list read even
// slightly out of date would silently drop another controller's
// finalizer. Sending resourceVersion alongside it makes the API server
// reject the patch with a conflict instead.
//
// The object in hand is now read from the informer cache, so a conflict
// is an ordinary outcome rather than a rare one: retrying the pass would
// only bring back the same stale copy. On conflict this re-reads the
// object from the API server and applies the same intent to the list it
// actually has - which is why mutate is a function of the current list
// rather than a list computed by the caller.
func patchFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, mutate func([]string) []string) error {
	target := obj
	for attempt := 0; ; attempt++ {
		patchPayload := map[string]interface{}{
			"metadata": map[string]interface{}{
				"finalizers":      mutate(target.GetFinalizers()),
				"resourceVersion": target.GetResourceVersion(),
			},
		}
		patchData, err := json.Marshal(patchPayload)
		if err != nil {
			return fmt.Errorf("marshaling finalizer patch: %w", err)
		}
		_, err = client.Resource(gvr).Namespace(target.GetNamespace()).Patch(
			ctx, target.GetName(), types.MergePatchType, patchData, metav1.PatchOptions{},
		)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) || attempt > 0 {
			return fmt.Errorf("patching finalizers on %s/%s: %w", target.GetNamespace(), target.GetName(), err)
		}
		fresh, getErr := client.Resource(gvr).Namespace(target.GetNamespace()).Get(ctx, target.GetName(), metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("re-reading %s/%s after a finalizer patch conflict: %w", target.GetNamespace(), target.GetName(), getErr)
		}
		target = fresh
	}
}

func containsFinalizer(obj *unstructured.Unstructured, finalizerName string) bool {
	return containsString(obj.GetFinalizers(), finalizerName)
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
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

// holdFinalizers is metadata.finalizers as this controller wants it
// while the resource is alive: its own finalizer present, and any
// finalizer it no longer manages removed.
func (c *Controller) holdFinalizers(existing []string) []string {
	desired := existing
	for _, stale := range c.staleFinalizers {
		desired = removeString(desired, stale)
	}
	if c.finalizerName != "" && !containsString(desired, c.finalizerName) {
		desired = append(desired, c.finalizerName)
	}
	return desired
}

// releaseFinalizers is metadata.finalizers with everything this
// controller holds taken off, for once cleanup has succeeded.
func (c *Controller) releaseFinalizers(existing []string) []string {
	remaining := existing
	if c.finalizerName != "" {
		remaining = removeString(remaining, c.finalizerName)
	}
	for _, stale := range c.staleFinalizers {
		remaining = removeString(remaining, stale)
	}
	return remaining
}

// finalizersNeedUpdate reports whether the live resource is already
// holding exactly what holdFinalizers wants.
func (c *Controller) finalizersNeedUpdate(u *unstructured.Unstructured) bool {
	if c.finalizerName != "" && !containsFinalizer(u, c.finalizerName) {
		return true
	}
	for _, stale := range c.staleFinalizers {
		if containsFinalizer(u, stale) {
			return true
		}
	}
	return false
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

			latestU, getErr := c.cachedGet(ctx, namespace, name)
			if getErr != nil {
				log.Printf("%s CRITICAL: failed to re-read object for status patch: %v. Returning original error.\n", logPrefix, getErr)
				reconcileResult = reconcileErr
				return
			}
			if latestU == nil {
				log.Printf("%s Status patch skipped: resource was deleted during defer execution.\n", logPrefix)
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
			releasing = true
		}
		for _, stale := range c.staleFinalizers {
			if containsFinalizer(u, stale) {
				releasing = true
			}
		}

		if !releasing {
			log.Printf("%s Finalizer not present. Deletion complete/in progress by Kubernetes.\n", logPrefix)
			return nil
		}

		if err := patchFinalizer(ctx, c.client, c.gvr, u, c.releaseFinalizers); err != nil {
			log.Printf("%s ERROR patching to remove finalizer: %v\n", logPrefix, err)
			reconcileErr = &errCleanupPending{fmt.Errorf("finalizer removal patch failed: %w", err)}
			return reconcileErr
		}
		log.Printf("%s Finalizer(s) removed successfully. Deletion will now complete.\n", logPrefix)
		return nil
	}

	if c.finalizersNeedUpdate(u) {
		log.Printf("%s Updating finalizers to %v.\n", logPrefix, c.holdFinalizers(u.GetFinalizers()))

		if err := patchFinalizer(ctx, c.client, c.gvr, u, c.holdFinalizers); err != nil {
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
	result, provisionErr := c.provisionFunc(ctx, c.client, obj)
	if provisionErr != nil {
		reconcileErr = fmt.Errorf("provisioning failed: %w", provisionErr)
		return reconcileErr // defer handles the status update and returns the error for retry
	}

	// A StatusUpdater derives the generic state from the detail fields
	// provisioning just wrote (per-VM run outcomes, say), so it needs an
	// object that carries them. provisionFunc hands one back rather than
	// the engine re-reading the object it was just given.
	latestU := result.Object
	if latestU == nil {
		latestU = u
	}

	if result.RequeueAfter > 0 {
		c.Queue.AddAfter(key(namespace, name), result.RequeueAfter)
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

// watchChildren wakes the parent controller whenever one of its children
// changes, mapping the child to its binding's key the way
// EnqueueRequestForOwner does in controller-runtime.
//
// A Deployment does not poke a Pod, but it does watch every Pod it owns,
// and that watch is what makes its status mean anything. The event
// carries nothing but "look again": the parent's pass lists all its
// children afresh and recomputes the whole summary, so losing an event
// costs latency and the next resync repairs it. The workqueue dedupes by
// key, so twenty children changing at once is one parent pass.
func watchChildren(informer cache.SharedIndexInformer, parent *Controller) {
	parentKeyOf := func(obj interface{}) (string, bool) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		u, err := toUnstructured(obj)
		if err != nil {
			return "", false
		}
		// spec.bindingName is the authority: it carries the binding's full
		// name, where the label may be a truncated-and-hashed stand-in for
		// one too long to be a label value. The label is the fallback for
		// a child whose spec cannot be read - and a child whose label was
		// edited off is exactly the case the parent most needs to hear
		// about, since it is the one that would otherwise never be reaped.
		binding, _, _ := unstructured.NestedString(u.Object, "spec", "bindingName")
		if binding == "" {
			binding = u.GetLabels()[BindingLabel]
		}
		if binding == "" {
			return "", false
		}
		return key(u.GetNamespace(), binding), true
	}

	enqueueParent := func(obj interface{}) {
		if k, ok := parentKeyOf(obj); ok {
			parent.Queue.Add(k)
		}
	}

	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: enqueueParent,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldU, oldErr := toUnstructured(oldObj)
			newU, newErr := toUnstructured(newObj)
			if oldErr == nil && newErr == nil && oldU.GetResourceVersion() == newU.GetResourceVersion() {
				// A resync redelivery rather than a change. The parent
				// resyncs on its own schedule; waking it again here would
				// only duplicate that.
				return
			}
			enqueueParent(newObj)
		},
		DeleteFunc: enqueueParent,
	})
}

// setupInformer builds the shared informer for one kind and points its
// controller at the resulting store, so reconciles read from the cache
// the informer is already maintaining instead of re-fetching every
// object it holds.
func setupInformer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, controller *Controller, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	if indexers == nil {
		indexers = cache.Indexers{}
	}
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
		indexers,
	)
	controller.indexer = informer.GetIndexer()

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
