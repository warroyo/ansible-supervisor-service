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
	// the AWX API.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
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
	Name          string `json:"name"`
	ObservedIP    string `json:"observedIP,omitempty"`
	Phase         string `json:"phase,omitempty"`
	AWXHostID     int64  `json:"awxHostID,omitempty"`
	LastJobID     int64  `json:"lastJobID,omitempty"`
	LastJobURL    string `json:"lastJobURL,omitempty"`
	LastJobStatus string `json:"lastJobStatus,omitempty"`
	LastUpdated   string `json:"lastUpdated,omitempty"`
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
	AppliedGeneration int64  `json:"appliedGeneration,omitempty"`
	AppliedTrigger    string `json:"appliedTrigger,omitempty"`
	// HostVarsHash fingerprints the host name + variables last pushed to
	// AWX, so unchanged hosts aren't re-PATCHed on every resync.
	HostVarsHash string              `json:"hostVarsHash,omitempty"`
	History      []VMRunHistoryEntry `json:"history,omitempty"`
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
