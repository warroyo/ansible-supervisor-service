# Ansible Supervisor Service

![Release Status](https://github.com/warroyo/ansible-supervisor-service/actions/workflows/build-release.yml/badge.svg)

Supervisor service that binds VM Service `VirtualMachine`s to AWX/Tower job and workflow templates - the day-2 "run a playbook against this VM" experience Aria Automation's Ansible Automation Platform integration provides for classic vRA deployments, reproduced for Supervisor-native VMs.

- [Quickstart](QUICKSTART.md) - shortest path to a first working run
- [How it works](#how-it-works)
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

Two CRDs, both namespace-scoped (this is a multi-tenant environment: each tenant namespace owns its own AWX connection and credentials, nothing is shared across namespaces):

**AWXConnection**: points at an AWX/Tower instance and a `Secret` holding its API token. A namespace can define more than one (e.g. dev/prod AWX instances).

**AnsibleBinding**: a persistent binding from a `vmSelector` (matchLabels against `VirtualMachine`s in the same namespace) to an AWX job or workflow template. Re-runnable: bump the `ansible.field.vmware.com/reconcile-requested-at` annotation, or edit the spec, and the controller launches a fresh run against every currently-matched VM. One CR fans out to every VM the selector matches, each with its own `AnsibleBindingVM` carrying that VM's phase, job URL and bounded run history.

For each matched, powered-on VM with a reported IP, the controller upserts an AWX inventory host (into whatever inventory the target template is already configured with) and launches the template scoped to that host via `--limit`. VMs that drop out of the selector - deleted, relabeled - have their AWX host cleaned up on the very next reconcile, not left stale (a stale host's IP can get reassigned to an unrelated VM later); set `cleanupPolicy: Retain` to opt out if you manage AWX inventory by hand.

A few guardrails worth knowing about:

- **`vmSelector` may not be empty.** An empty selector would match every VM in the namespace, so it's rejected by the CRD schema and by the controller.
- **Pre-existing AWX hosts are adopted, never hijacked.** If a host with the target name already exists in the inventory, the controller merges its variables in rather than overwriting them, records that it did not create the host (`awxHostCreated: false` on that VM's `AnsibleBindingVM`), and never deletes it during cleanup. If that host's existing variables aren't a JSON object it can safely merge into, it refuses rather than destroying them.
- **Hosts owned by another supervisor are refused outright.** See [Can several supervisors share one AWX instance?](FAQ.md#can-several-supervisors-share-one-awx-instance)
- **The inventory host is reconciled against AWX, not against status.** Every `host_check_period` (600s by default) the host is read back from AWX, so one deleted or hand-edited in the AWX UI is recreated or repaired. Trusting the controller's own record instead would leave a deleted host undetected, and every later run failing with `--limit does not match any hosts` with nothing to repair it. Variables the controller doesn't manage are left untouched; a steady state writes nothing, and between checks an idle VM costs AWX nothing at all. A spec change or a re-run request is not on that timer - both take effect on the next pass.
- **Hosts nothing accounts for are reaped.** Less often (four host-check periods), each binding lists the AWX hosts carrying its own ownership marker and deletes any that no VM and no child accounts for - what a controller killed mid-cleanup leaves behind. Only hosts this supervisor created for that binding are ever considered, and `cleanupPolicy: Retain` disables it.
- **In-flight runs are tracked independently of the VM.** A VM powering off mid-run doesn't lose the job, and re-run requests made during downtime aren't swallowed. See [the FAQ](FAQ.md#what-happens-to-in-flight-runs-when-a-vm-powers-off).

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
| `namespace`     | `""`    | Namespace to deploy into (filled by the supervisor, do not edit) |
| `supervisor_id` | `""`    | Identity stamped on AWX hosts this supervisor owns. Empty derives it from the `kube-system` namespace UID - set something readable (e.g. `sup-lab-01`) if you share one AWX between supervisors and want its inventory legible |

## Usage

Three manifests, applied in this order, are a complete working setup - `examples/` holds all of them:

| File | What it creates |
|---|---|
| `examples/virtualMachine.yml` | a target VM, labelled for a `vmSelector` and cloud-init'd with the SSH key AWX will log in with |
| `examples/awxConnection.yml` | the `Secret` holding an AWX API token, and the `AWXConnection` pointing at the instance |
| `examples/ansibleBinding.yml` | the binding itself: which VMs, which template |

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

Cleanup is retried rather than leaked: a host that can't be deleted keeps its `AnsibleBindingVM` in `Terminating` until the deletion succeeds, and deleting an `AnsibleBinding` blocks on its finalizer until every child has finished and AWX has confirmed the hosts are gone. A temporary failure - AWX unreachable, the API server erroring - is retried indefinitely rather than abandoned, since an abandoned host keeps an IP that AWX may later hand to an unrelated VM. Only a permanently unrecoverable case gives up: the `AWXConnection` or its Secret has been deleted, or is malformed beyond retrying, so there's no way left to reach AWX; that's logged and the CR finishes deleting. To release a binding whose AWX instance is gone for good, set `cleanupPolicy: Retain` on it - that's honored even while it's terminating.

## CRD status

Both CRDs track reconciliation state in `.status`:

| State     | Description |
|-----------|-------------|
| `Pending` | Not yet able to run, or not yet done: provisioning hasn't run, no VM matches `vmSelector`, a matched VM is powered off / has no reported IP, or a matched VM has not yet completed the run this generation asked for |
| `Running` | At least one VM's run is still in flight in AWX |
| `Ready`   | Every currently-matched VM completed the requested run successfully |
| `Failed`  | A reconciliation error, or a run that AWX reported as failed. See `message`, and `status.summary.failedVMs` for which specific VM(s) |
| `Terminating` | Every matched VM is gone, but a child is still being cleaned up - typically an AWX host that will not delete. `status.summary.terminating` counts them |

An `AnsibleBinding`'s state is aggregated from its children's outcomes, not just from whether the last reconcile threw an error - a job that AWX ran and failed is not a controller error, but it is not `Ready` either.

```bash
kubectl get awxconnection <name> -n <namespace> -o jsonpath='{.status}'
kubectl get ansiblebinding <name> -n <namespace> -o jsonpath='{.status}'
```

`AnsibleBinding` rolls its children up into `status.summary`, which is a fixed size however many VMs the selector matches:

| Field | Meaning |
|---|---|
| `total` | VMs currently matched by `vmSelector` |
| `succeeded`, `running`, `pending`, `failed` | How many children are in each phase |
| `failedVMs`, `firstFailure` | A bounded sample of the failing VMs and one of their messages, so "why is this binding red" is answerable without listing the children |
| `terminating` | Children still being deleted. Counted apart from the phases and not included in `total`, so a child stuck on an AWX host that will not delete is visible rather than silently absent |

The per-VM detail lives on one `AnsibleBindingVM` per matched VM, named `<binding>-<vm>-<hash>` - the hash of the pair keeps two bindings whose names concatenate the same way from colliding, and keeps the name inside the 253-character limit when both halves are long. The controller creates and deletes these; they are not something to write by hand. Look them up by label rather than by name:

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
| `appliedGeneration`, `appliedTrigger` | What this VM last launched a run for - how re-run requests are tracked per VM |
| `awxEndpoint` | Fingerprint of the AWX instance the ids above came from. Repoint the `AWXConnection` at a different instance and those ids belong to unrelated objects there, so the child forgets them and looks its host up by name again rather than acting on them |
| `lastHostCheck` | When the inventory host was last reconciled against AWX. The next check is due `host_check_period` after it; passes in between cost no AWX requests at all |
| `history` | Bounded log of recent runs (one entry per run) |

Each child's `ownerReference` points at its `VirtualMachine`, so deleting a VM removes its child through ordinary garbage collection. That reference is also required: a child whose owner is missing, or names a different VM, is refused rather than reconciled, so a hand-written `AnsibleBindingVM` cannot launch playbooks nothing would ever clean up. A child whose VM merely stops matching the selector is deleted by the binding instead. Either way the child's own finalizer cleans up the AWX host first.

## Upgrading from 1.0.x

1.1.0 moves the per-VM detail out of the binding's status and onto one `AnsibleBindingVM` per matched VM. The upgrade needs no action, with one thing worth knowing: **each matched VM re-runs its playbook once.** A child starts with no record of what that VM last ran, so it launches. Ansible playbooks are expected to be idempotent, which is what makes that survivable - but if a run is expensive or disruptive, expect it, or set `cleanupPolicy: Retain` and recreate the bindings when you are ready for the runs.

Inventory hosts are **not** affected. Ownership lives in the AWX host's description, keyed to the namespace and binding, so each child adopts the host 1.0.x created rather than creating a second one, and a host whose VM had already stopped matching is picked up by the reaper described above. The old `status.vms[]` on existing bindings is pruned by the API server on the first status write.

## Uninstalling

Delete your `AnsibleBinding` resources **while the controller is still running**, then remove the service. Each one carries a cleanup finalizer, so if the controller is gone first, the CRs hang in `Terminating` and deleting the CRD blocks indefinitely. (`AWXConnection` has no finalizer - it creates nothing outside Kubernetes - so it deletes whether the controller is running or not.) Recovery is to reinstall the controller and let it drain them, or to strip the finalizers by hand:

```bash
kubectl patch ansiblebinding <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch ansiblebindingvm <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":[]}}'
```

Note that stripping finalizers skips AWX cleanup, leaving host entries behind - [how to find them](FAQ.md#how-do-i-find-awx-hosts-a-supervisor-left-behind).
