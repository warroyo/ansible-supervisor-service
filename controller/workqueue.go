package main

import (
	"context"
	"fmt"
	"log"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func (c *Controller) processNextWorkItem() bool {
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

	if err := c.reconcileByKey(key); err != nil {
		if c.Queue.NumRequeues(key) < 10 {
			log.Printf("Failed to reconcile %s: %v. Retrying...\n", key, err)
			c.Queue.AddRateLimited(key)
			return true
		}
		c.Queue.Forget(obj)
		log.Printf("Max retries exceeded for %s: %v. Forgetting item.\n", key, err)
		return true
	}

	c.Queue.Forget(obj)
	return true
}

// reconcileByKey re-fetches the object from the API before reconciling it,
// rather than trusting the (possibly stale) object handed to the informer
// callback, so reconciliation is level-triggered.
func (c *Controller) reconcileByKey(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", key)
	}

	u, err := c.client.Resource(c.gvr).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Resource is gone; nothing left to reconcile.
			return nil
		}
		return fmt.Errorf("failed to fetch resource %s: %w", key, err)
	}

	return c.Reconcile(u)
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem() {
	}
}

// Run starts the controller's worker pool and blocks until ctx is
// cancelled.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer c.Queue.ShutDown()

	for i := 0; i < workers; i++ {
		go c.runWorker(ctx)
	}

	<-ctx.Done()
}
