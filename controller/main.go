package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/workqueue"
)

func main() {
	// Cancelled on SIGTERM/SIGINT so a rolling update drains in-flight
	// reconciles instead of being killed part-way through one.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resync := flag.Int("resync-period", 60, "reconcile resync interval, in seconds")
	reconcileTimeoutFlag := flag.Int("reconcile-timeout", 300, "maximum time one reconcile of one resource may take, in seconds, so a slow AWX cannot pin a worker indefinitely")
	supervisorIDFlag := flag.String("supervisor-id", "", "identity stamped on AWX inventory hosts this supervisor owns, so one AWX instance can be shared by several supervisors (default: the kube-system namespace UID)")
	flag.Parse()
	resyncPeriod := time.Duration(*resync) * time.Second
	reconcileTimeout = time.Duration(*reconcileTimeoutFlag) * time.Second

	// A kubeconfig is for local development; running as a supervisor
	// service there is none and the service account is used instead.
	// Which one won is only known after the config actually builds -
	// in-cluster, $HOME/.kube/config is a path that simply isn't there -
	// so nothing is logged until then.
	var kubeconfig string
	if envPath := os.Getenv("KUBECONFIG"); envPath != "" {
		kubeconfig = envPath
	} else if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	var config *rest.Config
	if kubeconfig != "" {
		if built, buildErr := clientcmd.BuildConfigFromFlags("", kubeconfig); buildErr == nil {
			config = built
			fmt.Printf("using kubeconfig %s\n", kubeconfig)
		}
	}
	if config == nil {
		inCluster, err := rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
		config = inCluster
		fmt.Println("using in-cluster config")
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	supervisorID, err = resolveSupervisorID(ctx, dynClient, *supervisorIDFlag)
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("supervisor id: %s\n", supervisorID)

	// Which VirtualMachine API version this Supervisor serves is decided
	// here rather than compiled in. A failure isn't fatal - the fallback
	// version may well be right - but it is worth shouting about, since
	// the symptom of getting this wrong is bindings sitting in Pending
	// forever with nothing to indicate why.
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	if resolved, resolveErr := resolveVMGVR(discoveryClient); resolveErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not discover the VirtualMachine API version (%v); falling back to %s\n", resolveErr, vmGVR.GroupVersion())
	} else {
		vmGVR = resolved
	}
	fmt.Printf("virtualmachine api: %s\n", vmGVR.GroupVersion())

	// One rate limiter per queue: its backoff state is keyed by
	// "namespace/name", so a shared limiter would let a failing
	// AnsibleBinding throttle an identically named AWXConnection.
	newRateLimiter := func() workqueue.RateLimiter {
		return workqueue.NewItemExponentialFailureRateLimiter(time.Second, 60*time.Second)
	}

	// An AWXConnection creates nothing outside Kubernetes, so it needs no
	// finalizer; the one earlier versions set is stripped on sight.
	awxConnController := &Controller{
		client:           dynClient,
		gvr:              awxConnGVR,
		staleFinalizers:  []string{awxConnectionStaleFinalizer},
		provisionFunc:    applyAWXConnection,
		updateStatusFunc: updateGenericStatus,
		Queue:            workqueue.NewRateLimitingQueue(newRateLimiter()),
	}

	ansBindController := &Controller{
		client:           dynClient,
		gvr:              ansBindGVR,
		finalizerName:    "field.vmware.com/ansible-binding-cleanup",
		provisionFunc:    applyAnsibleBinding,
		cleanupFunc:      cleanupAnsibleBinding,
		updateStatusFunc: updateAnsibleBindingStatus,
		Queue:            workqueue.NewRateLimitingQueue(newRateLimiter()),
	}

	// Both CRDs are namespace-scoped and tenant-owned, but the controller
	// itself watches cluster-wide (no namespace allowlist to maintain) -
	// same for the VirtualMachine lookups applyAnsibleBinding does
	// per-namespace on demand.
	awxConnInformer := setupInformer(ctx, dynClient, awxConnController.gvr, awxConnController, resyncPeriod)
	ansBindInformer := setupInformer(ctx, dynClient, ansBindController.gvr, ansBindController, resyncPeriod)

	go awxConnInformer.Run(ctx.Done())
	go ansBindInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), awxConnInformer.HasSynced, ansBindInformer.HasSynced) {
		fmt.Fprintln(os.Stderr, "error waiting for cache sync")
		os.Exit(1)
	}
	fmt.Println("ansible supervisor controller started successfully")

	// Each Run returns once its queue has drained, so the process stays
	// alive until both controllers have finished the work in hand.
	var wg sync.WaitGroup
	for _, c := range []*Controller{awxConnController, ansBindController} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Run(ctx, numWorkers)
		}()
	}

	<-ctx.Done()
	fmt.Println("shutting down controller: draining in-flight reconciles")
	wg.Wait()
	fmt.Println("shutdown complete")
}
