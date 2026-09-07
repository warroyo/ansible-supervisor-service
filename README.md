# Ansible Supervisor Service

![Release Status](https://github.com/warroyo/ansible-supervisor-service/actions/workflows/build-release.yml/badge.svg)

Supervisor service that binds VM Service `VirtualMachine`s to AWX/Tower job and workflow templates - the day-2 "run a playbook against this VM" experience Aria Automation's Ansible Automation Platform integration provides for classic vRA deployments, reproduced for Supervisor-native VMs.

- [Quickstart](QUICKSTART.md) - shortest path to a first working run
- [Scenarios](SCENARIOS.md) - what the controller does on a create, an update and a delete
- [Architecture](ARCHITECTURE.md) - how the objects and the process fit together, with diagrams
- [How it works](#how-it-works)
- [AnsibleBinding or AnsibleRun?](#ansiblebinding-or-ansiblerun)
- [Prerequisites](#prerequisites)
- [Install](#install)
- [Values](#values)
- [Usage](#usage)
- [CRD status](#crd-status)
- [Uninstalling](#uninstalling)
- [VCFA 9.x blueprints](VCFA-BLUEPRINTS.md) - driving this from a VCF Automation All Apps blueprint
- [Argo CD](ARGOCD.md) - health checks, hooks and sync waves for these CRDs
- [Changelog](CHANGELOG.md) - what changed in each release
- [FAQ](FAQ.md) · [Contributing](CONTRIBUTING.md)

## How it works

**Coming from classic Aria Automation:** this is the `Cloud.Ansible.Tower` capability, rebuilt as Supervisor CRDs. That integration only reaches VMs vRA provisions itself, and it isn't available at all in a VCF Automation 9.x All Apps organization. In place of an org-wide AAP endpoint and a resource bound to one provisioned machine: a per-namespace `AWXConnection`, and an `AnsibleBinding` that selects VMs by label and sticks around for day-2 re-runs. Field-by-field mapping in [the FAQ](FAQ.md#i-used-the-aap-integration-in-classic-aria-automation-what-maps-to-what).

The controller runs centralized in the supervisor cluster and only ever talks to Kubernetes and to AWX/Tower's HTTPS API - never to the VMs themselves. AWX does the SSH, exactly as it already does for the existing vRA integration, so the controller never needs network reachability into a workload network.

Three CRDs to write, all namespace-scoped (this is a multi-tenant environment: each tenant namespace owns its own AWX connection and credentials, nothing is shared across namespaces). A fourth, `AnsibleBindingVM`, is created and deleted by the controller - one per matched VM, covered under [CRD status](#crd-status) - and [Architecture](ARCHITECTURE.md) has the whole picture in one diagram:

**AWXConnection**: points at an AWX/Tower instance and a `Secret` holding its API token. A namespace can define more than one (e.g. dev/prod AWX instances).

**AnsibleBinding**: a persistent binding from a `vmSelector` (matchLabels against `VirtualMachine`s in the same namespace) to an AWX job or workflow template. Re-runnable: bump the `ansible.field.vmware.com/reconcile-requested-at` annotation, or edit the spec, and the controller launches a fresh run against every currently-matched VM. One CR fans out to every VM the selector matches, each with its own `AnsibleBindingVM` carrying that VM's phase, job URL and bounded run history.

**AnsibleRun**: a single execution - one AWX job, launched once, terminal forever. Where a binding is standing desired state, a run is what an orchestrator creates when something has already happened: register this VM in DNS, patch these two servers tonight, open this ticket. Its spec is immutable and neither a spec edit nor the re-run annotation starts a second job; re-running means creating another CR. It can target a VM, an explicit list of hosts, or nothing at all - the last being what a `hosts: localhost` playbook calling an external API needs. [When to use which](#ansiblebinding-or-ansiblerun).

For each matched, powered-on VM with a reported IP, the controller upserts an AWX inventory host (into whatever inventory the target template is already configured with) and launches the template scoped to that host via `--limit`. A deleted VM can run a teardown playbook first, via [`onDeleted`](#ansiblebinding). VMs that drop out of the selector - deleted, relabeled - have their AWX host cleaned up on the very next reconcile, not left stale (a stale host's IP can get reassigned to an unrelated VM later); set `cleanupPolicy: Retain` to opt out if you manage AWX inventory by hand.

A few guardrails worth knowing about:

- **`vmSelector` may not be empty.** An empty selector would match every VM in the namespace, so it's rejected by the CRD schema and by the controller.
- **Pre-existing AWX hosts are adopted, never hijacked.** If a host with the target name already exists in the inventory, the controller merges its variables in rather than overwriting them, records that it did not create the host (`awxHostCreated: false` on that VM's `AnsibleBindingVM`), and never deletes it during cleanup. If that host's existing variables aren't a JSON object it can safely merge into, it refuses rather than destroying them.
- **Hosts owned by another supervisor are refused outright.** See [Can several supervisors share one AWX instance?](FAQ.md#can-several-supervisors-share-one-awx-instance)
- **The inventory host is reconciled against AWX, not against status.** Every `host_check_period` (600s by default) the host is read back from AWX, so one deleted or hand-edited in the AWX UI is recreated or repaired. Trusting the controller's own record instead would leave a deleted host undetected, and every later run failing with `--limit does not match any hosts` with nothing to repair it. Variables the controller doesn't manage are left untouched; a steady state writes nothing, and between checks an idle VM costs AWX nothing at all. A spec change or a re-run request is not on that timer - both take effect on the next pass.
- **Hosts nothing accounts for are reaped.** Less often (four host-check periods), each binding lists the AWX hosts carrying its own ownership marker and deletes any that no VM and no child accounts for - what a controller killed mid-cleanup leaves behind. Only hosts this supervisor created for that binding are ever considered, and `cleanupPolicy: Retain` disables it.
- **In-flight runs are tracked independently of the VM.** A VM powering off mid-run doesn't lose the job, and re-run requests made during downtime aren't swallowed. See [the FAQ](FAQ.md#what-happens-to-in-flight-runs-when-a-vm-powers-off).

Step by step, for each of these - a binding created, a VM relabelled in or out, a spec edited, a host hand-deleted in AWX, a binding torn down - see [Scenarios](SCENARIOS.md).

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
| `reconcile_timeout` | `"300"` | Maximum seconds one reconcile of one resource may take. Bounds how long a binding matching many VMs can hold a worker against a slow AWX |
| `api_qps` / `api_burst` | `"50"` / `"100"` | Kubernetes API request budget. client-go's defaults (5/10) are an interactive kubectl's, not a controller's - a binding matching hundreds of VMs would spend minutes inside the client's own rate limiter |
| `host_check_period` | `"600"` | How often, in seconds, each VM's AWX inventory host is reconciled against AWX itself. Everything else in an idle pass is served from the controller's own caches, so this is what sets the steady-state AWX request rate - and it is the worst case for repairing a host deleted or edited by hand in the AWX UI. Lower it if hand edits in AWX are common; raise it if AWX is the bottleneck |
| `log_level` | `"info"` | `info` logs launches, terminal outcomes and errors. `debug` adds a line per reconcile pass - useful on one binding, a great deal of output during a large teardown |
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
  onDeleted:                   # optional: run a playbook when a VM is deleted
    targeting: ManagedHost     # ManagedHost (default) | Template
    template:
      name: "Deregister Host"
      type: JobTemplate
    timeoutSeconds: 900
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
| `onDeleted` | Optional teardown playbook, run when a matched VM is **deleted** - see below |

**One binding owns a VM.** Selectors may overlap, but a `VirtualMachine`'s lifecycle - provisioning, its AWX inventory host, and `onDeleted` - belongs to exactly one `AnsibleBinding`. The first binding whose child is created wins the VM and keeps it; another binding matching the same VM reports it as conflicted and does nothing to it: no child, no job, no change to the host. Its other VMs carry on as normal, and the binding is not `Ready` while any of its selected VMs is owned elsewhere.

```bash
kubectl get ansiblebinding my-binding -n my-namespace -o jsonpath='{.status.summary.conflictedVMs}'
# ["web-1 (my-namespace/platform-base)"]
```

Ownership is released when the owner stops selecting the VM or is deleted, and only once its child has finished cleaning up; a waiting binding picks it up within about half a minute. Under `cleanupPolicy: Retain` the AWX host keeps the old binding's ownership marker, so the new owner will refuse to touch that host until you retire it or give the new binding its own `hostNamePrefix` - the Kubernetes claim moves, the AWX host is not stolen.

To run **several playbooks for one VM**, compose them into one AWX workflow under a single binding rather than pointing two bindings at it. A workflow's nodes may use different inventories and act on more than this VM; that is AWX's business and the controller does not inspect or restrict it.

**Forcing a re-run**: bump the reconcile-requested-at annotation, controller-runtime/Flux style:

```bash
kubectl annotate ansiblebinding sample-webserver-binding -n my-namespace \
  ansible.field.vmware.com/reconcile-requested-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

Editing `spec` (which bumps `.metadata.generation`) triggers a re-run the same way, without needing the annotation.

**cleanupPolicy**: `Delete` (default) removes AWX inventory hosts **this controller created** when a VM stops matching the selector or the whole `AnsibleBinding` is deleted. Hosts that already existed and were adopted are never deleted regardless. `Retain` leaves everything in place - useful if you manage AWX inventory by hand, want hosts kept for audit/history, or have other job templates that reference that host outside this CR's control.

Cleanup is retried rather than leaked: a host that can't be deleted keeps its `AnsibleBindingVM` in `Terminating` until the deletion succeeds, and deleting an `AnsibleBinding` blocks on its finalizer until every child has finished and AWX has confirmed the hosts are gone. A temporary failure - AWX unreachable, the API server erroring - is retried indefinitely rather than abandoned, since an abandoned host keeps an IP that AWX may later hand to an unrelated VM. Only a permanently unrecoverable case gives up: the `AWXConnection` or its Secret has been deleted, or is malformed beyond retrying, so there's no way left to reach AWX; that's logged and the CR finishes deleting. To release a binding whose AWX instance is gone for good, set `cleanupPolicy: Retain` on it - that's honored even while it's terminating.

**onDeleted**: a template to launch when a matched `VirtualMachine` is deleted, before its inventory host is removed - deregistering it from DNS, IPAM, a CMDB, monitoring or a licence server. It is the counterpart of the provisioning run: `spec.template` runs when a VM arrives, `spec.onDeleted.template` runs when one goes.

```yaml
spec:
  onDeleted:
    template:
      name: "Deregister Host"
      type: JobTemplate
    timeoutSeconds: 900        # the whole hook, across retries. Default 900
```

Four things are worth knowing before you write the playbook.

**The guest is gone.** By the time the hook runs, vm-operator has already destroyed the virtual machine during its own finalization; there is no machine to log into. The playbook has to act on the external record, not on the host. To make that unmissable the controller sets `ansible_connection: local` on the inventory host before launching, so a play that forgets `delegate_to` runs on the AWX control node rather than connecting to an address IPAM may have re-leased to somebody else.

**It fires only for a VM that is really gone.** A VM that merely stopped matching the selector - relabelled, or the selector narrowed - is still running, and a decommission playbook against a live production machine would be damage rather than cleanup. Its inventory host is still cleaned up; no playbook runs. Deleted, being deleted, or replaced by a different VM of the same name all count as gone.

**It never blocks a delete.** A hook that fails, cannot be launched, or overruns `timeoutSeconds` is recorded and released: the host is removed and the object finishes deleting. A teardown playbook that will never succeed must not be able to hold a VM - and the namespace above it - in `Terminating` forever. What happened is written to the controller log with the AWX job URL, to `status.deprovision` on the child while it exists, and to a Kubernetes Event on the `AnsibleBinding`, which outlives the child:

```bash
kubectl get events -n my-namespace --field-selector involvedObject.name=sample-webserver-binding
```

**What it runs against is a choice: `onDeleted.targeting`.**

`ManagedHost` is the default and is what a manifest that says nothing gets. The controller aims the hook at this VM's inventory host: it supplies the host name as the launch limit, so Prompt on Launch for Limit has to be enabled on the hook's template - there is no `useDefaultLimit` escape here, because a deprovision playbook that ran against a whole inventory would decommission every host in it. The template must also be configured with the **same inventory** as `spec.template`: a limit only selects hosts within the job's own inventory, so a hook pointed elsewhere would run against an unrelated host of the same name, or against nothing while reporting success. Either mismatch is refused rather than launched, as is a template with no inventory of its own. If AWX accepts the launch but reports that it dropped the limit, the hook is reported as failed however well the job itself ran.

`Template` hands the aiming back to AWX. The controller supplies **no inventory and no limit at all**, so the job or workflow runs against whatever it is configured to run against. Nothing is required of the template - no shared inventory, no limit prompt - and the hook does not need this VM's inventory host to exist at all: it runs when the host is missing, when it belongs to another binding, or when provisioning never created one. That is the mode for a decommission whose records do not live on the machine: a workflow whose nodes each carry their own inventory, one that retires a DNS record, an IPAM lease and a monitoring entry in three different places, or a `hosts: localhost` playbook that calls an API.

```yaml
spec:
  onDeleted:
    targeting: Template
    template:
      name: "Decommission Records"
      type: WorkflowTemplate
    timeoutSeconds: 900
```

It is opt-in and never inferred. A `ManagedHost` hook whose template stops accepting a limit fails and says so; it is not quietly widened to whatever that template targets. The trade is real: in `Template` mode you own the scope of what runs, including anything it touches beyond this VM, and AWX's own launch requirements (credentials, survey answers) still apply. Nothing changes about the managed host either way - it is cleaned up from what provisioning recorded, not from the workflow's inventory - and in `Template` mode the controller neither pins nor edits it, because it never aimed anything at it.

Prompt on Launch for Variables is optional in both modes: with it, the run gets `asb_hook`, `asb_vm_name`, `asb_vm_uid`, `asb_binding` and `asb_last_known_ip`; without it, those are dropped and the run still goes ahead. That context is reconstructed from the child, not from AWX, so it is there even in `Template` mode where there may be no host to read it from - though `asb_last_known_ip` can be empty, and is a last known address rather than proof of identity.

A host the hook does **not** own is left alone entirely - no variables changed, no playbook run, nothing deleted - if it carries another binding's ownership marker. An unmarked host that this binding adopted is a legitimate target, and so is one under `cleanupPolicy: Retain`; because both outlive the hook, the `ansible_connection` override is taken back off them afterwards, restoring whatever the variable said before. That holds when `cleanupPolicy` is switched to `Retain` while the hook is running, too - the decision is made from the policy in force when the hook finishes.

Under `Retain` there is no host to delete, so a hook that has expired finishes even if AWX cannot be reached at all; the log and the Event then say that the override could not be undone. Under `Delete` an unreachable AWX is still retried indefinitely rather than leaking a host, exactly as it was before hooks existed.

The hook waits for a provisioning job still in flight against the same host before it launches, so a decommission playbook cannot run against a machine a provisioning playbook is still configuring. That wait counts against the same `timeoutSeconds`.

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
| `ttlSecondsAfterFinished` | Delete the CR that long after it finishes, taking the AWX hosts it created with it. Granularity is `resync_period`. Leave it unset when something else owns the object's lifecycle - [a VCFA deployment, for instance](VCFA-BLUEPRINTS.md#one-off-actions-with-ansiblerun) |

**No target means no limit**, and that is deliberate. The template's own inventory and scope apply, exactly as `useDefaultLimit: true` does on a binding. For a `hosts: localhost` playbook that is the whole point - AWX requires *an* inventory on a job template regardless, and the usual answer is a throwaway one holding only `localhost`. For a `hosts: all` template with a populated inventory, it means the run reaches all of it. A binding refuses to launch in the analogous situation; a targetless run does not, because there is nothing to scope to.

**Hosts are adopted, never hijacked**, the same as with a binding: a host that already exists has its variables merged into rather than overwritten, reports `awxHostCreated: false`, and is never deleted at cleanup. Only hosts the run created are.

**The spec is immutable, and the API server enforces it.** That makes a run idempotent across anything that re-applies it - re-applying an unchanged manifest does nothing, and there is no second job to worry about. It also means an *edit* is rejected outright, so any templating system that generates a run's spec from values that can change has to generate a new `metadata.name` when they do, rather than patching in place.

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

All four CRDs track reconciliation state in `.status`. Bindings use these states:

| State     | Description |
|-----------|-------------|
| `Pending` | Not yet able to run, or not yet done: provisioning hasn't run, no VM matches `vmSelector`, a matched VM is powered off / has no reported IP, or a matched VM has not yet completed the run this generation asked for |
| `Running` | At least one VM's run is still in flight in AWX |
| `Ready`   | Every currently-matched VM completed the requested run successfully |
| `Failed`  | A reconciliation error, or a run that AWX reported as failed. See `message`, and `status.summary.failedVMs` for which specific VM(s) |
| `Conflict` | At least one selected VM is owned by a different binding. `status.summary.conflictedVMs` names them and the binding holding each claim. Reported ahead of `Failed`, since nothing will run for those VMs until the overlap is resolved |
| `Terminating` | Every matched VM is gone, but a child is still being cleaned up - typically an AWX host that will not delete. `status.summary.terminating` counts them |

An `AnsibleBinding`'s state is aggregated from its children's outcomes, not just from whether the last reconcile threw an error - a job that AWX ran and failed is not a controller error, but it is not `Ready` either.

`AnsibleRun` reads `Failed` more narrowly: it means the run is **over**. A run still retrying a transient problem stays `Pending` or `Running` with the error in `message`, because there it is the difference between "this is finished" and "this hasn't got there yet". See [Finished, failed, and collected](#finished-failed-and-collected).

```bash
kubectl get awxconnection <name> -n <namespace> -o jsonpath='{.status}'
kubectl get ansiblebinding <name> -n <namespace> -o jsonpath='{.status}'
kubectl get ansiblerun <name> -n <namespace> -o jsonpath='{.status}'
```

`AnsibleBinding` rolls its children up into `status.summary`, which is a fixed size however many VMs the selector matches:

| Field | Meaning |
|---|---|
| `total` | VMs currently matched by `vmSelector` |
| `succeeded`, `running`, `pending`, `failed` | How many children are in each phase |
| `failedVMs`, `firstFailure` | A bounded sample of the failing VMs and one of their messages, so "why is this binding red" is answerable without listing the children |
| `terminating` | Children still being deleted. Counted apart from the phases and not included in `total`, so a child stuck on an AWX host that will not delete is visible rather than silently absent |
| `conflicted`, `conflictedVMs` | Selected VMs another binding owns, and a bounded sample naming the owner of each. Part of `total`, and deliberately not counted as `pending`: this binding is not waiting for them to start, it will not run them at all |

The per-VM detail lives on one `AnsibleBindingVM` per matched VM, named `vm-<vm>-<hash>` - after the VM alone, so that the name *is* the claim on it: every binding computes the same name for a given VM, Kubernetes lets one object hold it, and the create that succeeds is what decides the owner. The hash is of the full VM name, so two VMs that differ only past the truncation still get separate children. The controller creates and deletes these; they are not something to write by hand, and one under any other name is refused rather than reconciled. Look them up by label rather than by name:

```bash
kubectl get ansiblebindingvm -n <namespace> -l field.vmware.com/binding=<binding>
kubectl get ansiblebindingvm -n <namespace> -l field.vmware.com/binding=<binding> \
  -o jsonpath='{.items[?(@.spec.vmName=="<vm>")].status}'
```

| Field | Meaning |
|---|---|
| `observedIP` | Guest IP the AWX host was pointed at |
| `phase` | `Pending` (never ran, waiting on the VM) / `Running` / `Succeeded` / `Failed` |
| `awxHostID`, `awxHostName`, `awxInventoryID`, `awxHostCreated` | The AWX inventory host, the name and inventory it lives under, and whether this controller owns it (adopted hosts are never deleted) |
| `lastJobID`, `lastJobURL`, `lastJobStatus` | The most recent run and a link to its output in AWX |
| `lastJobType`, `lastJobConnection` | Which template kind and which AWX instance that run was launched against, so it is still polled correctly if the binding is repointed while it is in flight. Secret *references* only - no token or CA contents are ever copied into status |
| `deprovision` | How far the `onDeleted` hook has got while the child is terminating: its phase, the AWX job, the targeting it started under, and any `ansible_connection` override still to be undone |
| `appliedGeneration`, `appliedTrigger` | What this VM last launched a run for - how re-run requests are tracked per VM |
| `awxEndpoint` | Fingerprint of the AWX instance the ids above came from. Repoint the `AWXConnection` at a different instance and those ids belong to unrelated objects there, so the child forgets them and looks its host up by name again rather than acting on them |
| `lastHostCheck` | When the inventory host was last reconciled against AWX. The next check is due `host_check_period` after it; passes in between cost no AWX requests at all |
| `history` | Bounded log of recent runs (one entry per run) |

`spec.bindingName` and `spec.bindingUID` say which binding, and which incarnation of it, holds the claim; both are immutable, as is `spec.vmName`. A binding deleted and recreated under the same name claims its VMs afresh rather than inheriting whatever the previous one left mid-cleanup.

Each child's `ownerReference` points at its `VirtualMachine`, so deleting a VM removes its child through ordinary garbage collection. That reference is also required: a child whose owner is missing, or names a different VM, is refused rather than reconciled, so a hand-written `AnsibleBindingVM` cannot launch playbooks nothing would ever clean up. A VM deleted and recreated under the same name holds its claim across the replacement: the new VM waits for the old child's cleanup to finish before its own can be created. A child whose VM merely stops matching the selector is deleted by the binding instead. Either way the child's own finalizer cleans up the AWX host first.

`AnsibleRun` tracks its single job directly, with no per-VM array:

| Field | Meaning |
|---|---|
| `jobID`, `jobURL`, `jobStatus` | The one AWX job, a link to its output, and AWX's own status word for it |
| `startedAt`, `finishedAt` | `finishedAt` is set only on a terminal outcome, and is what the TTL counts from |
| `launchAttemptedAt` | Written before the launch request goes out. Set with no `jobID` means the outcome was lost |
| `failureReason` | Why a terminal failure happened, kept separately so it survives later reconciles |
| `resolvedVars` | The variable names `varsFrom` produced. Names only |
| `hosts[]` | Inventory hosts this run touched, with `awxHostCreated` marking the ones it owns |

## Upgrading to the next release

**This upgrade is not transparent, and the controller will refuse to start until it is done.** `AnsibleBindingVM`s are now named after their VM alone, because that name is what makes one binding's ownership of a VM exclusive. Children from earlier versions are named after the binding *and* the VM, so running the new controller alongside them would create a second owner for VMs that already have one, and could launch a second provisioning job against a machine already being configured.

The controller checks for them at startup, before any worker runs and before it talks to AWX, and exits with the list rather than starting. To upgrade:

1. Stop anything that recreates bindings - GitOps included.
2. Run the **previous** controller version and let it finish, or explicitly resolve, any job still in flight.
3. Delete the `AnsibleBinding`s so their children and finalizers complete. `cleanupPolicy: Delete` removes their AWX hosts; `Retain` keeps the hosts and their ownership markers.
4. Deploy this version, then recreate the bindings.

Recreated bindings provision again, exactly as the 1.0.x upgrade did. Do not remove finalizers or delete children by hand to get past the check: that abandons AWX hosts and job state the controller can no longer account for. Rolling back after canonical children exist needs the same drain in reverse, not just an image downgrade.

The child schema now also declares `x-kubernetes-validations` on the identity fields, which needs a Kubernetes 1.25 or newer API server.

## Upgrading from 1.0.x

1.1.0 moves the per-VM detail out of the binding's status and onto one `AnsibleBindingVM` per matched VM. The upgrade needs no action, with one thing worth knowing: **each matched VM re-runs its playbook once.** A child starts with no record of what that VM last ran, so it launches. Ansible playbooks are expected to be idempotent, which is what makes that survivable - but if a run is expensive or disruptive, expect it, or set `cleanupPolicy: Retain` and recreate the bindings when you are ready for the runs.

Inventory hosts are **not** affected. Ownership lives in the AWX host's description, keyed to the namespace and binding, so each child adopts the host 1.0.x created rather than creating a second one, and a host whose VM had already stopped matching is picked up by the reaper described above. The old `status.vms[]` on existing bindings is pruned by the API server on the first status write.

## Uninstalling

Delete your `AnsibleBinding` and `AnsibleRun` resources **while the controller is still running**, then remove the service. Each one carries a cleanup finalizer, so if the controller is gone first, the CRs hang in `Terminating` and deleting the CRD blocks indefinitely. (`AWXConnection` has no finalizer - it creates nothing outside Kubernetes - so it deletes whether the controller is running or not.) Recovery is to reinstall the controller and let it drain them, or to strip the finalizers by hand:

```bash
kubectl patch ansiblebinding <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch ansiblerun <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch ansiblebindingvm <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":[]}}'
```

Note that stripping finalizers skips AWX cleanup, leaving host entries behind - [how to find them](FAQ.md#how-do-i-find-awx-hosts-a-supervisor-left-behind).
