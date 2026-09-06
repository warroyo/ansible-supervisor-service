package main

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Exercises the update filter through a real SharedIndexInformer over a
// fake watch source, rather than by calling the predicate directly: what
// wakes a resource is a property of the informer wiring, and a direct
// call to a cleanup function cannot show it.

func childObject(t *testing.T, resourceVersion string, mutate func(*unstructured.Unstructured)) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": ansBindVMGVR.GroupVersion().String(),
		"kind":       "AnsibleBindingVM",
		"metadata": map[string]interface{}{
			"name": "bind-web-1", "namespace": "ns", "generation": int64(1),
			"resourceVersion": resourceVersion,
		},
		"spec":   map[string]interface{}{"vmName": "web-1", "bindingName": "bind"},
		"status": map[string]interface{}{"phase": PhaseSucceeded},
	}}
	if mutate != nil {
		mutate(u)
	}
	return u
}

func terminating(u *unstructured.Unstructured) {
	now := metav1.Now()
	u.SetDeletionTimestamp(&now)
	u.SetFinalizers([]string{"field.vmware.com/ansible-binding-vm-cleanup"})
}

// informerHarness runs one controller's event handler against a fake
// watch source, so a test can push updates and see what reaches the
// queue.
type informerHarness struct {
	watcher  *watch.FakeWatcher
	queue    workqueue.RateLimitingInterface
	informer cache.SharedIndexInformer
	stop     chan struct{}
}

func newInformerHarness(t *testing.T, initial *unstructured.Unstructured) *informerHarness {
	t.Helper()
	h := &informerHarness{
		watcher: watch.NewFake(),
		queue:   workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		stop:    make(chan struct{}),
	}
	h.informer = cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
				return &unstructured.UnstructuredList{
					Object: map[string]interface{}{
						"apiVersion": ansBindVMGVR.GroupVersion().String(), "kind": "AnsibleBindingVMList",
						"metadata": map[string]interface{}{"resourceVersion": initial.GetResourceVersion()},
					},
					Items: []unstructured.Unstructured{*initial},
				}, nil
			},
			WatchFunc: func(metav1.ListOptions) (watch.Interface, error) { return h.watcher, nil },
		},
		&unstructured.Unstructured{}, 0, cache.Indexers{},
	)
	controller := &Controller{gvr: ansBindVMGVR, Queue: h.queue}
	if _, err := h.informer.AddEventHandler(informerEventHandler(controller)); err != nil {
		t.Fatal(err)
	}
	go h.informer.Run(h.stop)
	if !cache.WaitForCacheSync(h.stop, h.informer.HasSynced) {
		t.Fatal("informer did not sync")
	}
	t.Cleanup(func() {
		close(h.stop)
		h.queue.ShutDown()
	})
	// The initial LIST arrives as an Add, which always enqueues: a
	// controller coming back up has to look at everything once.
	h.drain(t)
	return h
}

// drain empties the queue and reports how many keys were on it.
func (h *informerHarness) drain(t *testing.T) int {
	t.Helper()
	var seen int
	for h.queue.Len() > 0 {
		item, shutdown := h.queue.Get()
		if shutdown {
			break
		}
		h.queue.Done(item)
		h.queue.Forget(item)
		seen++
	}
	return seen
}

// modify pushes an update and gives the informer a moment to deliver it.
// A false result has to survive the wait, so this errs on the side of
// waiting rather than sampling immediately.
func (h *informerHarness) modify(t *testing.T, u *unstructured.Unstructured) int {
	t.Helper()
	h.watcher.Modify(u)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.queue.Len() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Settle, so a late delivery is counted rather than missed.
	time.Sleep(50 * time.Millisecond)
	return h.drain(t)
}

func TestHookStatusWritesDoNotWakeATerminatingChild(t *testing.T) {
	initial := childObject(t, "1", terminating)
	h := newInformerHarness(t, initial)

	// What a hook writes as it goes: a deadline, then a launch, then a
	// job id. None of it changes what the next pass has to do, and the
	// cleanup already asked to be looked at again on its own schedule.
	for i, phase := range []string{PhasePending, PhaseLaunching, PhaseRunning} {
		next := childObject(t, "10"+string(rune('0'+i)), terminating)
		_ = unstructured.SetNestedMap(next.Object, map[string]interface{}{
			"phase": phase, "jobID": int64(42),
		}, "status", "deprovision")
		if got := h.modify(t, next); got != 0 {
			t.Fatalf("a %s status write enqueued %d reconcile(s); the poll interval the cleanup asked for is skipped", phase, got)
		}
	}
}

func TestDeletionAndPolicyChangesStillWakeAChild(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{
			// The transition itself: an object that was alive on the
			// previous event and is terminating on this one.
			name:   "deletion starts",
			mutate: terminating,
		},
		{
			// The parent copying cleanupPolicy down into a terminating
			// child bumps its generation, and that has to take effect
			// promptly - it is the documented way to release a binding
			// stuck on an unreachable AWX.
			name: "cleanup policy changes mid-teardown",
			mutate: func(u *unstructured.Unstructured) {
				terminating(u)
				u.SetGeneration(2)
				_ = unstructured.SetNestedField(u.Object, CleanupPolicyRetain, "spec", "cleanupPolicy")
			},
		},
		{
			name: "re-run requested",
			mutate: func(u *unstructured.Unstructured) {
				u.SetAnnotations(map[string]string{ReconcileRequestedAtAnnotation: "2026-09-05T00:00:00Z"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newInformerHarness(t, childObject(t, "1", nil))
			next := childObject(t, "2", tc.mutate)
			if got := h.modify(t, next); got != 1 {
				t.Fatalf("enqueued %d reconcile(s), want 1", got)
			}
		})
	}
}

func TestResyncStillDeliversATerminatingChild(t *testing.T) {
	// The backstop: whatever the filters skip, the periodic resync
	// redelivers the object unchanged, and that must still enqueue.
	initial := childObject(t, "1", terminating)
	h := newInformerHarness(t, initial)

	same := childObject(t, "1", terminating)
	if got := h.modify(t, same); got != 1 {
		t.Fatalf("a resync redelivery enqueued %d reconcile(s), want 1", got)
	}
}
