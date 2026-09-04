package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// maxReconcileRetries bounds fast retries for an ordinary reconcile
// failure. Past it the key is dropped and the periodic resync picks it
// up again, so a permanently broken resource stops spinning without
// being forgotten for good.
const maxReconcileRetries = 10

// reconcileTimeout bounds one pass over one resource. The AWX client's
// timeout is per request, so without this a binding matching many VMs
// can hold a worker for that timeout multiplied by the number of VMs -
// and there are only numWorkers of them, shared by every resource in the
// cluster. Cancelling mid-pass is safe: the reconcile returns an error
// and is retried, and finalization is never abandoned either way.
// Set at startup from --reconcile-timeout.
var reconcileTimeout = 5 * time.Minute

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	// Get() blocks until an item is available.
	obj, shutdown := c.Queue.Get()
	if shutdown {
		return false
	}

	// Tell the queue we're done with this item, even if it failed. If it
	// succeeded this forgets its rate-limit history too.
	defer c.Queue.Done(obj)

	key, ok := obj.(string)
	if !ok {
		c.Queue.Forget(obj)
		log.Printf("Expected string in workqueue but got %#v\n", obj)
		return true
	}

	if err := c.reconcileByKey(ctx, key); err != nil {
		// Finalization is never abandoned. Dropping the key would leave
		// the resource stuck in Terminating with its AWX hosts leaked,
		// and a deleting object may produce no further events to
		// rediscover it from.
		if isCleanupPending(err) || c.Queue.NumRequeues(key) < maxReconcileRetries {
			log.Printf("Failed to reconcile %s: %v. Retrying...\n", key, err)
			c.Queue.AddRateLimited(key)
			return true
		}
		c.Queue.Forget(obj)
		log.Printf("Max retries exceeded for %s: %v. Forgetting item; the next resync will pick it up again.\n", key, err)
		return true
	}

	c.Queue.Forget(obj)
	return true
}

// reconcileByKey re-fetches the object from the API before reconciling it,
// rather than trusting the (possibly stale) object handed to the informer
// callback, so reconciliation is level-triggered.
func (c *Controller) reconcileByKey(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", key)
	}

	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	u, err := c.client.Resource(c.gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Resource is gone; nothing left to reconcile.
			return nil
		}
		return fmt.Errorf("failed to fetch resource %s: %w", key, err)
	}

	return c.Reconcile(ctx, u)
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// Run starts the controller's worker pool and blocks until ctx is
// cancelled and every worker has drained. Shutting the queue down first
// makes the blocking Get() in each worker return, so a SIGTERM finishes
// the reconcile in flight instead of being killed mid-launch.
func (c *Controller) Run(ctx context.Context, workers int) {
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runWorker(ctx)
		}()
	}

	<-ctx.Done()
	c.Queue.ShutDown()
	wg.Wait()
}
