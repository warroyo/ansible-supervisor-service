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

// Targeting modes for an onDeleted hook.
//
// The distinction is who decides what the playbook runs against. Under
// ManagedHost the controller decides: it supplies this VM's inventory
// host as the launch limit, and refuses to launch at all unless the
// template shares that inventory and will accept the limit - the same
// guard the provisioning path has, for the same reason.
//
// Under Template the author of the AWX template decides. The controller
// supplies neither inventory nor limit, so a workflow can deregister a
// VM from wherever its records actually live, in whatever inventories
// its nodes are configured with. That is a wider blast radius by
// design, which is why it is opt-in and never inferred: a template
// without an inventory, or one that stopped accepting a limit, is a
// ManagedHost hook that fails, not a Template hook that silently
// broadens.
const (
	TargetingManagedHost = "ManagedHost"
	TargetingTemplate    = "Template"
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

// Phases a deprovision hook passes through, in status.deprovision.phase.
// The first four are the run phases above; these are the outcomes only a
// hook can reach.
const (
	// PhaseLaunching is written before the launch request goes out, so a
	// controller that dies mid-launch finds a record that something was
	// started. A hook found in this phase is never relaunched: the job
	// may well be running, and running a decommission playbook twice is
	// worse than not knowing whether it ran once.
	PhaseLaunching = "Launching"
	// PhaseTimedOut means the hook did not reach a terminal state within
	// spec.onDeleted.timeoutSeconds. The finalizer is released anyway.
	PhaseTimedOut = "TimedOut"
	// PhaseSkipped means the hook could not run at all - no inventory
	// host to target, AWX unreachable for good, the VM still alive.
	PhaseSkipped = "Skipped"
)

// defaultHookTimeoutSeconds bounds a hook that never finishes. It is a
// deadline measured across requeues rather than a duration anything
// sleeps for: one reconcile is bounded by --reconcile-timeout, which is
// shorter than most playbooks.
const defaultHookTimeoutSeconds = 900

// TemplateRef identifies the AWX job or workflow template an
// AnsibleBinding launches.
type TemplateRef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// DeprovisionHook is a playbook to run on the way out, and how long to
// wait for it.
//
// The template is nested rather than being the hook itself so that the
// timeout - and whatever a later hook needs - sits beside it instead of
// alongside it in the spec.
type DeprovisionHook struct {
	// Targeting is what the hook is aimed at: TargetingManagedHost, the
	// default, or TargetingTemplate. Omitted means ManagedHost, so a
	// manifest written before this existed keeps the behaviour it had.
	//
	// It is deliberately separate from provisioning's useDefaultLimit.
	// Provisioning configures the machine this binding owns; a
	// decommission may have to act on records the machine never held -
	// a DNS zone, an IPAM lease, a CMDB entry, a monitoring silence -
	// and those live wherever the workflow's author put them.
	Targeting string `json:"targeting,omitempty"`
	// Template is the AWX job or workflow template to launch. Under
	// ManagedHost it must accept a limit at launch time, exactly as the
	// provisioning template must: a hook that ran against a whole
	// inventory would decommission every host in it. Under Template
	// nothing is narrowed, so nothing has to be accepted.
	Template TemplateRef `json:"template"`
	// TimeoutSeconds bounds the whole hook - waiting for an in-flight
	// provisioning job, launching, and polling to a terminal state.
	// Past it the finalizer is released regardless, so a playbook that
	// hangs cannot hold a VM, a binding or a namespace in Terminating.
	// Defaults to defaultHookTimeoutSeconds.
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`
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
	// OnDeleted names a template to run when a matched VirtualMachine is
	// deleted, before its inventory host is removed - deregistering it
	// from DNS, IPAM, a CMDB or monitoring. It fires only for a VM that
	// is actually gone; a VM that merely stopped matching the selector is
	// still running, and running a teardown playbook against it would be
	// a surprise rather than a service.
	//
	// The guest is unreachable by then - vm-operator destroys the VM
	// during its own finalization - so the playbook must act on the
	// external record, not on the machine.
	OnDeleted *DeprovisionHook `json:"onDeleted,omitempty"`
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
	// Conflicted counts selected VMs another binding already claims.
	// They are part of Total and counted here instead of Pending: this
	// binding is not waiting for them to start, it will never run them
	// while someone else owns them, and reporting them as merely not
	// started yet is what would leave an operator waiting too.
	Conflicted int `json:"conflicted,omitempty"`
	// ConflictedVMs names a bounded sample of them with the binding that
	// holds each claim, since "which binding took it" is the whole of
	// what an operator needs to resolve one.
	ConflictedVMs []string `json:"conflictedVMs,omitempty"`
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
	// BindingUID is which incarnation of that binding owns it. The name
	// alone is not an identity: a binding deleted and recreated under it
	// is a different object with a different intent, and it must claim
	// its VMs rather than inherit live claims - including any child the
	// previous incarnation left behind mid-cleanup.
	BindingUID string `json:"bindingUID,omitempty"`

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
	OnDeleted        *DeprovisionHook  `json:"onDeleted,omitempty"`

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

	// Deprovision is how far the onDeleted hook has got. It is written
	// during finalization, which is the only reason it exists: one
	// reconcile is too short to launch a playbook and wait for it, so
	// each pass has to be able to resume from what the last one recorded
	// rather than start again. Without it a hook of any real length
	// relaunches on every pass and never converges.
	Deprovision *DeprovisionStatus `json:"deprovision,omitempty"`
}

// DeprovisionStatus is the state of one onDeleted hook, persisted on the
// terminating child so the hook survives requeues, a reconcile timeout
// and a controller restart.
type DeprovisionStatus struct {
	// Phase is Launching, Running, Succeeded, Failed, TimedOut or
	// Skipped.
	Phase string `json:"phase,omitempty"`
	// Targeting is the mode this hook actually started under, stamped
	// with the deadline and read back on every later pass. An edit to
	// the spec mid-teardown must not change what a running hook is
	// aimed at, or relaunch it under the other mode. Empty on a hook
	// started before this was recorded, which means ManagedHost.
	Targeting string `json:"targeting,omitempty"`
	// Message says why, for the phases where why is not obvious.
	Message string `json:"message,omitempty"`
	// StartedAt is when finalization first took an interest in this
	// object, and Deadline is StartedAt plus the configured timeout. The
	// deadline is stored rather than recomputed so that editing the
	// timeout mid-teardown cannot extend a hook that is already running.
	StartedAt string `json:"startedAt,omitempty"`
	Deadline  string `json:"deadline,omitempty"`

	JobID     int64  `json:"jobID,omitempty"`
	JobURL    string `json:"jobURL,omitempty"`
	JobStatus string `json:"jobStatus,omitempty"`
	JobType   string `json:"jobType,omitempty"`

	// Endpoint fingerprints the AWX instance JobID was issued by, so a
	// poll cannot follow that number to whatever unrelated job holds it
	// on another instance after the AWXConnection is repointed
	// mid-teardown. status.awxEndpoint carries the same fingerprint for
	// the inventory host, but only once a provisioning pass has recorded
	// one - a hook that ran on a child whose host was rediscovered rather
	// than remembered would have nothing to check against.
	Endpoint string `json:"endpoint,omitempty"`

	// LaunchError is what AWX said it ignored about the launch, kept for
	// the life of the hook. AWX answers a launch it has narrowed with
	// both a job id and an ignored_fields list: the job is real and has
	// to be tracked, but a limit it dropped means the playbook is running
	// against something other than this host, and a later "successful"
	// job status must not be allowed to erase that.
	LaunchError string `json:"launchError,omitempty"`

	// HostPinned records that the hook set ansible_connection on an
	// inventory host that will outlive it - one under cleanupPolicy:
	// Retain, or an adopted host this controller never owned - so the
	// override can be taken back off. PriorConnection is what the
	// variable said before, with nil meaning it was not set at all: an
	// absent value and an explicitly configured one restore differently.
	HostPinned      bool    `json:"hostPinned,omitempty"`
	PriorConnection *string `json:"priorConnection,omitempty"`

	// PinnedHostID and PinnedHostEndpoint are which host the override
	// above actually went on. The restore has to find that same host
	// rather than whatever now answers to the name: a host deleted out
	// of band and recreated during the hook is a different host with a
	// different id, and writing a remembered "prior" connection onto it
	// would be inventing a variable nothing ever set. Absent on a record
	// written before this was tracked, where the name is all there is.
	PinnedHostID       int64  `json:"pinnedHostID,omitempty"`
	PinnedHostEndpoint string `json:"pinnedHostEndpoint,omitempty"`
}
