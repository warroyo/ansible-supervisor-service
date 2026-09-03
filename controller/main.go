package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/workqueue"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resync := flag.Int("resync-period", 60, "reconcile resync interval, in seconds")
	supervisorIDFlag := flag.String("supervisor-id", "", "identity stamped on AWX inventory hosts this supervisor owns, so one AWX instance can be shared by several supervisors (default: the kube-system namespace UID)")
	flag.Parse()
	resyncPeriod := time.Duration(*resync) * time.Second

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

	supervisorID, err = resolveSupervisorID(dynClient, *supervisorIDFlag)
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("supervisor id: %s\n", supervisorID)

	// One rate limiter per queue: its backoff state is keyed by
	// "namespace/name", so a shared limiter would let a failing
	// AnsibleBinding throttle an identically named AWXConnection.
	newRateLimiter := func() workqueue.RateLimiter {
		return workqueue.NewItemExponentialFailureRateLimiter(time.Second, 60*time.Second)
	}

	awxConnController := &Controller{
		client:           dynClient,
		gvr:              awxConnGVR,
		finalizerName:    "field.vmware.com/awx-connection-cleanup",
		provisionFunc:    applyAWXConnection,
		cleanupFunc:      cleanupAWXConnection,
		updateStatusFunc: updateGenericStatus,
		Queue:            workqueue.NewRateLimitingQueue(newRateLimiter()),
	}

	ansBindController := &Controller{
		client:           dynClient,
		gvr:              ansBindGVR,
		finalizerName:    "field.vmware.com/ansible-binding-cleanup",
		provisionFunc:    applyAnsibleBinding,
		cleanupFunc:      cleanupAnsibleBinding,
		updateStatusFunc: updateGenericStatus,
		Queue:            workqueue.NewRateLimitingQueue(newRateLimiter()),
	}

	// Both CRDs are namespace-scoped and tenant-owned, but the controller
	// itself watches cluster-wide (no namespace allowlist to maintain) -
	// same for the VirtualMachine lookups applyAnsibleBinding does
	// per-namespace on demand.
	awxConnInformer := setupInformer(dynClient, awxConnController.gvr, awxConnController, resyncPeriod)
	ansBindInformer := setupInformer(dynClient, ansBindController.gvr, ansBindController, resyncPeriod)

	stop := make(chan struct{})
	defer close(stop)

	go awxConnInformer.Run(stop)
	go ansBindInformer.Run(stop)

	if !cache.WaitForCacheSync(stop, awxConnInformer.HasSynced, ansBindInformer.HasSynced) {
		fmt.Fprintln(os.Stderr, "error waiting for cache sync")
		os.Exit(1)
	}
	fmt.Println("ansible supervisor controller started successfully")

	go awxConnController.Run(ctx, numWorkers)
	go ansBindController.Run(ctx, numWorkers)

	<-ctx.Done()
	fmt.Println("shutting down controller")
}
