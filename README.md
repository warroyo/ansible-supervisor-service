# Ansible Supervisor Service

![Release Status](https://github.com/warroyo/ansible-supervisor-service/actions/workflows/build-release.yml/badge.svg)

Supervisor service that binds VM Service `VirtualMachine`s to AWX/Tower job and workflow templates - the day-2 "run a playbook against this VM" experience Aria Automation's Ansible Automation Platform integration provides for classic vRA deployments, reproduced for Supervisor-native VMs.

- [Quickstart](QUICKSTART.md) - shortest path to a first working run
- [How it works](#how-it-works)
- [AnsibleBinding or AnsibleRun?](#ansiblebinding-or-ansiblerun)
- [Prerequisites](#prerequisites)
- [Install](#install)
- [Values](#values)
- [Usage](#usage)
- [CRD status](#crd-status)
- [Uninstalling](#uninstalling)
- [VCFA 9.x blueprints](VCFA-BLUEPRINTS.md) - driving this from a VCF Automation All Apps blueprint
- [FAQ](FAQ.md) · [Contributing](CONTRIBUTING.md)

## How it works

**Coming from classic Aria Automation:** this is the `Cloud.Ansible.Tower` capability, rebuilt as Supervisor CRDs. That integration only reaches VMs vRA provisions itself, and it isn't available at all in a VCF Automation 9.x All Apps organization. In place of an org-wide AAP endpoint and a resource bound to one provisioned machine: a per-namespace `AWXConnection`, and an `AnsibleBinding` that selects VMs by label and sticks around for day-2 re-runs. Field-by-field mapping in [the FAQ](FAQ.md#i-used-the-aap-integration-in-classic-aria-automation-what-maps-to-what).

The controller runs centralized in the supervisor cluster and only ever talks to Kubernetes and to AWX/Tower's HTTPS API - never to the VMs themselves. AWX does the SSH, exactly as it already does for the existing vRA integration, so the controller never needs network reachability into a workload network.

Three CRDs, all namespace-scoped (this is a multi-tenant environment: each tenant namespace owns its own AWX connection and credentials, nothing is shared across namespaces):

**AWXConnection**: points at an AWX/Tower instance and a `Secret` holding its API token. A namespace can define more than one (e.g. dev/prod AWX instances).

**AnsibleBinding**: a persistent binding from a `vmSelector` (matchLabels against `VirtualMachine`s in the same namespace) to an AWX job or workflow template. Re-runnable: bump the `ansible.field.vmware.com/reconcile-requested-at` annotation, or edit the spec, and the controller launches a fresh run against every currently-matched VM. One CR fans out to every VM the selector matches, with a per-VM status entry and bounded run history for each.

**AnsibleRun**: a single execution - one AWX job, launched once, terminal forever. Where a binding is standing desired state, a run is what an orchestrator creates when something has already happened: register this VM in DNS, patch these two servers tonight, open this ticket. Its spec is immutable and neither a spec edit nor the re-run annotation starts a second job; re-running means creating another CR. It can target a VM, an explicit list of hosts, or nothing at all - the last being what a `hosts: localhost` playbook calling an external API needs. [When to use which](#ansiblebinding-or-ansiblerun).

For each matched, powered-on VM with a reported IP, the controller upserts an AWX inventory host (into whatever inventory the target template is already configured with) and launches the template scoped to that host via `--limit`. VMs that drop out of the selector - deleted, relabeled - have their AWX host cleaned up on the very next reconcile, not left stale (a stale host's IP can get reassigned to an unrelated VM later); set `cleanupPolicy: Retain` to opt out if you manage AWX inventory by hand.

A few guardrails worth knowing about:

- **`vmSelector` may not be empty.** An empty selector would match every VM in the namespace, so it's rejected by the CRD schema and by the controller.
- **Pre-existing AWX hosts are adopted, never hijacked.** If a host with the target name already exists in the inventory, the controller merges its variables in rather than overwriting them, records that it did not create the host (`status.vms[].awxHostCreated: false`), and never deletes it during cleanup. If that host's existing variables aren't a JSON object it can safely merge into, it refuses rather than destroying them.
- **Hosts owned by another supervisor are refused outright.** See [Can several supervisors share one AWX instance?](FAQ.md#can-several-supervisors-share-one-awx-instance)
- **The inventory host is reconciled against AWX, not against status.** Every pass reads the host back from AWX, so one deleted or hand-edited in the AWX UI is recreated or repaired. Trusting the controller's own record instead would leave a deleted host undetected, and every later run failing with `--limit does not match any hosts` with nothing to repair it. Variables the controller doesn't manage are left untouched; a steady state writes nothing.
- **In-flight runs are tracked independently of the VM.** A VM powering off mid-run doesn't lose the job, and re-run requests made during downtime aren't swallowed. See [the FAQ](FAQ.md#what-happens-to-in-flight-runs-when-a-vm-powers-off).

## AnsibleBinding or AnsibleRun?

The short version: **is the thing you want a standing state, or an event that already happened?**

| | `AnsibleBinding` | `AnsibleRun` |
|---|---|---|
| Means | "these VMs should be configured like this" | "do this, once, now" |
| Targets | every VM matching `vmSelector`, as they come and go | one VM, an explicit host list, or nothing |
| Runs | on create, on spec edit, on annotation bump, forever | exactly one AWX job, ever |
| Spec | editable | immutable |
| Lifecycle | you delete it | terminal, then optionally self-collecting via `ttlSecondsAfterFinished` |
| Reaches | only VMs this Supervisor owns | anything AWX can reach, including no host at all |

Provisioning a web tier and keeping it configured is a binding. Registering a DNS record, opening a change ticket, patching two bare-metal database servers, or running a decommission playbook before a VM is destroyed is a run.

An `AnsibleRun` has two independent choices, and keeping them apart is what makes the awkward cases easy:

**Where it points** - what lands in the AWX inventory and in `--limit`:

| `spec` | Inventory | `--limit` | For |
|---|---|---|---|
| neither `hosts` nor `vmRef` | untouched | none | `hosts: localhost` playbooks; anything targeting an external API |
| `hosts: [...]` | one host upserted per entry | those hosts | machines this Supervisor does not own |
| `vmRef: {name}` | one host from the VM's reported IP | that host | configuring a Supervisor VM's guest |

**Where its variables come from** - `extraVars` for literals, `varsFrom` for fields read off live Kubernetes objects. This is entirely separate from the above: a run with no target at all can still read a VM's IP, which is exactly what a DNS-registration playbook running on localhost needs.

So there is no "VM mode". `vmRef` means only "build an inventory host from this VM"; reading a VM's fields into playbook variables is `varsFrom`'s job, with `kind: VirtualMachine` like any other object.

## Prerequisites

- VCF supervisor cluster with VM Service enabled. Any served `vmoperator.vmware.com` version works - the controller resolves one by API discovery at startup, preferring the newest, and logs its choice as `virtualmachine api: vmoperator.vmware.com/<version>`
- An AWX or Ansible Automation Platform (Tower) instance that can reach your VMs over SSH at the IP they report in `status.network.primaryIP4` - that address is what the controller writes into the inventory host's `ansible_host`. If AWX has to reach them at some other address, override it per binding with `hostVariables`. AWX, Ansible Tower, AAP 2.4 and AAP 2.5+ are all supported - see [the FAQ](FAQ.md#which-awxtoweraap-versions-are-supported)
- A Job Template or Workflow Template in AWX, with **Prompt on Launch enabled for Limit** (and for Variables, if you use `extraVars`) - [why](FAQ.md#why-does-my-template-need-prompt-on-launch-for-limit). Its inventory is where the controller creates host entries, and the Machine credential attached to it is what logs into the VMs. A Workflow Template with no inventory of its own is the exception: nothing gets a host created for it and there is no inventory for `--limit` to scope against, so the run is not confined to the selected VMs - see [What's different about Workflow Templates?](FAQ.md#whats-different-about-workflow-templates)

## Install

### UI

1. Log into vCenter and navigate to **Workload Management → Services**
2. Click **Add Service** and upload `ansible-supervisor.yml` from the [latest release](https://github.com/warroyo/ansible-supervisor-service/releases)
3. Configure values as needed (see [Values](#values) below)
4. Click **Install**

### Air-gapped

1. Relocate the image bundle to your registry:

```bash
imgpkg copy -b <bundle-ref-from-ansible-supervisor.yml> --to-repo your-repo.example.com/ansible-supervisor
```

2. In `ansible-supervisor.yml`, replace `ghcr.io/warroyo/ansible-supervisor-service` with your registry path. SHA stays the same; only the registry prefix changes.

3. Follow the UI steps above.

## Values

| Field           | Default | Description |
|-----------------|---------|-------------|
| `resync_period` | `"60"`  | Periodic reconcile interval in seconds |
| `namespace`     | `""`    | Namespace to deploy into (filled by the supervisor, do not edit) |
| `supervisor_id` | `""`    | Identity stamped on AWX hosts this supervisor owns. Empty derives it from the `kube-system` namespace UID - set something readable (e.g. `sup-lab-01`) if you share one AWX between supervisors and want its inventory legible |
| `vars_from_api_groups` | `["core", "vmoperator.vmware.com"]` | API groups an `AnsibleRun`'s `varsFrom` may read, `core` meaning the core group. This grants the controller RBAC *and* gates what it will read, so the two can't drift. [What widening it costs](#varsfrom-and-what-it-can-read) |

## Usage

Three manifests, applied in this order, are a complete working setup - `examples/` holds all of them:

| File | What it creates |
|---|---|
| `examples/virtualMachine.yml` | a target VM, labelled for a `vmSelector` and cloud-init'd with the SSH key AWX will log in with |
| `examples/awxConnection.yml` | the `Secret` holding an AWX API token, and the `AWXConnection` pointing at the instance |
| `examples/ansibleBinding.yml` | the binding itself: which VMs, which template |
| `examples/ansibleRun.yml` | one-off runs, in each of their shapes - not part of the minimal setup |

### Target VMs

The service never touches a VM directly, so there's nothing to install on one. A VM only needs to be labelled for the selector, and to accept SSH from AWX:

```bash
kubectl apply -f examples/virtualMachine.yml
```

The label is the only part of that file this service reads: `app: webserver` in the example is what `vmSelector` matches. Everything else is an ordinary VM Service VM.

AWX authenticates with the Machine credential attached to the job template, so the VM has to boot with the matching user and public key already in place. The example does that with cloud-init. A VM that comes up without it produces a job that fails with `unreachable`, and the `AnsibleBinding` reports that VM's `phase: Failed` along with the AWX job to look at.

A matched VM is picked up on the next resync - creating a VM after the `AnsibleBinding` already exists needs no annotation bump, and one binding fans out to every VM its selector matches, each with its own inventory host and its own run.

### AWXConnection

Apply in the namespace you want to launch templates from:

```bash
kubectl apply -f examples/awxConnection.yml
```

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: awx-token
  namespace: my-namespace
type: Opaque
stringData:
  token: "<AWX/Tower API token>"
---
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: sample-awx
  namespace: my-namespace
spec:
  url: "https://awx.example.com"
  secretRef: "awx-token"
  insecureSkipVerify: false
  caBundleSecretRef:          # optional: trust a private CA instead of skipping verification
    name: "awx-token"         # may be the token Secret itself
    key: "ca.crt"             # optional, defaults to ca.crt
  apiBasePath: ""             # leave empty to detect /api/v2 vs /api/controller/v2 (AAP 2.5+)
  hostNamePrefix: ""          # prefix for inventory host names, when sharing one AWX
```

| Field | Description |
|---|---|
| `url` | Base URL of the AWX/Tower instance |
| `secretRef` | Name of a `Secret` in this namespace with the API token under key `token` |
| `insecureSkipVerify` | Skip TLS verification. For self-signed test instances only. Mutually exclusive with `caBundleSecretRef` |
| `caBundleSecretRef` | `Secret` in this namespace holding a PEM CA bundle (key `ca.crt` by default) to trust for an AWX served by a private CA. Added to the system roots, not a replacement for them |
| `apiBasePath` | Leave empty to auto-detect. Set explicitly to skip detection - [details](FAQ.md#which-awxtoweraap-versions-are-supported) |
| `hostNamePrefix` | Prepended to every inventory host name created through this connection - [details](FAQ.md#can-several-supervisors-share-one-awx-instance) |

### AnsibleBinding

```bash
kubectl apply -f examples/ansibleBinding.yml
```

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: sample-webserver-binding
  namespace: my-namespace
spec:
  vmSelector:
    app: webserver
  awxConnectionRef: sample-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate          # JobTemplate | WorkflowTemplate
  hostName: ""                 # only honored when vmSelector matches exactly one VM
  hostVariables: {}            # merged into the upserted host's vars, e.g. custom ansible_host
  useDefaultLimit: false       # false (default) scopes the run to the provisioned VM host
  extraVars:
    environment: production
  cleanupPolicy: Delete        # Delete (default) | Retain
```

| Field | Description |
|---|---|
| `vmSelector` | matchLabels against `VirtualMachine`s in this namespace. May not be empty |
| `awxConnectionRef` | Name of an `AWXConnection` in this namespace |
| `template.name` | Name of the job or workflow template in AWX |
| `template.type` | `JobTemplate` or `WorkflowTemplate` - [differences](FAQ.md#whats-different-about-workflow-templates) |
| `hostName` | Override the inventory host name. Only honored when the selector matches exactly one VM |
| `hostVariables` | Extra host variables merged into the upserted host, e.g. a custom `ansible_host` |
| `useDefaultLimit` | `false` (default) scopes each run to that VM's host. `true` accepts the template's own scope |
| `extraVars` | Passed to the template at launch. Needs Prompt on Launch for Variables in AWX |
| `cleanupPolicy` | `Delete` (default) or `Retain` - see below |

**Forcing a re-run**: bump the reconcile-requested-at annotation, controller-runtime/Flux style:

```bash
kubectl annotate ansiblebinding sample-webserver-binding -n my-namespace \
  ansible.field.vmware.com/reconcile-requested-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

Editing `spec` (which bumps `.metadata.generation`) triggers a re-run the same way, without needing the annotation.

**cleanupPolicy**: `Delete` (default) removes AWX inventory hosts **this controller created** when a VM stops matching the selector or the whole `AnsibleBinding` is deleted. Hosts that already existed and were adopted are never deleted regardless. `Retain` leaves everything in place - useful if you manage AWX inventory by hand, want hosts kept for audit/history, or have other job templates that reference that host outside this CR's control.

Cleanup is retried rather than leaked: a host that can't be deleted stays tracked in `status.vms` with `pendingCleanup: true` until the deletion succeeds, and deleting an `AnsibleBinding` blocks on its finalizer until AWX confirms the hosts are gone. A temporary failure - AWX unreachable, the API server erroring - is retried indefinitely rather than abandoned, since an abandoned host keeps an IP that AWX may later hand to an unrelated VM. Only a permanently unrecoverable case gives up: the `AWXConnection` or its Secret has been deleted, or is malformed beyond retrying, so there's no way left to reach AWX; that's logged and the CR finishes deleting. To release a binding whose AWX instance is gone for good, set `cleanupPolicy: Retain` on it - that's honored even while it's terminating.

### AnsibleRun

```bash
kubectl apply -f examples/ansibleRun.yml
```

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: register-webserver-dns
  namespace: my-namespace
spec:
  awxConnectionRef: sample-awx
  template:
    name: "Register DNS Record"
    type: JobTemplate          # JobTemplate | WorkflowTemplate

  # where it points - at most one of these, both optional
  vmRef:
    name: webserver-a1b2       # a VirtualMachine in this namespace
  hostName: ""                 # override the host name derived from vmRef
  hostVariables: {}            # merged into the host derived from vmRef
  hosts:                       # explicit inventory targets instead
    - name: db-prod-01
      address: 10.20.5.11      # optional; omit to leave ansible_host alone
      variables: {}

  # where its variables come from
  extraVars:
    zone: corp.example.com
  varsFrom:
    - resource:
        apiVersion: vmoperator.vmware.com/v1alpha5
        kind: VirtualMachine
        name: webserver-a1b2
      vars:
        record_name: "{.metadata.name}"
        record_ip: "{.status.network.primaryIP4}"

  cleanupPolicy: Delete          # Delete (default) | Retain
  activeDeadlineSeconds: 600     # unset = no deadline
  ttlSecondsAfterFinished: 3600  # unset = keep the CR indefinitely
```

| Field | Description |
|---|---|
| `awxConnectionRef` | Name of an `AWXConnection` in this namespace |
| `template.name` / `.type` | Same as `AnsibleBinding` |
| `vmRef.name` | Build an inventory host from this VM's reported IP and scope the run to it. Mutually exclusive with `hosts` |
| `hostName`, `hostVariables` | Override / extend the host derived from `vmRef` |
| `hosts[]` | Explicit inventory targets. `name` is used **verbatim** - `hostNamePrefix` is not applied. `address` is optional: omit it for a host already in the inventory with working connection details |
| `extraVars` | Passed to the template at launch. Needs Prompt on Launch for Variables |
| `varsFrom[]` | Read fields off live objects in this namespace into `extra_vars` - see below |
| `cleanupPolicy` | `Delete` (default) or `Retain`, same meaning as on a binding |
| `activeDeadlineSeconds` | Bound the whole run, measured from creation. On expiry it goes terminally `Failed` |
| `ttlSecondsAfterFinished` | Delete the CR that long after it finishes, taking the AWX hosts it created with it. Granularity is `resync_period` |

**No target means no limit**, and that is deliberate. The template's own inventory and scope apply, exactly as `useDefaultLimit: true` does on a binding. For a `hosts: localhost` playbook that is the whole point - AWX requires *an* inventory on a job template regardless, and the usual answer is a throwaway one holding only `localhost`. For a `hosts: all` template with a populated inventory, it means the run reaches all of it. A binding refuses to launch in the analogous situation; a targetless run does not, because there is nothing to scope to.

**Hosts are adopted, never hijacked**, the same as with a binding: a host that already exists has its variables merged into rather than overwritten, reports `awxHostCreated: false`, and is never deleted at cleanup. Only hosts the run created are.

#### `varsFrom`, and what it can read

A playbook that *acts on* a host and one that *acts on its behalf* need opposite things. The second is the common case for DNS, IPAM, CMDB and monitoring registration: the playbook runs `hosts: localhost`, talks to an API, and needs the machine's details as **variables**, not as an inventory host. `varsFrom` is that, and it works over any kind - a `VirtualMachine`, a `Service`, one of your own CRs.

Each entry is one read; `vars` maps an `extra_vars` name to a JSONPath, grouped so several fields off one object cost a single GET. Names are explicit rather than magic, so nothing collides with variables the template already uses, and any field of any VM API version is reachable without this service tracking where each version puts things.

Guardrails, all of which fail the run terminally rather than quietly doing something surprising:

- **Same namespace only.** There is no `namespace` field on `resource` - a cross-namespace read would let a tenant pull data out of a neighbour.
- **Never a `Secret`.** `extra_vars` are visible in AWX job output and in the job's stored launch parameters, so sourcing a Secret through them is a credential leak with extra steps. Attach an AWX Credential to the template instead.
- **Only the groups `vars_from_api_groups` allows.** Widening that value grants the controller `get` on `*` in those groups. It reads with its own identity, not the requesting user's, so anything of an allowed kind in the run's namespace becomes readable by anyone who can create an `AnsibleRun` there. Namespace-scoped RBAC on Supervisor generally already grants a tenant that much, and Secrets are refused regardless - but widen it deliberately, not reflexively.
- **Scalars only.** A path resolving to a list or an object is refused rather than silently JSON-encoded, since `extraVars` is `map[string]string` and the playbook would receive a string where it expected structure.
- **No collisions.** A `varsFrom` name that also appears in `extraVars`, or in another entry, is an error rather than a precedence rule.

Only variable *names* are echoed into `status.resolvedVars`. Values never are.

#### Finished, failed, and collected

`finishedAt` is the pivot: it is set only on a **terminal** outcome, and it is what `ttlSecondsAfterFinished` counts from. The distinction matters because most failures are worth retrying and a few are not:

- **Terminal** - the AWX job failed; the template doesn't exist; a `varsFrom` guardrail was hit; the template would silently drop the limit; the deadline expired. `state: Failed`, and the controller stops.
- **Retryable** - AWX unreachable, the API server erroring, a `varsFrom` object or a `vmRef` VM that hasn't appeared yet, a VM not yet powered on with an IP. The run stays `Pending`/`Running` with the error in `status.message` and keeps trying. `activeDeadlineSeconds` is what bounds this; without one, a run waiting on an object that never arrives waits forever.

`state: Failed` therefore always means "over", never "struggling".

**If a launch outcome is ever lost** - the controller dies between sending the launch and recording the job ID - the run is failed with a message pointing at AWX, rather than launched again. An `AnsibleBinding` resolves that same window the other way, because relaunching a convergent configuration run is harmless; running a decommission playbook or opening a ticket twice is not.

## CRD status

All three CRDs track reconciliation state in `.status`:

| State     | Description |
|-----------|-------------|
| `Pending` | Not yet able to run: provisioning hasn't run, no VM matches `vmSelector`, or a matched VM is powered off / has no reported IP |
| `Running` | A run is still in flight in AWX |
| `Ready`   | Everything targeted completed the requested run successfully |
| `Failed`  | A reconciliation error, or a run that AWX reported as failed. See `message`, and `status.vms` for which specific VM(s) |

An `AnsibleBinding`'s state is aggregated from the per-VM outcomes in `status.vms`, not just from whether the last reconcile threw an error - a job that AWX ran and failed is not a controller error, but it is not `Ready` either.

`AnsibleRun` reads `Failed` more narrowly: it means the run is **over**. A run still retrying a transient problem stays `Pending` or `Running` with the error in `message`, because there it is the difference between "this is finished" and "this hasn't got there yet". See [Finished, failed, and collected](#finished-failed-and-collected).

```bash
kubectl get awxconnection <name> -n <namespace> -o jsonpath='{.status}'
kubectl get ansiblebinding <name> -n <namespace> -o jsonpath='{.status}'
kubectl get ansiblerun <name> -n <namespace> -o jsonpath='{.status}'
```

`AnsibleBinding` additionally tracks `status.vms[]` - one entry per currently-matched VM:

| Field | Meaning |
|---|---|
| `observedIP` | Guest IP the AWX host was pointed at |
| `phase` | `Pending` (never ran, waiting on the VM) / `Running` / `Succeeded` / `Failed` |
| `awxHostID`, `awxHostName`, `awxInventoryID`, `awxHostCreated` | The AWX inventory host, the name and inventory it lives under, and whether this controller owns it (adopted hosts are never deleted) |
| `lastJobID`, `lastJobURL`, `lastJobStatus` | The most recent run and a link to its output in AWX |
| `appliedGeneration`, `appliedTrigger` | What this VM last launched a run for - how re-run requests are tracked per VM |
| `pendingCleanup` | Set when the VM no longer matches but its AWX host still needs deleting |
| `history` | Bounded log of recent runs (one entry per run) |

`AnsibleRun` tracks its single job directly, with no per-VM array:

| Field | Meaning |
|---|---|
| `jobID`, `jobURL`, `jobStatus` | The one AWX job, a link to its output, and AWX's own status word for it |
| `startedAt`, `finishedAt` | `finishedAt` is set only on a terminal outcome, and is what the TTL counts from |
| `launchAttemptedAt` | Written before the launch request goes out. Set with no `jobID` means the outcome was lost |
| `failureReason` | Why a terminal failure happened, kept separately so it survives later reconciles |
| `resolvedVars` | The variable names `varsFrom` produced. Names only |
| `hosts[]` | Inventory hosts this run touched, with `awxHostCreated` marking the ones it owns |

## Uninstalling

Delete your `AnsibleBinding` and `AnsibleRun` resources **while the controller is still running**, then remove the service. Each one carries a cleanup finalizer, so if the controller is gone first, the CRs hang in `Terminating` and deleting the CRD blocks indefinitely. (`AWXConnection` has no finalizer - it creates nothing outside Kubernetes - so it deletes whether the controller is running or not.) Recovery is to reinstall the controller and let it drain them, or to strip the finalizers by hand:

```bash
kubectl patch ansiblebinding <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch ansiblerun <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":[]}}'
```

Note that stripping finalizers skips AWX cleanup, leaving host entries behind - [how to find them](FAQ.md#how-do-i-find-awx-hosts-a-supervisor-left-behind).
