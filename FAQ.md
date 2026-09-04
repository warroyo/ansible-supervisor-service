# FAQ

- [I used the AAP integration in classic Aria Automation. What maps to what?](#i-used-the-aap-integration-in-classic-aria-automation-what-maps-to-what)
- [Which AWX/Tower/AAP versions are supported?](#which-awxtoweraap-versions-are-supported)
- [Why does my template need Prompt on Launch for Limit?](#why-does-my-template-need-prompt-on-launch-for-limit)
- [Can several supervisors share one AWX instance?](#can-several-supervisors-share-one-awx-instance)
- [How do I find AWX hosts a supervisor left behind?](#how-do-i-find-awx-hosts-a-supervisor-left-behind)
- [What's different about Workflow Templates?](#whats-different-about-workflow-templates)
- [What happens to in-flight runs when a VM powers off?](#what-happens-to-in-flight-runs-when-a-vm-powers-off)

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
| Drift in AWX | Not tracked | The inventory host is reconciled against AWX itself on a timer (`host_check_period`, 600s); one deleted or hand-edited in the UI is repaired |
| Teardown | Deprovisioning removes the host | The binding's finalizer removes the AWX hosts it created (`cleanupPolicy: Retain` opts out) |
| AAP 2.5+ | [Broken](#which-awxtoweraap-versions-are-supported) - KB 394498 says stay on 2.4 | Detected and supported |

Two differences are worth internalizing rather than skimming:

**A selector is not a machine.** `Cloud.Ansible.Tower` could only ever touch the machine it was attached to. `vmSelector` reaches every matching VM in the namespace, including VMs from other deployments - which is what makes "configure this whole tier" a single CR, and what makes a careless selector dangerous. Label per deployment and match on that label.

**Configuration is a persistent binding, not a provisioning step.** The CR stays around after the run, so re-running a playbook is a day-2 update to an object rather than a redeploy, and each VM keeps its own phase, job URL and bounded run history on its own `AnsibleBindingVM`.

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

## Can a playbook run twice for one request?

Rarely, yes: execution is at-least-once, not exactly-once.

The controller launches the job in AWX and then records the job id in the `AnsibleBindingVM`'s status. If the process is killed in the window between those two - AWX has accepted the launch, the status write has not landed - the next reconcile sees a VM with no run recorded and launches again. AWX's launch endpoint takes no idempotency key, so there is nothing to make the second launch collapse into the first.

The window is milliseconds wide and needs a crash inside it, but it is real, and it is the reason a provisioning playbook should be idempotent - which is the normal expectation of Ansible anyway. Nothing else in the controller re-launches on its own: a run already recorded is polled to completion, a re-run needs a generation bump or the annotation, and a VM that is powered off waits rather than retrying.

## I edited a host in the AWX UI. How long until the controller puts it back?

Up to `host_check_period` (600s by default). Each VM's inventory host is reconciled against AWX itself - not against what the CR's status claims it pushed - so a host deleted, renamed or hand-edited in the AWX UI is drift like any other and is repaired. What changed is the cadence: the check runs on its own period rather than on every resync, because with one object per VM a per-pass check made the AWX request rate scale with the number of VMs rather than the number of bindings.

Everything else in an idle pass is answered from the controller's own caches, so between checks a matched, up-to-date VM costs AWX nothing at all. Lower `host_check_period` if hand edits in AWX are common; raise it if AWX is the bottleneck. A spec change or a re-run annotation is not affected either way - both take the full path immediately.

The same period governs one other thing: each binding periodically lists the AWX hosts it owns and deletes any that no VM and no child accounts for - a host leaked by a controller killed mid-cleanup, say. Only hosts carrying this supervisor's ownership marker for that binding are ever considered, so adopted hosts and other supervisors' hosts are never touched, and `cleanupPolicy: Retain` disables it entirely.

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

Re-run requests made during downtime aren't swallowed either. Whether a run is needed is decided per VM (each `AnsibleBindingVM`'s `status.appliedGeneration` / `appliedTrigger` against the `bindingGeneration` / `bindingTrigger` its binding copied down), so a spec change or annotation bump made while one VM's job is still running - or while a VM is powered off - is honored as soon as that VM can act on it.
