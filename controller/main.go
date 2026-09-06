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
	hostCheckFlag := flag.Int("host-check-period", 600, "how often, in seconds, each VM's AWX inventory host is reconciled against AWX itself - the worst case for repairing a host deleted or edited by hand in the AWX UI, and what sets the steady-state AWX request rate")
	logLevelFlag := flag.String("log-level", "info", "info logs launches, terminal outcomes and errors; debug adds a line per reconcile pass, which at teardown scale is a great many")
	apiQPSFlag := flag.Float64("api-qps", 50, "sustained requests per second this controller makes to the Kubernetes API server")
	apiBurstFlag := flag.Int("api-burst", 100, "how far above --api-qps a burst of requests may go, e.g. creating children for a selector that suddenly matches many VMs")
	flag.Parse()

	// Nonsense here is quiet and expensive rather than loud: a zero
	// reconcile timeout fails every pass before it starts, a zero API
	// burst blocks every request behind the rate limiter, and a zero host
	// check period puts the AWX traffic back to once per VM per resync.
	// Refuse at startup instead, where it is one line in the log.
	for _, check := range []struct {
		name string
		ok   bool
	}{
		{"--resync-period must be a positive number of seconds", *resync > 0},
		{"--reconcile-timeout must be a positive number of seconds", *reconcileTimeoutFlag > 0},
		{"--host-check-period must be a positive number of seconds", *hostCheckFlag > 0},
		{"--api-qps must be greater than zero", *apiQPSFlag > 0},
		{"--api-burst must be at least --api-qps", float64(*apiBurstFlag) >= *apiQPSFlag},
		{`--log-level must be "info" or "debug"`, *logLevelFlag == "info" || *logLevelFlag == "debug"},
	} {
		if !check.ok {
			fmt.Fprintln(os.Stderr, check.name)
			os.Exit(2)
		}
	}

	resyncPeriod := time.Duration(*resync) * time.Second
	reconcileTimeout = time.Duration(*reconcileTimeoutFlag) * time.Second
	hostCheckPeriod = time.Duration(*hostCheckFlag) * time.Second
	debugLogging = *logLevelFlag == "debug"

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

	// client-go defaults to 5 QPS with a burst of 10, which is an
	// interactive kubectl's budget rather than a controller's: one pass
	// over a binding matching a few hundred VMs would spend minutes
	// inside the client's own rate limiter, and the parallel child writes
	// below would be parallel only in the sense that they wait at the
	// same time. The API server has its own priority-and-fairness
	// protection, so the honest thing is to ask for what the work needs
	// and let the server shed it if it must.
	config.QPS = float32(*apiQPSFlag)
	config.Burst = *apiBurstFlag
	fmt.Printf("api rate limit: %g qps, burst %d\n", config.QPS, config.Burst)

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

	// Before any worker runs and before a single AWX request goes out:
	// this version's claim scheme cannot coexist with children from the
	// previous one, and the check is worth nothing once reconciles have
	// started creating canonical children alongside them.
	if err := checkForLegacyChildren(ctx, dynClient); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}

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

	// One AnsibleBindingVM per matched VM, created by the binding above.
	// Its finalizer cleans up that VM's AWX inventory host - the work
	// that used to happen inside the binding's own finalizer for every
	// VM at once, and so could not fit in one reconcile at scale.
	ansBindVMController := &Controller{
		client:           dynClient,
		gvr:              ansBindVMGVR,
		finalizerName:    "field.vmware.com/ansible-binding-vm-cleanup",
		provisionFunc:    applyAnsibleBindingVM,
		cleanupFunc:      cleanupAnsibleBindingVM,
		updateStatusFunc: updateAnsibleBindingVMStatus,
		Queue:            workqueue.NewRateLimitingQueue(newRateLimiter()),
	}

	// Every CRD here is namespace-scoped and tenant-owned, but the
	// controller itself watches cluster-wide (no namespace allowlist to
	// maintain) - same for the VirtualMachine lookups the children do
	// per-namespace on demand.
	awxConnInformer := setupInformer(ctx, dynClient, awxConnController.gvr, awxConnController, resyncPeriod, nil)
	ansBindInformer := setupInformer(ctx, dynClient, ansBindController.gvr, ansBindController, resyncPeriod, nil)
	ansBindVMInformer := setupInformer(ctx, dynClient, ansBindVMController.gvr, ansBindVMController, resyncPeriod, cache.Indexers{
		childrenByBindingIndex: childrenByBindingIndexFunc,
	})

	// These stores were already being maintained and read by nothing but
	// the event handlers that enqueue keys. Every object in them was then
	// fetched again over the wire by the reconcile that followed.
	awxConnStore = awxConnInformer.GetIndexer()
	ansBindVMStore = ansBindVMInformer.GetIndexer()

	// The parent watches its children, which is the one standard
	// parent-child mechanism the split left out.
	//
	// This carries no intent: the event says "look again", and the pass
	// it triggers re-derives the whole rollup from a full list of
	// children exactly as a resync-driven pass does. A dropped event
	// costs latency, not correctness. Without it status.summary is stale
	// by up to a full resync at all times, including immediately after a
	// run finishes.
	watchChildren(ansBindVMInformer, ansBindController)

	go awxConnInformer.Run(ctx.Done())
	go ansBindInformer.Run(ctx.Done())
	go ansBindVMInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), awxConnInformer.HasSynced, ansBindInformer.HasSynced, ansBindVMInformer.HasSynced) {
		fmt.Fprintln(os.Stderr, "error waiting for cache sync")
		os.Exit(1)
	}
	fmt.Println("ansible supervisor controller started successfully")

	// Each Run returns once its queue has drained, so the process stays
	// alive until every controller has finished the work in hand.
	var wg sync.WaitGroup
	for _, c := range []*Controller{awxConnController, ansBindController, ansBindVMController} {
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
