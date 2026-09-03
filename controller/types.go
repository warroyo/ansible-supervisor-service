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

// Per-VM run phases, tracked in status.vms[].phase.
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

// VMStatus is the per-VM run status for one VirtualMachine currently (or
// formerly) matched by an AnsibleBinding's vmSelector.
type VMStatus struct {
	Name       string `json:"name"`
	ObservedIP string `json:"observedIP,omitempty"`
	Phase      string `json:"phase,omitempty"`
	AWXHostID  int64  `json:"awxHostID,omitempty"`
	// AWXInventoryID is the inventory the host above lives in. Tracked
	// because the inventory comes from the AWX template, which can be
	// repointed at a different one: without this the host would be left
	// behind in the old inventory and a second one created in the new.
	AWXInventoryID int64  `json:"awxInventoryID,omitempty"`
	LastJobID      int64  `json:"lastJobID,omitempty"`
	LastJobURL     string `json:"lastJobURL,omitempty"`
	LastJobStatus  string `json:"lastJobStatus,omitempty"`
	LastUpdated    string `json:"lastUpdated,omitempty"`
	// AWXHostName is the inventory host name last used for this VM.
	// Tracked so a rename (a changed hostNamePrefix or spec.hostName)
	// can retire the old host instead of orphaning it.
	AWXHostName string `json:"awxHostName,omitempty"`
	// AWXHostCreated records whether this controller owns the AWX host.
	// Ownership is stamped into the host's description in AWX, so it
	// survives this CR being deleted and recreated. Hosts that already
	// existed unmarked are adopted, never deleted.
	AWXHostCreated bool `json:"awxHostCreated,omitempty"`
	// PendingCleanup marks an entry whose VM no longer matches the
	// selector but whose AWX host could not be deleted yet. It is kept
	// in status purely so the deletion is retried instead of leaked.
	PendingCleanup bool `json:"pendingCleanup,omitempty"`
	// AppliedGeneration and AppliedTrigger record what this specific VM
	// last launched a run for, so a spec change or re-run request made
	// while a job was in flight isn't swallowed.
	AppliedGeneration int64               `json:"appliedGeneration,omitempty"`
	AppliedTrigger    string              `json:"appliedTrigger,omitempty"`
	History           []VMRunHistoryEntry `json:"history,omitempty"`
}

// AnsibleRun is a single execution: one CR, one AWX job, terminal
// forever. Where an AnsibleBinding is standing desired state that
// re-runs on demand, an AnsibleRun is what an orchestrator creates when
// an event has already happened - configure this host once, register
// this record, open this ticket. Its spec is immutable and it never
// launches a second job; re-running means creating another one.
//
// Two independent axes describe a run. Where it points (nothing, an
// explicit list of hosts, or one VirtualMachine) decides what lands in
// the AWX inventory and in --limit. Where its variables come from
// (literal extraVars, plus varsFrom reading fields off live Kubernetes
// objects) is entirely separate - a run with no target at all can still
// read a VM's IP, which is exactly what a "register this in DNS"
// playbook running on localhost needs.
type AnsibleRun struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *AnsibleRunSpec   `json:"spec,omitempty"`
	Status            *AnsibleRunStatus `json:"status,omitempty"`
}

// VMRef names a VirtualMachine in the same namespace to build an
// inventory host from. It means only that: reading a VM's fields into
// playbook variables is varsFrom's job, with kind VirtualMachine like
// any other object.
type VMRef struct {
	Name string `json:"name"`
}

// RunHost is one explicit inventory target.
//
// Unlike the host name an AnsibleBinding derives from a VM, this is a
// literal the user typed, and it commonly names a host that already
// exists in the AWX inventory. So the AWXConnection's hostNamePrefix is
// deliberately NOT applied to it - prefixing "db-prod-01" would match
// nothing, create a duplicate, and run against the wrong machine.
type RunHost struct {
	Name string `json:"name"`
	// Address sets ansible_host. Optional: left empty, the host's
	// ansible_host is not managed at all, which is what a host already in
	// the inventory with working connection details needs. A host that
	// does not exist yet is still created without one - AWX resolves the
	// name, which is an ordinary inventory pattern.
	Address string `json:"address,omitempty"`
	// Variables are merged into the host's variables, alongside
	// ansible_host when Address is set.
	Variables map[string]string `json:"variables,omitempty"`
}

// ResourceRef points at one object in the AnsibleRun's own namespace.
// There is deliberately no namespace field: a cross-namespace read would
// let a tenant pull data out of a neighbour.
type ResourceRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// VarsFromSource reads fields off one live object into extra_vars. Vars
// maps an extra_vars key to a JSONPath expression evaluated against the
// fetched object, grouped this way so several fields off one object cost
// a single read.
type VarsFromSource struct {
	Resource ResourceRef `json:"resource"`
	// Vars maps an extra_vars key to a JSONPath, e.g.
	// record_ip: "{.status.network.primaryIP4}".
	Vars map[string]string `json:"vars"`
}

type AnsibleRunSpec struct {
	// AWXConnectionRef names an AWXConnection in this namespace.
	AWXConnectionRef string `json:"awxConnectionRef"`
	// Template is the job or workflow template to launch, once.
	Template TemplateRef `json:"template"`

	// VMRef targets one VirtualMachine in this namespace: an inventory
	// host is built from its reported IP and the run is scoped to it.
	// Mutually exclusive with Hosts.
	VMRef *VMRef `json:"vmRef,omitempty"`
	// HostName overrides the inventory host name derived from VMRef.
	HostName string `json:"hostName,omitempty"`
	// HostVariables are merged into the host derived from VMRef.
	HostVariables map[string]string `json:"hostVariables,omitempty"`

	// Hosts targets explicit inventory entries. Mutually exclusive with
	// VMRef.
	Hosts []RunHost `json:"hosts,omitempty"`

	// ExtraVars are passed to the template at launch.
	ExtraVars map[string]string `json:"extraVars,omitempty"`
	// VarsFrom adds extra vars read off live objects in this namespace.
	VarsFrom []VarsFromSource `json:"varsFrom,omitempty"`

	// CleanupPolicy controls whether AWX inventory hosts this run created
	// are deleted when it is. Defaults to Delete. Hosts that already
	// existed are adopted and never deleted regardless.
	CleanupPolicy string `json:"cleanupPolicy,omitempty"`
	// ActiveDeadlineSeconds bounds the whole run, measured from creation.
	// On expiry the run goes terminally Failed, which is what stops a
	// retryable condition - a referenced object that never appears, an
	// AWX job wedged non-terminal - from waiting forever. Zero means no
	// deadline.
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`
	// TTLSecondsAfterFinished deletes this CR that long after it reaches
	// a terminal state, taking the AWX hosts it created with it. Nil
	// keeps it indefinitely; zero collects it on the next pass.
	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

// RunHostStatus is one inventory host this run touched.
type RunHostStatus struct {
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	// AWXHostID and AWXInventoryID locate the host for cleanup.
	AWXHostID      int64 `json:"awxHostID,omitempty"`
	AWXInventoryID int64 `json:"awxInventoryID,omitempty"`
	// AWXHostCreated records whether this run created the host. A host
	// that already existed is adopted and never deleted.
	AWXHostCreated bool `json:"awxHostCreated,omitempty"`
	// PendingCleanup marks a host whose deletion has not succeeded yet.
	PendingCleanup bool `json:"pendingCleanup,omitempty"`
}

type AnsibleRunStatus struct {
	State       string `json:"state,omitempty"`
	Message     string `json:"message,omitempty"`
	Ready       bool   `json:"ready,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`

	JobID     int64  `json:"jobID,omitempty"`
	JobURL    string `json:"jobURL,omitempty"`
	JobStatus string `json:"jobStatus,omitempty"`

	StartedAt string `json:"startedAt,omitempty"`
	// FinishedAt is set only on a terminal outcome, and is what the TTL
	// counts from. A retryable failure leaves it empty on purpose.
	FinishedAt string `json:"finishedAt,omitempty"`
	// LaunchAttemptedAt is written before the launch request goes out.
	// Finding it set with no JobID means the process died between the
	// POST and recording its result: the run is failed rather than
	// launched a second time, since a job that already ran cannot be
	// un-run.
	LaunchAttemptedAt string `json:"launchAttemptedAt,omitempty"`

	// FailureReason explains a terminal failure. It is kept in the detail
	// half of status rather than only in status.message because message
	// is rewritten from scratch on every pass by the engine's own field
	// manager, which would lose the explanation as soon as the next
	// reconcile found nothing left to do.
	FailureReason string `json:"failureReason,omitempty"`

	// ResolvedVars lists the extra_vars names varsFrom produced. Names
	// only - the values are never echoed back into status.
	ResolvedVars []string `json:"resolvedVars,omitempty"`

	Hosts []RunHostStatus `json:"hosts,omitempty"`
}

type AnsibleBindingStatus struct {
	State       string `json:"state,omitempty"`
	Message     string `json:"message,omitempty"`
	Ready       bool   `json:"ready,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`
	// ObservedGeneration and LastAppliedTrigger are informational: the
	// generation and re-run request the controller most recently saw.
	// The decision to (re)launch is made per VM, against the equivalent
	// fields in VMStatus, so a request made while one VM's job is still
	// in flight is not lost.
	ObservedGeneration int64      `json:"observedGeneration,omitempty"`
	LastAppliedTrigger string     `json:"lastAppliedTrigger,omitempty"`
	VMs                []VMStatus `json:"vms,omitempty"`
}
