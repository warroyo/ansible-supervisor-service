package main

import (
	"context"
	"encoding/json"
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
	client           *dynamic.DynamicClient
	gvr              schema.GroupVersionResource
	finalizerName    string
	provisionFunc    func(*dynamic.DynamicClient, interface{}, []string) error
	cleanupFunc      func(*dynamic.DynamicClient, interface{}) error
	updateStatusFunc StatusUpdater
	namespaces       []string

	Queue    workqueue.RateLimitingInterface
	Informer cache.SharedIndexInformer
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
// the array is replaced wholesale.
func patchFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, finalizerName string, finalizers []string) error {
	patchPayload := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": finalizers,
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

func (c *Controller) Reconcile(obj interface{}) (reconcileResult error) {
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

			latestU, getErr := c.client.Resource(c.gvr).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
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

			statusMap := c.updateStatusFunc(latestU, false, reconcileErr)
			if statusPatchErr := patchStatus(context.TODO(), c.client, c.gvr, latestU, statusMap, StatusFieldManager); statusPatchErr != nil {
				if !apierrors.IsNotFound(statusPatchErr) && !apierrors.IsConflict(statusPatchErr) {
					log.Printf("%s CRITICAL: failed to patch status after error: %v\n", logPrefix, statusPatchErr)
				}
			}

			reconcileResult = reconcileErr
		}
	}()

	if !u.GetDeletionTimestamp().IsZero() {
		log.Printf("%s DeletionTimestamp detected. Initiating finalization.\n", logPrefix)

		if containsFinalizer(u, c.finalizerName) {
			log.Printf("%s Finalizer %s is present. Starting cleanup...\n", logPrefix, c.finalizerName)

			if cleanupErr := c.cleanupFunc(c.client, obj); cleanupErr != nil {
				log.Printf("%s CLEANUP FAILED: %v. Will retry on next sync.\n", logPrefix, cleanupErr)
				reconcileErr = fmt.Errorf("cleanup failed: %w", cleanupErr)
				return reconcileErr
			}

			updatedFinalizers := removeString(u.GetFinalizers(), c.finalizerName)
			if err := patchFinalizer(context.TODO(), c.client, c.gvr, u, c.finalizerName, updatedFinalizers); err != nil {
				log.Printf("%s ERROR patching to remove finalizer: %v\n", logPrefix, err)
				reconcileErr = fmt.Errorf("finalizer removal patch failed: %w", err)
				return reconcileErr
			}
			log.Printf("%s Finalizer %s removed successfully. Deletion will now complete.\n", logPrefix, c.finalizerName)
			return nil
		}

		log.Printf("%s Finalizer not present. Deletion complete/in progress by Kubernetes.\n", logPrefix)
		return nil
	}

	if !containsFinalizer(u, c.finalizerName) {
		log.Printf("%s Finalizer %s missing. Adding it now.\n", logPrefix, c.finalizerName)

		updatedFinalizers := append(u.GetFinalizers(), c.finalizerName)
		if err := patchFinalizer(context.TODO(), c.client, c.gvr, u, c.finalizerName, updatedFinalizers); err != nil {
			log.Printf("%s ERROR patching to add finalizer: %v\n", logPrefix, err)
			reconcileErr = fmt.Errorf("finalizer addition patch failed: %w", err)
			return reconcileErr
		}

		statusMap := c.updateStatusFunc(u, false, nil)
		if statusPatchErr := patchStatus(context.TODO(), c.client, c.gvr, u, statusMap, StatusFieldManager); statusPatchErr != nil {
			log.Printf("%s Warning: failed to patch status after adding finalizer: %v\n", logPrefix, statusPatchErr)
		}

		log.Printf("%s Finalizer added. Continuing to main reconciliation.\n", logPrefix)
	}

	log.Printf("%s Running normal reconciliation.\n", logPrefix)
	if provisionErr := c.provisionFunc(c.client, obj, c.namespaces); provisionErr != nil {
		reconcileErr = fmt.Errorf("provisioning failed: %w", provisionErr)
		return reconcileErr // defer handles the status update and returns the error for retry
	}

	statusMap := c.updateStatusFunc(u, true, nil)
	if statusPatchErr := patchStatus(context.TODO(), c.client, c.gvr, u, statusMap, StatusFieldManager); statusPatchErr != nil {
		log.Printf("%s Warning: failed to patch status after successful provisioning: %v. Requeuing...\n", logPrefix, statusPatchErr)
		return statusPatchErr
	}

	log.Printf("%s Reconciliation complete and status updated.\n", logPrefix)
	return nil
}

func setupInformer(client dynamic.Interface, gvr schema.GroupVersionResource, controller *Controller, resyncPeriod time.Duration) cache.SharedIndexInformer {
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return client.Resource(gvr).Namespace("").List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return client.Resource(gvr).Namespace("").Watch(context.TODO(), options)
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
