# Scenarios

What actually happens, step by step, when something changes. The [README](README.md) covers what the CRDs are; this page covers what the controller does with them - on a create, on an update, and on a delete.

- [The cast](#the-cast)
- [Creates](#creates)
- [Updates](#updates)
- [Deletes](#deletes)
- [How long each of these takes](#how-long-each-of-these-takes)

## The cast

| Object | Who writes it | What it owns |
|---|---|---|
| `AWXConnection` | you | where AWX lives, and the `Secret` holding its API token |
| `AnsibleBinding` | you | a `vmSelector` and the template to launch |
| `AnsibleBindingVM` | the controller | one matched VM's AWX inventory host and its run |

Two facts explain most of the behavior below.

**The binding never talks to AWX.** Everything that upserts a host or launches a job happens on an `AnsibleBindingVM`. That keeps the binding's own pass O(1) in AWX requests no matter how many VMs the selector matches, and lets several VMs reconcile at once instead of queueing behind one another. The one exception is the rare [orphan sweep](#a-host-leaks-because-the-controller-was-killed-mid-cleanup).

**Each `AnsibleBindingVM` is owned by its `VirtualMachine`, not by the binding.** A deleted VM therefore takes its child with it through ordinary garbage collection, with no help from the binding. The binding deletes a child only when the VM merely stops matching the selector. Both routes end at the child's own finalizer, which is where the AWX host is cleaned up.

The children carry the per-VM detail - phase, job URL, run history - and the binding carries a fixed-size rollup of them. `kubectl get ansiblebindingvm` is where you look when one VM out of a hundred is unhappy.

## Creates

### A binding is created and VMs already match

You apply a binding selecting `app=web`, and three VMs carry that label.

1. The binding's pass lists the matching VMs and lists its own children out of the informer cache (no API server read), finds no children, and creates three `AnsibleBindingVM`s. Each one gets a **copy** of the binding's spec, plus the binding's `generation` and `reconcile-requested-at` value. A copy, not a reference, because a child has to be able to finalize after its parent is already gone.
2. Each child reconciles on its own worker. It reads its `VirtualMachine` from the API server and checks that it is powered on and reporting an address.
3. The child resolves the template **from AWX, every time** - never from the template cache. `ask_limit_on_launch` is what stops a run going against an entire inventory, and it can be switched off in the AWX UI between one pass and the next. If it is off, the child refuses to launch and says so in `status.message` rather than letting AWX silently widen the run.
4. The child writes the host name and inventory ID into its status **before** creating anything in AWX, so a crash between the two leaves a record its finalizer can act on. Then it upserts the host with `ansible_host` set to the VM's reported IP and an ownership marker in the host's description.
5. It launches the template with `--limit <host name>`, and records the job ID, job URL, and the generation and trigger it just satisfied.
6. The child's status change wakes the binding, which is watching its children. The binding recomputes the whole rollup from scratch: `summary: {total: 3, running: 3}`.
7. As jobs finish, each child polls its own job to a terminal status. When all three succeed the binding reads `Ready - All 3 VM(s) completed the requested run successfully.`

### A VM starts matching a binding that already exists

Either a new VM is provisioned with the label, or an existing VM is relabelled into the selector.

There is deliberately no `VirtualMachine` informer - caching every VM on the Supervisor would cost memory proportional to the whole cluster - so the binding notices on its next resync, within `resync_period` (60s by default). It then creates one child, which does the work above.

The VMs that were already matched are untouched. Their `status.appliedGeneration` and `status.appliedTrigger` still equal what their spec asks for, so nothing relaunches. Adding a VM to a tier does not re-run the playbook on the rest of it.

### A matched VM is powered off, or has no address yet

The child exists and sits at `Pending`. No inventory host is created and no job is launched - there is no address to write into `ansible_host` and nothing to run against. The binding reports `Pending - 1 of 4 VM(s) have not completed this request yet (not yet started, powered off, or no reported IP).`

When the VM powers on and reports an address, the child's next pass provisions the host and launches.

### A selector suddenly matches hundreds of VMs

The binding collects every write it wants before issuing any of them, then issues them in parallel batches that double in size (1, 2, 4, 8 ... capped at 32 concurrent), up to 500 writes per pass. Whatever is left is carried to the next pass, which comes straight back rather than waiting for the resync.

Starting from a batch of one is what makes a systematic failure cheap: a webhook rejecting every child, or exhausted object quota, costs a handful of requests rather than the whole burst. A batch that fails only in part carries on, so one permanently broken VM cannot starve every VM ordered behind it.

Deletes are issued before creates, so exhausted quota cannot deadlock: the deletes that free the quota are never queued behind the creates that need it.

## Updates

### You edit the binding's spec

Say you change `extraVars`, or point it at a different template.

1. `metadata.generation` bumps.
2. The binding rewrites every child's spec with a JSON Patch that **replaces the whole `spec`**, guarded on the child's UID and resourceVersion. Replacing rather than merging is the point: clearing `spec.hostName` has to clear it on the child, and `useDefaultLimit` going from true to false has to actually narrow the run.
3. Each child sees `status.appliedGeneration != spec.bindingGeneration` and launches a fresh run.
4. Until a child has applied the new generation, the binding counts it as **Pending** - whatever its last run says. Otherwise the binding would read `Ready`, with `observedGeneration` already bumped, in the window between your edit and the first child acting on it, and anything waiting on the binding would take that as "the new playbook has run".

### You want to re-run the same playbook

Bump the `ansible.field.vmware.com/reconcile-requested-at` annotation to any new value. The binding copies it down as `spec.bindingTrigger`, each child compares it against its own `status.appliedTrigger`, and every matched VM gets a fresh run.

### A re-run is requested while a job is still in flight

The child polls the running job, sees it is not terminal, and leaves `appliedGeneration` and `appliedTrigger` alone. When the job finishes, the next pass sees the request still unapplied and launches. The request is queued, not swallowed - which is why the comparison is per VM rather than on the binding.

### A VM powers off mid-run

A job's outcome does not depend on the VM's current power state, so the child polls it either way. The result lands in `status.history` like any other. See [the FAQ](FAQ.md#what-happens-to-in-flight-runs-when-a-vm-powers-off).

### Someone deletes or hand-edits the host in the AWX UI

Every `host_check_period` (600s by default) each child reconciles its inventory host against AWX itself, rather than trusting what its own status says it pushed last time. A deleted host is recreated; an edited `ansible_host` is repaired. Variables the controller does not manage are left untouched.

Without this, a host deleted in the UI would go undetected and every later run would fail with `--limit does not match any hosts`, with nothing to repair it.

Between checks an idle VM costs AWX nothing at all. A spec change or a re-run request is not on that timer - both take effect on the next pass.

### You repoint an `AWXConnection` at a different AWX instance

Every host ID and inventory ID in a child's status was issued by one instance and means nothing on another - the same number there belongs to some unrelated host, which cleanup would then delete.

The child fingerprints the endpoint into `status.awxEndpoint`. When that stops matching, it forgets the recorded IDs rather than acting on them, and looks the host up by name on the new instance, adopting or creating it like any other. Cleanup does the same: a child finalizing against an instance that did not issue its host ID abandons the host rather than deleting something at random.

### You change `hostName` or `hostNamePrefix`, or repoint the template at another inventory

The host has to move, and the old entry must not be left behind under the old name or in the old inventory while a second one appears alongside it.

The child first re-finds the previous host by name, since an earlier upsert may have reached AWX before its ID reached status. If that host carries this binding's ownership marker and `cleanupPolicy` is `Delete`, it is deleted. Then the new host is created. If the delete fails, the child keeps the recorded host and retries rather than losing track of it.

### The host already existed in AWX

It is adopted, never hijacked: its variables are merged rather than overwritten, `status.awxHostCreated` records that the controller did not create it, and cleanup never deletes it. If its existing variables are not a JSON object that can be safely merged into, the child refuses rather than destroying them.

A host carrying **another supervisor's** ownership marker is refused outright - see [Can several supervisors share one AWX instance?](FAQ.md#can-several-supervisors-share-one-awx-instance)

### Prompt on Launch is switched off in AWX between passes

The launch path re-reads the template every time precisely for this. `ask_limit_on_launch` off with a per-VM limit in play, or `ask_variables_on_launch` off with `extraVars` set, means AWX would silently drop the field - so the child refuses and tells you which setting to enable. The first would run your playbook against the whole inventory.

## Deletes

### A VM is deleted

1. The garbage collector deletes the child, because the child's `ownerReference` points at that `VirtualMachine`.
2. The child goes to `Terminating` and its finalizer runs.
3. Cleanup re-resolves the host **by name in the inventory** and re-checks the ownership marker before deleting anything. A saved ID alone is not trusted: a host deleted out of band may have been recreated just before a crash.
4. A host with no marker (adopted) is left alone, as is everything under `cleanupPolicy: Retain`.
5. If AWX is unreachable, the error is returned and the finalizer holds - the delete is retried rather than leaking the host. Only a genuinely unrecoverable case - the `AWXConnection` or its `Secret` is gone or malformed - is logged and abandoned, since blocking the delete forever would not bring the host back either.
6. The binding counts the child under `summary.terminating`, apart from the phase buckets. A child wedged on an AWX host that will not delete stays visible instead of vanishing from the rollup while the binding above it reads `Ready`.

### A VM is deleted and recreated under the same name

Owner references resolve by UID, so the old child is collected rather than handed to the new VM. The child checks this too: before acting, it compares the live VM's UID against the UID in its own owner reference, and stands down if they differ. Otherwise the old VM's inventory host - and its playbook run - would be pointed at the new VM in the window before the garbage collector catches up.

### A VM is relabelled out of the selector

The binding notices on its next pass, and deletes the child (with a UID precondition, so stale cache data cannot delete a replacement). From there it is the same finalizer path as a deleted VM, and the AWX host goes.

This matters more than it looks: a stale inventory host keeps an `ansible_host` IP that can be reassigned to an unrelated VM later.

### The binding is deleted

1. The binding's finalizer runs `cleanupAnsibleBinding`.
2. The children are owned by their VMs, not by the binding, so the garbage collector will not remove them. The binding lists them **live from the API server** - not from the cache - and deletes each one.
3. Before deleting a child, the binding copies the current `cleanupPolicy` down into it. Finalization no longer runs the normal reconcile that would otherwise copy the spec down, so without this, setting `cleanupPolicy: Retain` on a binding already stuck on an unreachable AWX would change nothing - the one thing the docs promise it does.
4. While any child remains, the binding returns an error and stays in `Terminating`. That is what makes every child's own finalizer run to completion before the binding disappears.

If the service itself is uninstalled while bindings still exist, nothing is left to drain them and they hang in `Terminating` - see the [uninstall notes](README.md#uninstalling) and [how to find leftover hosts](FAQ.md#how-do-i-find-awx-hosts-a-supervisor-left-behind).

### `cleanupPolicy: Retain`

No AWX host is ever deleted: not when a VM goes, not when it is relabelled out, not when the binding is deleted, not on a rename, and the orphan sweep is disabled too. For when you manage AWX inventory by hand.

### A host leaks because the controller was killed mid-cleanup

Every four host-check periods (2400s by default), a binding lists the AWX hosts carrying **its own** ownership marker, in the inventories its children actually use, and deletes any that no child and no matched VM accounts for. An adopted host carries no marker and can never be returned here.

Before deleting anything, the candidate list is re-checked against a **fresh list of children read from the API server**. Reaping is rare and destructive, which is exactly when a quorum read is worth paying for: without it, a child created moments ago but not yet in the cache would have the host it is about to use deleted out from under a running playbook.

The sweep is skipped entirely on any pass that still has child writes outstanding, or that hit an error - the children that would claim those hosts do not exist yet.

## How long each of these takes

| Event | Latency |
|---|---|
| Binding created or edited, re-run annotation bumped | immediate (watch) |
| A child's status changes, binding rollup updates | immediate (the binding watches its children) |
| A VM starts matching, or is relabelled out | up to `resync_period` (60s) |
| A VM is deleted, its child is deleted | immediate (garbage collector) |
| A host deleted or hand-edited in AWX is repaired | up to `host_check_period` (600s) |
| A leaked host is reaped | up to four host-check periods (2400s) |

In a steady state that is one AWX request per VM per host-check period, plus one per binding per sweep. Everything else in an idle pass is served from the informer caches this process already maintains.
