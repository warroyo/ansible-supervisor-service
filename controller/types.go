package main

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AWXConnection points the controller at an AWX/Tower instance and the
// credential Secret to authenticate with it. It is namespace-scoped: each
// tenant namespace owns its own connection(s) and the Secret backing them,
// so no cross-namespace credential sharing is ever needed.
type AWXConnection struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *AWXConnectionSpec   `json:"spec,omitempty"`
	Status            *AWXConnectionStatus `json:"status,omitempty"`
}

type AWXConnectionSpec struct {
	// URL is the base URL of the AWX/Tower instance, e.g. https://awx.example.com
	URL string `json:"url"`
	// SecretRef names a Secret in this namespace holding the AWX API token
	// in its "token" key.
	SecretRef string `json:"secretRef"`
	// InsecureSkipVerify skips TLS certificate verification when calling
	// the AWX API. Mutually exclusive with CABundleSecretRef: trusting a
	// CA and then not checking it is a contradiction worth rejecting
	// rather than silently resolving.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
	// CABundleSecretRef names a Secret in this namespace holding a PEM
	// CA bundle to trust when calling the AWX API, for an instance
	// served by a private CA. The bundle is added to the system roots,
	// not swapped for them.
	CABundleSecretRef *SecretKeyRef `json:"caBundleSecretRef,omitempty"`
	// APIBasePath overrides where this instance serves the controller
	// API. Leave empty to detect it: "/api/v2" on AWX, Ansible Tower and
	// AAP up to 2.4, "/api/controller/v2" on AAP 2.5+, where the
	// platform gateway moved the controller endpoints.
	APIBasePath string `json:"apiBasePath,omitempty"`
	// HostNamePrefix is prepended to every AWX inventory host name
	// created through this connection. AWX host names must be unique
	// within an inventory, so when several supervisors or namespaces
	// share one AWX instance a prefix keeps their host entries apart.
	HostNamePrefix string `json:"hostNamePrefix,omitempty"`
}

// SecretKeyRef points at one key of a Secret in the same namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	// Key holds the value within the Secret. Defaults to "ca.crt" when
	// empty - the name Kubernetes already uses for a CA bundle.
	Key string `json:"key,omitempty"`
}

type AWXConnectionStatus struct {
	State       string `json:"state,omitempty"`
	Message     string `json:"message,omitempty"`
	Ready       bool   `json:"ready,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`
	// APIBasePath is the API root actually in use - detected on first
	// validation, or copied from spec.apiBasePath. Cached here so every
	// reconcile doesn't re-probe the instance.
	APIBasePath string `json:"apiBasePath,omitempty"`
}

// Template types an AnsibleBinding can launch. AWX exposes Job
// Templates and Workflow Templates as distinct objects with distinct
// launch endpoints.
const (
	TemplateTypeJob      = "JobTemplate"
	TemplateTypeWorkflow = "WorkflowTemplate"
)

// CleanupPolicy controls whether the controller deletes AWX inventory
// hosts it created when they're no longer needed.
const (
	CleanupPolicyDelete = "Delete"
	CleanupPolicyRetain = "Retain"
)

// Per-VM run phases, tracked in an AnsibleBindingVM's status.phase.
const (
	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
)

// TemplateRef identifies the AWX job or workflow template an
// AnsibleBinding launches.
type TemplateRef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// AnsibleBinding binds one or more VM Service VirtualMachines
// (selected by vmSelector) to an AWX job/workflow template, mirroring
// Aria's day-2 Ansible Automation Platform integration.
type AnsibleBinding struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *AnsibleBindingSpec   `json:"spec,omitempty"`
	Status            *AnsibleBindingStatus `json:"status,omitempty"`
}

type AnsibleBindingSpec struct {
	// VMSelector is a matchLabels-style selector for VirtualMachines in
	// this namespace.
	VMSelector map[string]string `json:"vmSelector"`
	// AWXConnectionRef names an AWXConnection in this namespace.
	AWXConnectionRef string `json:"awxConnectionRef"`
	// Template is the job or workflow template to launch per matched VM.
	Template TemplateRef `json:"template"`
	// HostName overrides the Ansible inventory host name. Only honored
	// when vmSelector matches exactly one VM (otherwise every match
	// would collide on the same host name).
	HostName string `json:"hostName,omitempty"`
	// HostVariables are merged into the upserted inventory host's vars,
	// e.g. to override ansible_host with a FQDN instead of the VM's IP.
	HostVariables map[string]string `json:"hostVariables,omitempty"`
	// UseDefaultLimit, if true, launches without a --limit so the
	// template's own configured limit/inventory applies. Default false
	// scopes the run to the provisioned host.
	UseDefaultLimit bool `json:"useDefaultLimit,omitempty"`
	// ExtraVars are passed to the template launch as extra_vars.
	ExtraVars map[string]string `json:"extraVars,omitempty"`
	// CleanupPolicy controls whether AWX inventory hosts this controller
	// created are deleted when a VM stops matching or this CR is
	// deleted. Defaults to Delete.
	CleanupPolicy string `json:"cleanupPolicy,omitempty"`
}

// VMRunHistoryEntry is one past run recorded for a VM, most recent first.
type VMRunHistoryEntry struct {
	JobID      int64  `json:"jobID,omitempty"`
	Status     string `json:"status,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// historyLimit bounds how many past runs are kept per VM.
const historyLimit = 10

type AnsibleBindingStatus struct {
	State       string `json:"state,omitempty"`
	Message     string `json:"message,omitempty"`
	Ready       bool   `json:"ready,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`
	// ObservedGeneration and LastAppliedTrigger are informational: the
	// generation and re-run request the controller most recently saw.
	// The decision to (re)launch is made per VM, against the equivalent
	// fields in each child's status, so a request made while one VM's job
	// is still in flight is not lost.
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	LastAppliedTrigger string `json:"lastAppliedTrigger,omitempty"`
	// Summary counts the AnsibleBindingVM children by phase. It is fixed
	// size no matter how many VMs the selector matches, which is what
	// keeps the binding writable at fleet scale - the per-VM detail it
	// replaced grew without bound and stopped fitting in the object.
	Summary *BindingSummary `json:"summary,omitempty"`
	// LastOrphanScan is when this binding last listed the AWX hosts it
	// owns looking for ones no child claims. Kept in status for the same
	// reason as the child's lastHostCheck: the scan is rare and costs an
	// AWX request, and a pass has to be able to work out whether one is
	// due from the object rather than from process memory.
	LastOrphanScan string `json:"lastOrphanScan,omitempty"`
}

// BindingSummary is the rollup a binding reports in place of per-VM
// entries.
type BindingSummary struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded,omitempty"`
	Running   int `json:"running,omitempty"`
	Pending   int `json:"pending,omitempty"`
	Failed    int `json:"failed,omitempty"`
	// FailedVMs names a bounded sample of the failing VMs, and
	// FirstFailure carries one of their messages, so the common case -
	// "why is this binding red" - is answerable without listing the
	// children.
	FailedVMs    []string `json:"failedVMs,omitempty"`
	FirstFailure string   `json:"firstFailure,omitempty"`
	// Terminating counts children being deleted. They are counted apart
	// from the phase buckets, and not in Total, because a child on its
	// way out is not something the binding's readiness turns on - but
	// nor can it be dropped silently. A child wedged in Terminating (an
	// AWX host that will not delete) would otherwise vanish from the
	// rollup entirely and leave the binding above it reading Ready while
	// something underneath it retried forever.
	Terminating int `json:"terminating,omitempty"`
}

// AnsibleBindingVM is one VM's unit of work under an AnsibleBinding:
// the AWX inventory host for that VM, and the run launched against it.
//
// It exists so the per-VM work is one Kubernetes object rather than one
// entry in a list on the binding. That is what keeps status O(1) per VM
// instead of O(N) per binding, lets the workqueue reconcile VMs in
// parallel across workers, and - once teardown hooks land - gives each
// VM its own finalizer so a deprovision resumes across requeues instead
// of restarting a whole binding's worth of work every pass.
//
// Its ownerReference points at the VirtualMachine, not the binding, so
// the garbage collector deletes it when the VM goes away. The binding
// deletes it when the VM merely stops matching the selector. Both routes
// end in this object's own finalizer, which is where a teardown hook
// will hang.
type AnsibleBindingVM struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *AnsibleBindingVMSpec   `json:"spec,omitempty"`
	Status            *AnsibleBindingVMStatus `json:"status,omitempty"`
}

// BindingLabel is set on every child to the name of the AnsibleBinding
// that created it, so a parent can list its own children with a label
// selector rather than reading every child in the namespace.
const BindingLabel = "field.vmware.com/binding"

type AnsibleBindingVMSpec struct {
	// VMName is the VirtualMachine in this namespace this object tracks.
	VMName string `json:"vmName"`
	// BindingName is the AnsibleBinding that owns this child. The AWX
	// host ownership marker is keyed to it rather than to this object, so
	// a host outlives the child that made it and is adopted back rather
	// than refused as another binding's.
	BindingName string `json:"bindingName"`

	// The rest is copied down from the binding at create time rather
	// than read back through it. A child has to be able to finalize
	// after its parent is already gone - deleting a binding deletes its
	// children, and the parent may well win that race - so it cannot
	// hold a pointer to a spec that may not be there.
	AWXConnectionRef string            `json:"awxConnectionRef"`
	Template         TemplateRef       `json:"template"`
	HostName         string            `json:"hostName,omitempty"`
	HostVariables    map[string]string `json:"hostVariables,omitempty"`
	UseDefaultLimit  bool              `json:"useDefaultLimit,omitempty"`
	ExtraVars        map[string]string `json:"extraVars,omitempty"`
	CleanupPolicy    string            `json:"cleanupPolicy,omitempty"`

	// BindingGeneration and BindingTrigger are the binding's generation
	// and reconcile-requested-at value as of the last time the parent
	// wrote this spec. The child compares them against the equivalent
	// fields in its own status to decide whether to (re)launch, which is
	// how a spec change or a re-run request reaches a VM now that the
	// child cannot see the binding's own metadata.
	BindingGeneration int64  `json:"bindingGeneration,omitempty"`
	BindingTrigger    string `json:"bindingTrigger,omitempty"`
}

// AnsibleBindingVMStatus is one VM's inventory host and run, plus the
// generic state/message/ready every CRD here carries.
//
// Nothing marks a cleanup as outstanding: the object itself is that
// record, since it sits in Terminating until its finalizer clears.
type AnsibleBindingVMStatus struct {
	State       string `json:"state,omitempty"`
	Message     string `json:"message,omitempty"`
	Ready       bool   `json:"ready,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`

	ObservedIP string `json:"observedIP,omitempty"`
	Phase      string `json:"phase,omitempty"`

	AWXHostID      int64  `json:"awxHostID,omitempty"`
	AWXInventoryID int64  `json:"awxInventoryID,omitempty"`
	AWXHostName    string `json:"awxHostName,omitempty"`
	AWXHostCreated bool   `json:"awxHostCreated,omitempty"`
	// AWXEndpoint identifies the AWX instance the ids above came from.
	// They are only meaningful there: repoint the AWXConnection at a
	// different instance and the same numeric id belongs to some
	// unrelated host, which cleanup would then delete. When this stops
	// matching, the recorded ids are dropped rather than acted on and the
	// host is looked up again by name on the new instance.
	AWXEndpoint string `json:"awxEndpoint,omitempty"`

	LastJobID     int64  `json:"lastJobID,omitempty"`
	LastJobURL    string `json:"lastJobURL,omitempty"`
	LastJobStatus string `json:"lastJobStatus,omitempty"`
	// Launch identity is independent of the current desired template and
	// connection. Secrets themselves are never copied into status.
	LastJobType       string             `json:"lastJobType,omitempty"`
	LastJobConnection *AWXConnectionSpec `json:"lastJobConnection,omitempty"`

	// LastHostCheck is when this VM's inventory host was last reconciled
	// against AWX itself. It lives in status rather than in memory so the
	// decision survives a controller restart and stays a function of
	// observed state: a pass works out whether a check is due by looking
	// at the object, not by remembering what it did last time.
	LastHostCheck string `json:"lastHostCheck,omitempty"`

	// AppliedGeneration and AppliedTrigger record the spec.bindingGeneration
	// and spec.bindingTrigger this VM last launched a run for, so a
	// request made while a job was in flight is not swallowed.
	AppliedGeneration int64               `json:"appliedGeneration,omitempty"`
	AppliedTrigger    string              `json:"appliedTrigger,omitempty"`
	History           []VMRunHistoryEntry `json:"history,omitempty"`
}
