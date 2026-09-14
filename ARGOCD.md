# Argo CD

An `AnsibleRun` is deliberately Job-shaped, with an immutable spec, one AWX job, run once and never again, and optionally self-collecting. So most of what you already know about running a `Job` from Argo CD transfers directly, including the two mechanisms people mix up, resources and hooks, and which one an execution belongs in.

One thing doesn't transfer, and it's the thing that fails silently: **Argo has a built-in health check for `batch/v1 Job` and none for these CRDs.** An unknown custom resource is reported `Healthy` the moment it's created. Without the [health checks below](#1-install-the-health-checks), a sync wave won't wait for a playbook, a `PostSync` hook won't gate on it, and the application goes green while the job is still queued in AWX.

- [One-time setup](#one-time-setup)
- [Resource or hook?](#resource-or-hook)
- [Immutability, and what `Replace=true` does not fix](#immutability-and-what-replacetrue-does-not-fix)
- [Never set a TTL on a run Argo tracks](#never-set-a-ttl-on-a-run-argo-tracks)
- [Sync waves, and why `varsFrom` softens them](#sync-waves-and-why-varsfrom-softens-them)
- [Explicit hosts, and what a run may write](#explicit-hosts-and-what-a-run-may-write)
- [Decommissioning: `PostDelete` is too late](#decommissioning-postdelete-is-too-late)
- [Scenarios](#scenarios) - nine worked examples
- [Teardown](#teardown)
- [Troubleshooting](#troubleshooting)

Runnable manifests for the shapes below are in [`examples/argocd/`](examples/argocd/): the platform configuration, a tracked once-only run, a `PostSync` smoke test, and externally sequenced teardown.

## One-time setup

This is done once by whoever administers Argo CD rather than by every application. It's the *platform* team rather than a tenant, since `argocd-cm` lives in Argo's own namespace, so a tenant deploying into their Supervisor namespace can't add these themselves. Install them alongside the service.

### 1. Install the health checks

Add to the `argocd-cm` ConfigMap. [`examples/argocd/platform/argocd-cm.yml`](examples/argocd/platform/argocd-cm.yml) is this same text as an applyable file. The state machine these read is the one documented under [CRD status](README.md#crd-status), and it lines up with Argo's health model exactly. `Failed` is reserved for a *terminal* outcome, and a run still retrying a transient problem stays `Pending` or `Running` with the error in `message`. That's the difference between `Degraded` and `Progressing`, already made for you.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-cm
  namespace: argocd
data:
  resource.customizations.health.field.vmware.com_AnsibleRun: |
    local hs = {}
    if obj.status == nil or obj.status.state == nil then
      hs.status = "Progressing"
      hs.message = "Waiting for the controller"
      return hs
    end
    hs.message = obj.status.message or obj.status.state
    if obj.status.state == "Ready" then
      hs.status = "Healthy"
    elseif obj.status.state == "Failed" then
      -- Terminal only: an AnsibleRun that is still retrying reports
      -- Pending or Running, so this really does mean the run is over.
      hs.status = "Degraded"
    else
      hs.status = "Progressing"
    end
    return hs

  resource.customizations.health.field.vmware.com_AnsibleBinding: |
    local hs = {}
    if obj.status == nil or obj.status.state == nil then
      hs.status = "Progressing"
      hs.message = "Waiting for the controller"
      return hs
    end
    hs.message = obj.status.message or obj.status.state
    if obj.status.state == "Ready" then
      hs.status = "Healthy"
    elseif obj.status.state == "Failed" then
      hs.status = "Degraded"
    else
      -- Pending is normal and can persist: no VM matches the selector
      -- yet, or a matched VM is powered off. It is not a failure.
      hs.status = "Progressing"
    end
    return hs

  resource.customizations.health.field.vmware.com_AWXConnection: |
    local hs = {}
    if obj.status == nil or obj.status.state == nil then
      hs.status = "Progressing"
      hs.message = "Waiting for validation"
      return hs
    end
    hs.message = obj.status.message or obj.status.state
    if obj.status.ready == true then
      hs.status = "Healthy"
    elseif obj.status.state == "Failed" then
      hs.status = "Degraded"
    else
      hs.status = "Progressing"
    end
    return hs
```

A `VirtualMachine` is worth adding too, since it's what a wave usually needs to wait for and it's equally unknown to Argo. The controller needs the VM's reported IP, so that, rather than merely `PoweredOn`, is what health should mean here:

```yaml
  resource.customizations.health.vmoperator.vmware.com_VirtualMachine: |
    local hs = {}
    local ip = nil
    if obj.status ~= nil then
      if obj.status.network ~= nil then ip = obj.status.network.primaryIP4 end
      -- v1alpha1 served this flat, as status.vmIp
      if ip == nil then ip = obj.status.vmIp end
    end
    if ip ~= nil and ip ~= "" then
      hs.status = "Healthy"
      hs.message = "IP " .. ip
    else
      hs.status = "Progressing"
      hs.message = "Waiting for a reported IP"
    end
    return hs
```

Confirm they took, on a live object:

```bash
argocd app resources <app> --output tree
kubectl get ansiblerun -n <namespace>     # READY / STATE / JOB columns
```

### 2. Create the AWXConnection out of band

Don't put the `Secret` holding the AWX API token in the same Git repo as the application. The connection and its Secret are per-namespace, created once by the platform team, exactly as in the [VCFA setup](VCFA-BLUEPRINTS.md#one-time-setup), or through whatever sealed-secret / external-secret mechanism your Argo already uses. Applications reference it by name with `awxConnectionRef` and never carry the credential.

## Resource or hook?

Argo has two ways to run something, and an `AnsibleRun` fits both, differently.

| | Tracked resource | Hook |
|---|---|---|
| Declared as | an ordinary manifest in the app | `argocd.argoproj.io/hook: PreSync\|Sync\|PostSync\|SyncFail` |
| Drift-tracked | yes - a missing run is `OutOfSync` and gets recreated | no |
| Named | fixed `metadata.name` | usually `generateName:`, so each sync makes a new object |
| Runs again | only if the name changes | every sync |
| Cleaned up | by prune, on app delete | `hook-delete-policy` |

The rule that decides it is the same one that decides [`AnsibleBinding` or `AnsibleRun`](README.md#ansiblebinding-or-ansiblerun), one level up, which is whether this execution is part of the application's desired state or an event in its deployment.

| What you want | Shape |
|---|---|
| Run once when the app is first deployed, alongside the VM | tracked resource, sync wave after the VM, **no TTL**, stable name |
| Run on every sync | `hook: PostSync`, `generateName:`, `hook-delete-policy: HookSucceeded` |
| Run again only when an input changes | tracked resource with the varying value in `metadata.name` |
| Keep VMs configured, forever, re-runnable on demand | not a run at all - an `AnsibleBinding`, plain tracked resource |

The last row is worth stating outright. A binding is standing desired state, which is precisely what Argo is for. Most of what an application wants is a binding, and a binding needs none of the machinery below. It's an ordinary manifest that Argo applies, diffs, and prunes like any other. Everything from here down is about runs.

Here's a `PostSync` hook, with the health check installed:

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  generateName: smoke-test-        # a fresh object every sync
  namespace: my-namespace
  annotations:
    argocd.argoproj.io/hook: PostSync
    argocd.argoproj.io/hook-delete-policy: HookSucceeded
spec:
  awxConnectionRef: awx
  template:
    name: "Smoke Test"
    type: JobTemplate
  vmRef:
    name: webserver
  activeDeadlineSeconds: 900       # what eventually turns the sync red
```

Two things make that work. `generateName` sidesteps immutability entirely, since Argo never patches this object and creates a new one instead, and `activeDeadlineSeconds` is what stops a run that can never launch from leaving the sync `Progressing` forever. Set it on every hook.

## Immutability, and what `Replace=true` does not fix

An `AnsibleRun`'s spec is immutable, enforced by the CRD itself:

```yaml
x-kubernetes-validations:
  - rule: "self == oldSelf"
    message: spec is immutable - an AnsibleRun executes once; create a new one to run again
```

That's stricter than a `Job`, and it changes the usual advice. Argo's standard escape hatch for immutable fields is `argocd.argoproj.io/sync-options: Replace=true`, but a replace is still an update, so the API server rejects it the same way. Only `Replace=true` **plus** `Force=true` gets through, because that degrades to delete-then-create, which re-launches a playbook that already ran. For a smoke test that's harmless, but for a decommission or a ticket-open it's an incident.

So don't reach for `Replace`. Use one of the two shapes that never produce a diff in the first place:

- **A hook with `generateName`.** Nothing is ever patched.
- **A tracked resource whose spec does not derive from a mutable input.** If a value can change, put it in the name instead, so a change creates a *new* resource rather than patching the old one:

  ```yaml
  metadata:
    name: dns-webserver-corp-example-com
  ```

  This is the same guidance as for [VCFA blueprints](VCFA-BLUEPRINTS.md#decommissioning-in-the-right-order), and it's also how you deliberately get "re-run when this input changes" out of a resource that otherwise never runs twice. With Argo it needs `prune: true`, or the superseded run lingers (see [what a run may write](#explicit-hosts-and-what-a-run-may-write)).

## Never set a TTL on a run Argo tracks

`ttlSecondsAfterFinished` makes the CR delete itself once it reaches a terminal state. Against a tracked resource that's a loop:

1. The run finishes and collects itself.
2. Argo sees the resource missing and reports `OutOfSync`.
3. With `selfHeal: true`, Argo recreates it.
4. A brand-new `AnsibleRun` launches the playbook a second time. Go to 1.

This is the same trap as [a blueprint-owned run](VCFA-BLUEPRINTS.md#decommissioning-in-the-right-order), except that automated sync makes it recur rather than merely confuse the next update. The rule is pretty simple:

| Who owns the run | TTL |
|---|---|
| Tracked resource in an Argo app | leave unset - prune removes it with the app |
| Argo hook | leave unset - use `hook-delete-policy: HookSucceeded` |
| Created by hand or by an external orchestrator | set it; nothing else is tracking the object |

Deleting the CR is what deletes the AWX hosts it created, so letting Argo do the deleting loses nothing.

## Sync waves, and why `varsFrom` softens them

With the health checks installed, waves work as expected:

```yaml
metadata:
  annotations:
    argocd.argoproj.io/sync-wave: "1"     # VM is wave 0
```

Argo will not start wave 1 until every wave 0 resource is `Healthy`, and the `VirtualMachine` check above only reports healthy once an IP is reported, which is exactly the condition the controller is waiting on anyway.

That said, waves are a convenience here rather than a requirement, and it's worth knowing why. A run that references an object that isn't ready sits `Pending` and retries rather than failing. So the run can be created in the same wave as the VM and simply wait. This matters most with `varsFrom`, which reads live objects at launch time:

```yaml
  varsFrom:
    - resource:
        apiVersion: vmoperator.vmware.com/v1alpha5
        kind: VirtualMachine
        name: webserver
      vars:
        record_ip: "{.status.network.primaryIP4}"
```

The alternative, templating the IP into `extraVars` from Git, isn't available at all, because Git doesn't know it. `varsFrom` is what makes an Argo-managed run possible for anything that depends on runtime state, and the wave is only there to make the first attempt succeed rather than retry a few times. `activeDeadlineSeconds` is what bounds the waiting, and without it a reference that never resolves waits forever and the app sits `Progressing` with no explanation.

## Explicit hosts, and what a run may write

A run borrows the inventory rather than owning it. It **executes against** an existing AWX host without owning it, so the host's variables, address, groups, and ownership marker come out of the run exactly as they went in, and deleting the run leaves it alone. That holds whether the host was made by hand, by an `AnsibleBindingVM`, or by an earlier run.

A run only writes to a host it created itself, meaning one that was not there when it looked.

That single rule decides every repeating-hook shape:

| Setup | Second sync |
|---|---|
| Names only, no `address` and no `variables` | Fine, whatever the host's history. The run resolves it, scopes the job to it, and changes nothing. |
| `generateName` hook, `cleanupPolicy: Delete` (default), `hook-delete-policy: HookSucceeded` | Fine. The old run's deletion takes the host it created with it, so the new run creates a fresh one. |
| `address` or `variables` on a host that already exists | **Refused, before anything launches.** The run cannot apply them and will not pretend it did. Move the values into `extraVars`/`varsFrom`, or drop them and let the host keep its own. |
| A retained or superseded run's host, plus `address`/`variables` on the new run | Same refusal, and the usual way to meet it: the previous host outlived the run that made it. |

Per-execution values belong in `extraVars` and `varsFrom`. AWX passes those as the job's `extra_vars`, which Ansible ranks above inventory variables. So the run gets its value for that job without the inventory ever changing, and the next job on that host sees the inventory's own value again.

A run with `vmRef`, or with no target at all, needs none of this. A VM-derived host is named from the VM and shared with whatever binding manages it, and a run with neither `hosts` nor `vmRef` doesn't touch the inventory at all. That's one more reason the `hosts: localhost` shape is the easiest thing to drive from Argo.

## Decommissioning: `PostDelete` is too late

There's no pre-delete hook *this service* can use. The mechanism exists in VM Service but is restricted to privileged accounts ([the FAQ explains why](FAQ.md#why-is-there-no-pre-delete-hook)).

Argo's own hooks are `PreSync`, `Sync`, `PostSync`, `SyncFail`, and (2.10+) `PostDelete`, which runs **after** the application's resources have been deleted. By then the VM is gone and the guest is unreachable. Newer Argo CD documents a `PreDelete` hook as well, which would fire before the application's resources (the `VirtualMachine` among them) are removed, and that's the right shape for a guest decommission.

**This repo doesn't pin a minimum version for `PreDelete`, and doesn't test it.** Check your own Argo CD's documentation and behavior before relying on it, because the failure mode of getting it wrong is a hook that never runs and a guest destroyed with its decommission playbook unexecuted. The [external sequencing](#9-decommission-a-guest-before-the-vm-is-destroyed) below works on every version and is what the suite covers.

So the limitation carries over intact:

- **A decommission playbook that logs into the guest cannot be an Argo hook.** It has to run while the VM is still up, which means sequencing from outside Argo: create the `AnsibleRun`, wait on `.status.state`, then delete the application. Same shapes as [VCFA](VCFA-BLUEPRINTS.md#decommissioning-in-the-right-order).
- **Cleanup that doesn't need the guest can be a `PostDelete` hook**, and this is a genuinely good fit. Deregistering DNS, closing a CMDB record, or removing a monitoring target are all `hosts: localhost` playbooks against an external API, with no `vmRef` and no `hosts`:

  ```yaml
  metadata:
    generateName: dns-deregister-
    annotations:
      argocd.argoproj.io/hook: PostDelete
      argocd.argoproj.io/hook-delete-policy: HookSucceeded
  spec:
    template:
      name: "Deregister DNS Record"
      type: JobTemplate
    extraVars:
      record_name: webserver
      zone: corp.example.com
      record_state: absent
  ```

  Note that `varsFrom` is no use here, since the VM it would read is already deleted, so a `PostDelete` run must carry everything it needs as literals. If the value it needs is only known at runtime, capture it into the manifest at provisioning time, or accept that this has to be sequenced externally too.

## Scenarios

These nine shapes cover essentially everything people ask for. All of them assume the [health checks](#1-install-the-health-checks) are installed and that the `AWXConnection` named `awx` already exists in the namespace, [created out of band](#2-create-the-awxconnection-out-of-band).

| # | Scenario | Shape |
|---|---|---|
| [1](#1-keep-a-tier-configured) | Keep a tier configured, re-runnable on demand | `AnsibleBinding`, plain tracked resource |
| [2](#2-register-dns-when-the-app-is-first-deployed) | Register DNS at provision time | tracked `AnsibleRun`, sync wave, `varsFrom` |
| [3](#3-smoke-test-after-every-sync) | Smoke test after every sync | `PostSync` hook, `generateName` |
| [4](#4-open-a-change-ticket-before-the-sync-starts) | Open a change ticket before anything changes | `PreSync` hook, no target |
| [5](#5-re-run-only-when-an-input-changes) | Re-run only when an input changes | tracked run, value in `metadata.name` |
| [6](#6-patch-bare-metal-hosts-on-every-sync) | Patch bare-metal hosts on every sync | hook with `spec.hosts`, adopted hosts |
| [7](#7-deregister-dns-when-the-app-is-deleted) | Deregister DNS on app delete | `PostDelete` hook, literals only |
| [8](#8-page-someone-when-a-sync-fails) | Page someone when a sync fails | `SyncFail` hook |
| [9](#9-decommission-a-guest-before-the-vm-is-destroyed) | Decommission a guest before the VM dies | **not an Argo shape** - sequenced outside |

### 1. Keep a tier configured

This is the most common thing, and the one that needs none of the machinery in this document. A binding is standing desired state, which is what Argo is for, so it's an ordinary manifest that Argo applies, diffs and prunes.

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: webserver-config
  namespace: my-namespace
spec:
  awxConnectionRef: awx
  vmSelector:
    app: webserver
    deployment: shop-frontend    # scope per app, not per tier - see below
  template:
    name: "Configure Webserver"
    type: JobTemplate
  extraVars:
    nginx_worker_processes: "4"
    app_version: "2.4.1"
```

Editing `app_version` in Git and syncing re-runs the playbook against every matched VM, since the spec changed and the controller relaunches. That's the whole day-2 story, and it's a Git commit.

**Scope the selector to this application.** `vmSelector` reaches every matching VM in the namespace, including VMs belonging to other Argo apps in the same namespace. `app: webserver` alone will find someone else's webservers. Label per application and match on that label. This is the same warning as for [VCFA blueprints](VCFA-BLUEPRINTS.md#scope-the-selector-to-the-deployment), and it matters more here because several apps commonly share one Supervisor namespace.

To force a re-run without changing the spec, bump the annotation in Git:

```yaml
  annotations:
    ansible.field.vmware.com/reconcile-requested-at: "2026-09-04T10:00:00Z"
```

### 2. Register DNS when the app is first deployed

This is one execution, part of the application's desired state. It's a tracked resource, so Argo prunes it when the app goes away.

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: dns-register-webserver
  namespace: my-namespace
  annotations:
    argocd.argoproj.io/sync-wave: "1"        # VirtualMachine is wave 0
spec:
  awxConnectionRef: awx
  template:
    name: "Register DNS Record"
    type: JobTemplate
  # No vmRef and no hosts: the playbook is hosts: localhost and calls the
  # Infoblox API. Nothing goes in the inventory, no --limit is sent.
  extraVars:
    zone: corp.example.com
    record_state: present
  varsFrom:
    - resource:
        apiVersion: vmoperator.vmware.com/v1alpha5
        kind: VirtualMachine
        name: webserver
      vars:
        record_name: "{.metadata.name}"
        record_ip: "{.status.network.primaryIP4}"
  activeDeadlineSeconds: 600
  # No ttlSecondsAfterFinished - Argo owns this object's lifecycle.
```

**`varsFrom` is doing the essential work here.** The IP isn't knowable at commit time, so it can't be an `extraVars` literal from Git. Reading it off the live object means the run sits `Pending` and retries until the VM reports an IP. The sync wave is only there to make the first attempt succeed instead of retrying a few times, and `activeDeadlineSeconds` is what stops it waiting forever if the VM never comes up.

**The Infoblox credential is an AWX Credential on the template**, rather than anything in this manifest. `varsFrom` [refuses to read a Secret](FAQ.md#why-does-varsfrom-refuse-to-read-a-secret) precisely so nobody tries to route a token through Git and into `extra_vars`, where AWX would echo it in job output.

### 3. Smoke test after every sync

This runs on every sync, gates the sync on the result, and cleans itself up.

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  generateName: smoke-test-              # fresh object each sync
  namespace: my-namespace
  annotations:
    argocd.argoproj.io/hook: PostSync
    argocd.argoproj.io/hook-delete-policy: HookSucceeded
spec:
  awxConnectionRef: awx
  template:
    name: "Smoke Test"
    type: JobTemplate
  vmRef:
    name: webserver                      # inventory host from the VM's IP, --limit to it
  extraVars:
    expected_version: "2.4.1"
  activeDeadlineSeconds: 900
```

`generateName` is what makes this legal. Argo never patches this object and creates a new one each time, so the [immutable spec](#immutability-and-what-replacetrue-does-not-fix) is never an issue. `HookSucceeded` deletes it on success and **keeps it on failure**, which is what you want, since the failed run stays in the namespace with `status.jobURL` pointing at the AWX output.

With the health check installed, a `Failed` run reports `Degraded` and the sync fails. Without it, the sync goes green regardless. This is the scenario where the missing health check hurts most.

### 4. Open a change ticket before the sync starts

`PreSync` runs before Argo applies anything, so the run can't reference application resources, since on a first deploy they don't exist yet. That's fine, because this shape targets nothing at all.

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  generateName: change-ticket-
  namespace: my-namespace
  annotations:
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/hook-delete-policy: HookSucceeded
spec:
  awxConnectionRef: awx
  template:
    name: "Open Change Ticket"
    type: JobTemplate
  extraVars:
    summary: "shop-frontend deploy"
    requested_by: platform-team
    change_type: standard
  activeDeadlineSeconds: 300
```

If the ticket system refuses, the run goes `Failed`, the `PreSync` hook fails, and **Argo never applies the sync**. That gives you a real change gate out of a job template and an annotation.

### 5. Re-run only when an input changes

This is a tracked run whose spec must not change, but which should execute again when a value does. Put the varying value in the name, so a change creates a new object instead of patching the old one, which the API server would reject.

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  # Rendered by Helm/Kustomize. Changing .Values.appVersion produces a
  # different name, so Argo creates a new run and prunes the old one.
  name: deploy-shop-frontend-2-4-1
  namespace: my-namespace
spec:
  awxConnectionRef: awx
  template:
    name: "Deploy Application"
    type: JobTemplate
  vmRef:
    name: webserver
  extraVars:
    app_version: "2.4.1"
  activeDeadlineSeconds: 1800
```

```yaml
# Helm equivalent
metadata:
  name: deploy-shop-frontend-{{ .Values.appVersion | replace "." "-" }}
```

**`prune: true` is required here rather than optional.** Without it, the superseded runs pile up, and each one holds onto any inventory host it created. The next run will happily execute against that host, but it can't write an `address` or `variables` onto it. See [what a run may write](#explicit-hosts-and-what-a-run-may-write).

The name still has to be a legal Kubernetes object name, so 253 characters, lowercase alphanumeric plus `-` and `.`. A version string with `+build` metadata in it will be rejected, so sanitize it as above.

### 6. Patch bare-metal hosts on every sync

`spec.hosts` reaches machines this Supervisor does not own. Combined with a repeating hook, this is where [what a run may write](#explicit-hosts-and-what-a-run-may-write) matters most, so there are two safe forms and one that breaks.

**Safe form A - let each run clean up after itself** (default `cleanupPolicy`):

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  generateName: patch-db-
  namespace: my-namespace
  annotations:
    argocd.argoproj.io/hook: PostSync
    argocd.argoproj.io/hook-delete-policy: HookSucceeded
spec:
  awxConnectionRef: awx
  template:
    name: "Patch Linux Hosts"
    type: JobTemplate
  hosts:
    - name: db-prod-01
      address: 10.20.5.11
      variables:
        ansible_user: dbadmin
    - name: db-prod-02
      address: 10.20.5.12
      variables:
        ansible_user: dbadmin
  extraVars:
    package_name: openssl
  # cleanupPolicy defaults to Delete: the hosts this run created go away
  # with it, so the next sync's run creates them fresh under its own name.
  activeDeadlineSeconds: 1800
```

**Safe form B: pre-create the hosts in AWX, by hand or by whatever manages your inventory.** An existing host is resolved and used by every run, and written to by none of them. This is the better option when the machines are permanent inventory that happens to get patched, rather than something this service should own:

```yaml
spec:
  hosts:
    - name: db-prod-01        # already exists in AWX; omit address and its
    - name: db-prod-02        # ansible_host is left exactly as it is
```

**The form that breaks:** either safe form *plus* an `address` or `variables` on a host that's already there. That happens with `cleanupPolicy: Retain` and `generateName`, or with a tracked run that has a changing name and `prune: false`, both of which leave the previous run's host behind. The next sync is refused with `already exists and is not this run's to change`, and no job launches. The fix is the same either way: drop the host-level fields once the host exists, and put per-run values in `extraVars`.

Note that `hostNamePrefix` on the `AWXConnection` is **not** applied to `spec.hosts` entries. These are literals naming machines that usually already exist, and prefixing `db-prod-01` would create a duplicate and patch the wrong thing.

### 7. Deregister DNS when the app is deleted

`PostDelete` (Argo CD 2.10+) runs after the application's resources are gone. It's a good fit for cleanup that never needed the guest.

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  generateName: dns-deregister-
  namespace: my-namespace
  annotations:
    argocd.argoproj.io/hook: PostDelete
    argocd.argoproj.io/hook-delete-policy: HookSucceeded
spec:
  awxConnectionRef: awx
  template:
    name: "Deregister DNS Record"
    type: JobTemplate
  extraVars:
    record_name: webserver          # literals only - see below
    zone: corp.example.com
    record_state: absent
  activeDeadlineSeconds: 300
```

**`varsFrom` is useless here**, and this is the trap. By the time a `PostDelete` hook runs, the `VirtualMachine` it would read has already been deleted, so the run sits `Pending` until `activeDeadlineSeconds` kills it. Everything a `PostDelete` run needs must be a literal in the manifest. If the value is only knowable at runtime (a DHCP address, say), this can't be a `PostDelete` hook and has to be sequenced externally like scenario 9.

The `AWXConnection` also has to outlive the deletion. Keeping it [out of the application entirely](#2-create-the-awxconnection-out-of-band) is what guarantees that, and if it's in the app the hook may find it already gone.

### 8. Page someone when a sync fails

```yaml
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  generateName: notify-failure-
  namespace: my-namespace
  annotations:
    argocd.argoproj.io/hook: SyncFail
    argocd.argoproj.io/hook-delete-policy: HookSucceeded
spec:
  awxConnectionRef: awx
  template:
    name: "Notify Deploy Failure"
    type: JobTemplate
  extraVars:
    application: shop-frontend
    channel: "#platform-alerts"
  activeDeadlineSeconds: 120
```

This is only worth it if the paging path already lives in AWX. Argo's own notifications controller is the more direct tool. This is for when the escalation logic (who's on call, which severity, which downstream ticket) is already a playbook.

### 9. Decommission a guest before the VM is destroyed

**Sequence this from outside Argo unless you have verified `PreDelete` on your own Argo CD.** It's listed here because it's the thing people try first.

A decommission playbook that logs into the guest (flushing a queue, deregistering from a cluster, taking a final backup) has to run while the VM is still up. There's no pre-delete hook this service can use ([why](FAQ.md#why-is-there-no-pre-delete-hook)), and Argo's `PostDelete` runs after the VM is already destroyed. Newer Argo CD documents a `PreDelete` hook that fires early enough, but it's [not pinned or tested here](#decommissioning-postdelete-is-too-late), so what follows is the version-independent form. That's also what [`examples/argocd/external-teardown/`](examples/argocd/external-teardown/) implements.

What does **not** work:

- Putting the decommission `AnsibleRun` in the app as a tracked resource. It's created when the app is *deployed*, so the teardown playbook runs at provisioning time against a VM that probably has no IP yet, and then, being terminal, never runs again.
- A `PostDelete` hook with `vmRef`. The VM is gone, so the run cannot build an inventory host and waits out its deadline.
- A finalizer on the `VirtualMachine`. vm-operator's own finalizer destroys the vSphere VM during its finalization, so the playbook would SSH into nothing.

What works is sequencing it outside Argo, so run, wait, then delete:

```bash
kubectl apply -n my-namespace -f - <<'EOF'
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: decommission-webserver
spec:
  awxConnectionRef: awx
  template:
    name: "Decommission Host"
    type: JobTemplate
  vmRef:
    name: webserver
  activeDeadlineSeconds: 900
  ttlSecondsAfterFinished: 3600     # nothing else tracks this one, so a TTL is right
EOF

kubectl wait --for=jsonpath='{.status.state}'=Ready \
  ansiblerun/decommission-webserver -n my-namespace --timeout=15m

argocd app delete shop-frontend --cascade
```

Use `--for=jsonpath=` rather than `--for=condition=`, because these CRDs report state in `.status.state` and don't currently publish standard conditions. Note that `wait` won't return early on a `Failed` run, it waits out the timeout. So check the state afterwards before deleting anything:

```bash
kubectl get ansiblerun decommission-webserver -n my-namespace \
  -o jsonpath='{.status.state}{"\t"}{.status.message}{"\n"}'
```

This is a genuine regression from what `Cloud.Ansible.Tower` could do with `templates.de-provision[]` in a VM Apps organization, and it's worth being straight about rather than papering over. The ordering is the caller's problem now, in Argo exactly as [in VCFA](VCFA-BLUEPRINTS.md#decommissioning-in-the-right-order).

## Teardown

Deleting the application deletes the CRs, and each one's finalizer blocks until AWX confirms the inventory hosts it created are gone. That's deliberate ([why](README.md#ansiblebinding)), and it means an app delete can sit in `Terminating` while AWX is unreachable, rather than leaking a host whose IP AWX may later hand to an unrelated VM.

If AWX is gone for good and you need the app to finish deleting, the lever depends on the kind:

- An `AnsibleBinding` has a mutable spec, so `cleanupPolicy: Retain` can be set on it and is honored even while it is terminating. Sync before deleting.
- An `AnsibleRun` does **not**, since its spec is immutable and patching `cleanupPolicy` on one is rejected by the API server. What releases it is removing what it cleans up *through*, so delete the `AWXConnection` it names, or the `Secret` holding the token. A run that can no longer reach AWX at all abandons its hosts, logs which ids it left behind, and releases its finalizer. A run whose AWX is merely erroring keeps retrying, which is the intended difference.

Either way, don't strip finalizers, since that leaves hosts behind with no record of them. The FAQ has [how to find hosts a supervisor left behind](FAQ.md#how-do-i-find-awx-hosts-a-supervisor-left-behind).

Order matters on the way out. Argo deletes in reverse sync-wave order, so putting the `AWXConnection` and its `Secret` in an earlier wave than anything that uses them means they're deleted *last*, which is what the finalizers need. Better still, keep the connection out of the application entirely, per [one-time setup](#2-create-the-awxconnection-out-of-band).

## Troubleshooting

| Symptom | Cause |
|---|---|
| App goes `Healthy` immediately, playbook still running | Health checks not installed, or installed under the wrong key. The key is `resource.customizations.health.<group>_<Kind>` - group and kind, separated by an underscore |
| Sync hangs `Progressing` forever | A run stuck `Pending`: a `varsFrom` reference that never resolves, a VM with no IP, AWX unreachable. `status.message` says which. Set `activeDeadlineSeconds` so it eventually fails instead |
| `spec is immutable` on sync | Something patched a tracked run. Move the varying value into `metadata.name`, or make it a hook. Do not use `Replace=true` |
| App flips `OutOfSync` after a run finishes, then re-runs it | `ttlSecondsAfterFinished` set on a tracked resource. [Unset it](#never-set-a-ttl-on-a-run-argo-tracks) |
| `inventory host ... already exists and is not this run's to change` | The run asked to write an `address` or `variables` onto a host it does not own. [What a run may write](#explicit-hosts-and-what-a-run-may-write) |
| App stuck `Terminating` | A finalizer waiting on AWX. Check `status.hosts[]` for what it still has to delete, `status.jobID` with `status.cancelRequestedAt` for a job it is waiting to see stop, and whether the `AWXConnection` still resolves. The controller log names the run and what it is retrying |
