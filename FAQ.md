# FAQ

- [I used the AAP integration in classic Aria Automation. What maps to what?](#i-used-the-aap-integration-in-classic-aria-automation-what-maps-to-what)
- [Which AWX/Tower/AAP versions are supported?](#which-awxtoweraap-versions-are-supported)
- [Why does my template need Prompt on Launch for Limit?](#why-does-my-template-need-prompt-on-launch-for-limit)
- [Can several supervisors share one AWX instance?](#can-several-supervisors-share-one-awx-instance)
- [How do I find AWX hosts a supervisor left behind?](#how-do-i-find-awx-hosts-a-supervisor-left-behind)
- [What's different about Workflow Templates?](#whats-different-about-workflow-templates)
- [What happens to in-flight runs when a VM powers off?](#what-happens-to-in-flight-runs-when-a-vm-powers-off)
- [Why is there no pre-delete hook?](#why-is-there-no-pre-delete-hook)
- [Why does varsFrom refuse to read a Secret?](#why-does-varsfrom-refuse-to-read-a-secret)
- [Why do AnsibleBinding and AnsibleRun handle a lost launch differently?](#why-do-ansiblebinding-and-ansiblerun-handle-a-lost-launch-differently)

## I used the AAP integration in classic Aria Automation. What maps to what?

Classic Aria Automation (vRA) had a built-in Ansible Automation Platform integration: you registered an AAP endpoint org-wide, dropped a `Cloud.Ansible.Tower` resource into a cloud template, and the machine being provisioned was handed to it. It only ever applies to VMs vRA provisions that way, so it cannot touch Supervisor-native VMs - and in a **VCF Automation 9.x All Apps** organization the `Cloud.Ansible.Tower` resource type is not available at all.

This service puts the same capability on the Supervisor, as CRDs. Nothing here replaces or interferes with the classic integration: if you are on a VM Apps organization or classic vRA, that integration still exists and still works, and this is for the VMs it cannot reach.

| | Classic Aria Automation / VM Apps | This service |
|---|---|---|
| Where the config lives | `Cloud.Ansible.Tower` resource in a cloud template | `AnsibleBinding` CR in the VM's namespace |
| Where credentials live | AAP integration, registered org-wide in vRA | `AWXConnection` + `Secret`, per namespace, owned by the tenant |
| How a host is targeted | vRA hands the provisioned machine to the integration | Label selector (`vmSelector`); the controller creates the inventory host and scopes the run with `--limit` |
| What can be targeted | Exactly the one machine being provisioned | Every VM the selector matches, now and later |
| When it runs | Once, at provisioning time | At provisioning time and on demand afterwards - bump the `reconcile-requested-at` annotation or edit the spec |
| Drift in AWX | Not tracked | The inventory host is reconciled against AWX every pass; one deleted or hand-edited in the UI is repaired |
| Teardown | Deprovisioning removes the host | The binding's finalizer removes the AWX hosts it created (`cleanupPolicy: Retain` opts out) |
| AAP 2.5+ | [Broken](#which-awxtoweraap-versions-are-supported) - KB 394498 says stay on 2.4 | Detected and supported |

Two differences are worth internalizing rather than skimming:

**A selector is not a machine.** `Cloud.Ansible.Tower` could only ever touch the machine it was attached to. `vmSelector` reaches every matching VM in the namespace, including VMs from other deployments - which is what makes "configure this whole tier" a single CR, and what makes a careless selector dangerous. Label per deployment and match on that label.

**Configuration is a persistent binding, not a provisioning step.** The CR stays around after the run, so re-running a playbook is a day-2 update to an object rather than a redeploy, and each VM keeps its own phase, job URL and bounded run history in `status.vms`.

Driving all of this from a VCF Automation 9.x blueprint - where the binding becomes a `CCI.Supervisor.Resource` and deployment success waits on the playbook - is covered in [VCFA blueprints](VCFA-BLUEPRINTS.md).

## Which AWX/Tower/AAP versions are supported?

All of them: AWX, Ansible Tower, AAP 2.4 and older, and AAP 2.5+.

AWX, Ansible Tower and AAP up to 2.4 all serve the controller API at `/api/v2`. **AAP 2.5 introduced the platform gateway and moved it to `/api/controller/v2`** - the old path 404s rather than redirecting. Aria Automation's own Ansible integration breaks on exactly this: Broadcom [KB 394498](https://knowledge.broadcom.com/external/article/394498/ansible-automation-platformansible-tower.html) reports `"Failed to validate credentials."` plus a 404 after upgrading to AAP 2.5+, and the documented resolution is to stay on AAP 2.4 or older.

This controller detects which flavor it's talking to instead. On first validation of an `AWXConnection` it probes each candidate's unauthenticated `ping/` endpoint and caches the winner in `status.apiBasePath`:

```bash
kubectl get awxconnection -n my-namespace
# NAME         READY   STATE   API                  AGE
# sample-awx   true    Ready   /api/v2              4m
# aap-25       true    Ready   /api/controller/v2   2m
```

Probing `ping/` (which needs no credentials) keeps "wrong API path" distinguishable from "bad token", which is the confusion behind that KB's error message. If your instance serves the API somewhere else entirely, set it explicitly and detection is skipped:

```yaml
spec:
  apiBasePath: "/api/controller/v2"
```

## Why does my template need Prompt on Launch for Limit?

Because otherwise AWX runs your playbook against every host in the inventory.

If a template doesn't accept a limit at launch time (`ask_limit_on_launch: false`), AWX silently discards the limit the controller sends and runs against that template's entire inventory rather than just your VM. Nothing in AWX flags that it happened.

The controller checks this up front and refuses to launch rather than widen the blast radius, so an `AnsibleBinding` pointed at such a template goes `Failed` with an explanatory message and starts nothing. Either enable Prompt on Launch for Limit in AWX, or set `useDefaultLimit: true` on the binding to say you deliberately want the template's own scope.

Enable Prompt on Launch for Variables too if you use `extraVars`, for the same reason.

## Can several supervisors share one AWX instance?

Yes. Ownership is tracked so they don't fight over inventory hosts.

AWX host names are unique per inventory, and AWX Hosts have no labels or tags - so when several supervisors (or several tenant namespaces) point at one AWX instance and the same job template, two VMs called `web-1` want the same inventory host entry.

The controller records ownership in the AWX host's **description** field: `ansible-supervisor:<supervisor_id>:<namespace>/<name>`. Description is the only free-text field on an AWX Host, and unlike host variables it never leaks into playbooks. Because that marker lives in AWX rather than only in CR status, it survives a binding being deleted and recreated. On a name collision the controller then:

| Existing host | Behavior |
|---|---|
| Marked as **this** binding's | Updated and owned - including a host left behind by an earlier incarnation of the same binding (so `cleanupPolicy: Retain` → delete → recreate reclaims it rather than orphaning it forever) |
| Marked by **another** supervisor or binding | **Refused.** Nothing is written, no job is launched, and the `AnsibleBinding` goes `Failed` naming the other owner |
| **Unmarked** (created by hand in AWX) | Adopted: variables merged, description left alone, never deleted |

Set `supervisor_id` at install time to something readable (e.g. `sup-lab-01`); left empty it's derived from the `kube-system` namespace UID, which works but makes the inventory hard to read.

To resolve a refused collision, give the binding its own namespace in the inventory with `hostNamePrefix` on the `AWXConnection`:

```yaml
spec:
  url: "https://awx.example.com"
  secretRef: "awx-token"
  hostNamePrefix: "sup-lab-01-"    # -> inventory host "sup-lab-01-web-1"
```

Host names are only inventory labels - the real address rides in `ansible_host` - so a prefix costs nothing but what `inventory_hostname` looks like inside playbooks. Changing the prefix later retires the old host entry rather than orphaning it.

## How do I find AWX hosts a supervisor left behind?

Hosts can outlive their binding if you used `cleanupPolicy: Retain`, or if finalizers were stripped by hand during an uninstall. The ownership marker makes them findable:

```
GET /api/v2/hosts/?inventory=<id>&description__startswith=ansible-supervisor:<supervisor_id>
```

## What's different about Workflow Templates?

`type: JobTemplate` targets `/api/v2/job_templates/`, `type: WorkflowTemplate` targets `/api/v2/workflow_job_templates/` - AWX's two distinct launchable objects.

Workflow templates commonly have no inventory of their own, since each node can carry one. When that's the case there's no inventory for the controller to create a host in and nothing to scope a `--limit` against, so the binding effectively behaves like `useDefaultLimit: true` for that template regardless of what the spec says.

## What happens to in-flight runs when a VM powers off?

The run is tracked independently of the VM, so nothing is lost. A job already running is still polled to completion, and the VM keeps its last run's phase rather than reverting to `Pending`.

Re-run requests made during downtime aren't swallowed either. Whether a run is needed is decided per VM (`status.vms[].appliedGeneration` / `appliedTrigger`), so a spec change or annotation bump made while one VM's job is still running - or while a VM is powered off - is honored as soon as that VM can act on it.

## Why is there no pre-delete hook?

Because the mechanism exists and this service is not allowed to use it.

VM Service has a real one: annotate a VM with `delete.check.vmoperator.vmware.com/<component>: <reason>` and vm-operator will not destroy it until the annotation is removed. It is stronger than a finalizer, too - vm-operator holds off deleting the *vSphere* VM, not just the Kubernetes object, which is exactly what a decommission playbook needs. There is a sibling `poweron.check.vmoperator.vmware.com/<component>` for power-on.

Both are gated on vm-operator's `IsPrivilegedAccount`: the vm-operator service account, `system:masters`, kube-admin, or an entry in its `PRIVILEGED_USERS` list. That list is an environment variable baked into the vm-operator manager Deployment by VCF. It does support supervisor services - a stock VCF 9.x supervisor has an entry like `system:serviceaccount:svc-configuration-HASH:configuration-service-controller-manager`, whose wildcard matches the `svc-<name>-<5 characters>` namespace shape every supervisor service gets. But a Carvel package cannot add itself to it, so this service's account is not privileged and its annotation would be rejected.

A plain finalizer on the `VirtualMachine` is not a workaround. vm-operator's own finalizer destroys the vSphere VM during its finalization, so ours would only keep a dead API object around and the playbook would SSH into nothing.

Watching for VM deletions and reacting after the fact was considered and rejected on accuracy. "Left the selector" conflates a VM being deleted, a VM being relabelled, and the binding's own selector being edited; even narrowing to "the object is gone" cannot separate a decommission from a delete-and-recreate. Intent lives in whatever asked for the deletion, and it is not recoverable from watching state change.

**What to do instead** is what vRA itself does: let the orchestrator sequence it. Run the playbook, *then* destroy. In a VCF Automation blueprint that is a day-2 action creating an `AnsibleRun` with a `vmRef` while the VM is still up, waiting on `.status.state`, and deleting the VM after - see [VCFA blueprints](VCFA-BLUEPRINTS.md#decommissioning-in-the-right-order). For cleanup that doesn't need the guest at all (DNS, CMDB, monitoring), an `AnsibleRun` with `varsFrom` works after the VM is gone too, as long as whatever creates it still knows the name and address.

## Why does varsFrom refuse to read a Secret?

Because `extra_vars` are not a private channel. AWX echoes them in job output and keeps them in the job's stored launch parameters, so anything read this way is visible to everyone who can see that job - long after the run. Sourcing a password through it would be a credential leak with extra steps, so the refusal is unconditional: it applies even when the core API group is in `vars_from_api_groups` and the controller could technically read the Secret.

The mechanism for credentials is an AWX Credential attached to the template, exactly as the Machine credential that logs into VMs already is. A custom credential type injecting environment variables covers the API-token case:

```yaml
# input configuration
fields:
  - id: infoblox_host,     type: string, label: Grid Master
  - id: infoblox_username, type: string, label: Username
  - id: infoblox_password, type: string, label: Password, secret: true
# injector configuration
env:
  INFOBLOX_HOST:     "{{ infoblox_host }}"
  INFOBLOX_USERNAME: "{{ infoblox_username }}"
  INFOBLOX_PASSWORD: "{{ infoblox_password }}"
```

The playbook then needs no `provider:` block at all, and nothing sensitive passes through this service.

## Why do AnsibleBinding and AnsibleRun handle a lost launch differently?

There is a window in both: the controller sends a launch to AWX and dies before recording the job ID. On the next pass it cannot tell whether the job started.

An `AnsibleBinding` relaunches. Its playbooks are convergent configuration - running one twice is how the resource works in the first place, and leaving a VM unconfigured is the worse outcome.

An `AnsibleRun` refuses to. It exists for things that are *not* convergent: opening a ticket, decommissioning a host, sending a notification. Doing one of those twice can be worse than not doing it, and unlike a binding there is no later reconcile that would put things right. So the run records `status.launchAttemptedAt` before sending the launch, and finding that set with no `jobID` fails the run with a message pointing at AWX's recent jobs for that template. If it did not run, create another `AnsibleRun`.

This is also why `AnsibleRun` never launches twice for any other reason: its spec is immutable, and the re-run annotation an `AnsibleBinding` responds to is ignored.
