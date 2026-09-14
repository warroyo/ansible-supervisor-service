# Scenarios

This page walks through what actually happens, step by step, when something changes. The [README](README.md) covers what the CRDs are and [Architecture](ARCHITECTURE.md) covers how they fit together, this one covers what the controller does with them on a create, on an update, and on a delete.

- [The objects involved](#the-objects-involved)
- [Creates](#creates)
- [Updates](#updates)
- [Deletes](#deletes)
- [One-off runs](#one-off-runs)
- [How long each of these takes](#how-long-each-of-these-takes)

Everything from [Creates](#creates) to [Deletes](#deletes) is about an `AnsibleBinding`, which is standing state that fans out and re-runs. [One-off runs](#one-off-runs) covers the other model, `AnsibleRun`, where the whole life of the object is one AWX job.

## The objects involved

| Object | Who writes it | What it owns |
|---|---|---|
| `AWXConnection` | you | where AWX lives, and the `Secret` holding its API token |
| `AnsibleBinding` | you | a `vmSelector` and the template to launch |
| `AnsibleBindingVM` | the controller | one matched VM's AWX inventory host and its run |
| `AnsibleRun` | you | one execution: its own AWX job and whatever inventory hosts it made for it |

There are two things worth understanding up front, they explain most of the behavior below.

**The binding never talks to AWX.** Everything that upserts a host or launches a job happens on an `AnsibleBindingVM`. I did it this way so the binding's own pass stays O(1) in AWX requests no matter how many VMs the selector matches, and so several VMs can reconcile at once instead of queueing up behind one another. The one exception is the rare [orphan sweep](#a-host-leaks-because-the-controller-was-killed-mid-cleanup).

**Each `AnsibleBindingVM` is owned by its `VirtualMachine`, not by the binding.** A deleted VM takes its child with it through ordinary garbage collection, with no help from the binding. The binding only deletes a child when the VM stops matching the selector. Both routes end up at the child's own finalizer, which is where the AWX host gets cleaned up.

The children carry the per-VM detail like phase, job URL and run history, and the binding carries a fixed-size rollup of them. When one VM out of a hundred is unhappy, `kubectl get ansiblebindingvm` is where you want to look.

## Creates

### A binding is created and VMs already match

Say you apply a binding selecting `app=web`, and three VMs carry that label.

1. The binding's pass lists the matching VMs and lists its own children out of the informer cache (no API server read), finds no children, and creates three `AnsibleBindingVM`s. Each one gets a **copy** of the binding's spec, plus the binding's `generation` and `reconcile-requested-at` value. It's a copy rather than a reference because a child has to be able to finalize after its parent is already gone.
2. Each child reconciles on its own worker. It reads its `VirtualMachine` from the API server and checks that it is powered on and reporting an address.
3. The child resolves the template **from AWX, every time**, never from the template cache. `ask_limit_on_launch` is what stops a run going against an entire inventory, and it can be switched off in the AWX UI between one pass and the next. If it's off, the child refuses to launch and says so in `status.message` rather than letting AWX silently widen the run.
4. The child writes the host name and inventory ID into its status **before** creating anything in AWX, so if it crashes between the two there is still a record its finalizer can act on. Then it upserts the host with `ansible_host` set to the VM's reported IP and an ownership marker in the host's description.
5. It launches the template with `--limit <host name>`, and records the job ID, job URL, and the generation and trigger it just satisfied.
6. The child's status change wakes the binding, which is watching its children. The binding recomputes the whole rollup from scratch: `summary: {total: 3, running: 3}`.
7. As jobs finish, each child polls its own job to a terminal status. When all three succeed the binding reads `Ready - All 3 VM(s) completed the requested run successfully.`

### A VM starts matching a binding that already exists

Either a new VM is provisioned with the label, or an existing VM is relabelled into the selector.

There's deliberately no `VirtualMachine` informer, caching every VM on the Supervisor would cost memory proportional to the whole cluster, so the binding notices on its next resync, within `resync_period` (60s by default). It then creates one child, which does the work above.

The VMs that were already matched are left alone. Their `status.appliedGeneration` and `status.appliedTrigger` still equal what their spec asks for, so nothing relaunches. Adding a VM to a tier doesn't re-run the playbook on the rest of it.

### A matched VM is powered off, or has no address yet

The child exists and sits at `Pending`. No inventory host is created and no job is launched, there's no address to write into `ansible_host` and nothing to run against. The binding reports `Pending - 1 of 4 VM(s) have not completed this request yet (not yet started, powered off, or no reported IP).`

When the VM powers on and reports an address, the child's next pass provisions the host and launches.

### A selector suddenly matches hundreds of VMs

The binding collects every write it wants before issuing any of them, then issues them in parallel batches that double in size (1, 2, 4, 8 ... capped at 32 concurrent), up to 500 writes per pass. Whatever is left is carried to the next pass, which comes straight back rather than waiting for the resync.

Starting from a batch of one is what makes a systematic failure cheap. A webhook rejecting every child, or exhausted object quota, costs a handful of requests rather than the whole burst. A batch that fails only in part carries on, so one permanently broken VM can't starve every VM ordered behind it.

Deletes get issued before creates so that exhausted quota can't deadlock, the deletes that free the quota are never queued behind the creates that need it.

## Updates

### You edit the binding's spec

Say you change `extraVars`, or point it at a different template.

1. `metadata.generation` bumps.
2. The binding rewrites every child's spec with a JSON Patch that **replaces the whole `spec`**, guarded on the child's UID and resourceVersion. Replacing rather than merging is the point here, clearing `spec.hostName` has to clear it on the child, and `useDefaultLimit` going from true to false has to actually narrow the run.
3. Each child sees `status.appliedGeneration != spec.bindingGeneration` and launches a fresh run.
4. Until a child has applied the new generation, the binding counts it as **Pending** no matter what its last run says. Otherwise the binding would read `Ready`, with `observedGeneration` already bumped, in the window between your edit and the first child acting on it, and anything waiting on the binding would take that as "the new playbook has run".

### You want to re-run the same playbook

Bump the `ansible.field.vmware.com/reconcile-requested-at` annotation to any new value. The binding copies it down as `spec.bindingTrigger`, each child compares it against its own `status.appliedTrigger`, and every matched VM gets a fresh run.

### A re-run is requested while a job is still in flight

The child polls the running job, sees it is not terminal, and leaves `appliedGeneration` and `appliedTrigger` alone. When the job finishes, the next pass sees the request still unapplied and launches. The request gets queued rather than swallowed, which is why the comparison is per VM rather than on the binding.

### A VM powers off mid-run

A job's outcome doesn't depend on the VM's current power state, so the child polls it either way. The result lands in `status.history` like any other. See [the FAQ](FAQ.md#what-happens-to-in-flight-runs-when-a-vm-powers-off).

### Someone deletes or hand-edits the host in the AWX UI

Every `host_check_period` (600s by default) each child reconciles its inventory host against AWX itself, rather than trusting what its own status says it pushed last time. A deleted host is recreated, an edited `ansible_host` is repaired. Variables the controller doesn't manage are left untouched.

Without this, a host deleted in the UI would go undetected and every later run would fail with `--limit does not match any hosts`, with nothing around to repair it.

Between checks an idle VM costs AWX nothing at all. A spec change or a re-run request isn't on that timer, both take effect on the next pass.

### You repoint an `AWXConnection` at a different AWX instance

Every host ID and inventory ID in a child's status was issued by one instance and means nothing on another. The same number on the new instance belongs to some unrelated host, which cleanup would then go ahead and delete.

The child fingerprints the endpoint into `status.awxEndpoint`. When that stops matching it forgets the recorded IDs rather than acting on them, and looks the host up by name on the new instance, adopting or creating it like any other. Cleanup does the same, a child finalizing against an instance that didn't issue its host ID abandons the host rather than deleting something at random.

### You change `hostName` or `hostNamePrefix`, or repoint the template at another inventory

The host has to move, and the old entry must not be left behind under the old name or in the old inventory while a second one appears alongside it.

The child first re-finds the previous host by name, since an earlier upsert may have reached AWX before its ID reached status. If that host carries this binding's ownership marker and `cleanupPolicy` is `Delete`, it gets deleted. Then the new host is created. If the delete fails, the child keeps the recorded host and retries rather than losing track of it.

### The host already existed in AWX

It gets adopted rather than hijacked. Its variables are merged rather than overwritten, `status.awxHostCreated` records that the controller didn't create it, and cleanup never deletes it. If its existing variables aren't a JSON object that can be safely merged into, the child refuses rather than destroying them.

A host carrying **another supervisor's** ownership marker is refused outright, see [Can several supervisors share one AWX instance?](FAQ.md#can-several-supervisors-share-one-awx-instance)

### Prompt on Launch is switched off in AWX between passes

The launch path re-reads the template every time precisely for this. `ask_limit_on_launch` off with a per-VM limit in play, or `ask_variables_on_launch` off with `extraVars` set, means AWX would silently drop the field, so the child refuses and tells you which setting to enable. In the first case your playbook would otherwise run against the whole inventory.

### Two bindings select the same VM

1. Both compute the same child name for it, children are named after the VM alone, so both try to create the same object.
2. Kubernetes accepts one. The other's create comes back `AlreadyExists`, and it reads the object live to find out whose it is rather than assuming it is its own from an earlier pass.
3. The loser records the VM under `summary.conflicted` with the owner's name, reports `Conflict`, and is not `Ready`. It creates nothing, launches nothing, and touches neither the child nor the AWX host.
4. Its other VMs reconcile normally, one contested VM doesn't stall the rest.
5. It looks again on a jittered ~30s interval. A released claim wakes its former owner, not the bindings queued behind it, so the waiters come back on their own rather than the namespace waking every binding on every child update.
6. When the owner releases the VM, either by not selecting it any more or by being deleted, the claim is only free once that child's finalizer has finished, `onDeleted` hook included. Under `cleanupPolicy: Retain` the AWX host still carries the old binding's ownership marker, so the new owner claims the VM but refuses that host until it is retired or the new binding gets its own `hostNamePrefix`.

## Deletes

### A VM is deleted

1. The garbage collector deletes the child, because the child's `ownerReference` points at that `VirtualMachine`.
2. The child goes to `Terminating` and its finalizer runs.
3. Cleanup re-resolves the host **by name in the inventory** and re-checks the ownership marker before deleting anything. A saved ID on its own isn't trusted, a host deleted out of band may have been recreated just before a crash.
4. A host with no marker (adopted) is left alone, as is everything under `cleanupPolicy: Retain`.
5. If AWX is unreachable, the error is returned and the finalizer holds, so the delete is retried rather than leaking the host. Only a genuinely unrecoverable case, where the `AWXConnection` or its `Secret` is gone or malformed, is logged and abandoned, since blocking the delete forever wouldn't bring the host back either.
6. The binding counts the child under `summary.terminating`, apart from the phase buckets. A child wedged on an AWX host that won't delete stays visible instead of vanishing from the rollup while the binding above it reads `Ready`.

### A VM is deleted and the binding has an `onDeleted` hook

It's the same finalizer, with a playbook in front of the host deletion.

1. The child re-reads itself **live from the API server** rather than from the informer cache. Everything below resumes from what the previous pass recorded, and a stale copy saying "nothing launched yet" would launch a second decommission run.
2. It confirms the VM is genuinely gone: absent, carrying a `deletionTimestamp`, or resolving to a different UID than the one in its owner reference. A VM that is merely unmatched gets no playbook, see [A VM is relabelled out of the selector](#a-vm-is-relabelled-out-of-the-selector).
3. A deadline is stamped into `status.deprovision.deadline` once, from `timeoutSeconds`. It is read back on later passes rather than recomputed, so editing the timeout mid-teardown can't extend a hook that is already running.
4. If a provisioning job is still in flight against this host, the hook waits for it. Running two playbooks against the same target with opposite intent is the one ordering that really has to be right.
5. The targeting the hook started under is stamped alongside the deadline and read back afterwards, so editing `spec.onDeleted.targeting` mid-teardown can't re-aim a hook that is already running. A record with no mode, meaning one written before this existed, is treated as `ManagedHost`.
6. The hook's template is resolved from AWX at launch time, never from the template cache. Under `ManagedHost` it is refused unless it accepts a limit **and is configured with the same inventory as the host**. A deprovision run against a whole inventory would decommission every host in it, and a limit naming a host in a different inventory selects nothing while the teardown reports success. There's no `useDefaultLimit` here, and neither refusal is quietly downgraded to `Template`. A host marked as another binding's is refused before any of this, it is not modified, not run against, not deleted, and no playbook is launched.
7. Under `targeting: Template` steps 6 and 8 don't apply. No inventory and no limit are sent, no host is required to exist or to be ours, and nothing is written to one. A transient failure to look the host up doesn't hold the hook up either, the lookup is owed to the cleanup that follows and is retried there. The workflow runs against whatever it is configured for.
8. The inventory host gets `ansible_connection: local` before the launch. The guest is already destroyed and its address may have been re-leased, so a play that forgets `delegate_to` must not reach whatever now answers there. If the host is one that survives the teardown, i.e. `cleanupPolicy: Retain` or an adopted host, what the variable said before is recorded first and put back once the hook is terminal, so the next provisioning run isn't silently redirected to the AWX control node.
9. `status.deprovision.phase` goes to `Launching` **before** the launch request, then to `Running` with the job id. A pass that finds `Launching` with no job id does not relaunch. The job may well be running, and running a decommission playbook twice is worse than not knowing whether it ran once.
10. Each later pass polls the job and requeues. The finalizer holds, but nothing sleeps, the work is one AWX request per poll on a terminating object.
11. On any terminal outcome, whether `Succeeded`, `Failed` or `TimedOut`, the outcome is recorded, the host is deleted and the finalizer is released. The record is written before any retry of the host deletion, so a host that won't delete can't cost the hook's outcome or cause a relaunch. The `ansible_connection` override is only ever taken back off the host it was written on, checked by id rather than by name, so a host recreated under the same name during the hook is left alone. Failure never blocks, a broken teardown playbook would otherwise hold the VM, its binding, and any namespace being deleted above it.
12. The outcome is written to the log with the AWX job URL, and to an Event on the `AnsibleBinding`. The child is deleted a moment later and takes `status.deprovision` with it, so the Event is what an operator finds afterwards.

### A VM is deleted and recreated under the same name

Owner references resolve by UID, so the old child is collected rather than handed to the new VM. The child checks this too, before acting it compares the live VM's UID against the UID in its own owner reference, and stands down if they differ. Otherwise the old VM's inventory host, and its playbook run, would be pointed at the new VM in the window before the garbage collector catches up.

### A VM is relabelled out of the selector

The binding notices on its next pass, and deletes the child (with a UID precondition, so stale cache data can't delete a replacement). From there it's the same finalizer path as a deleted VM, and the AWX host goes.

This matters more than it looks, a stale inventory host keeps an `ansible_host` IP that can be reassigned to an unrelated VM later.

### The binding is deleted

1. The binding's finalizer runs `cleanupAnsibleBinding`.
2. The children are owned by their VMs rather than by the binding, so the garbage collector won't remove them. The binding lists them **live from the API server**, not from the cache, and deletes each one.
3. Before deleting a child, the binding copies the current `cleanupPolicy` down into it. Finalization no longer runs the normal reconcile that would otherwise copy the spec down, so without this, setting `cleanupPolicy: Retain` on a binding already stuck on an unreachable AWX would change nothing, which is the one thing the docs promise it does.
4. While any child remains, the binding stays in `Terminating`. It reads the remaining children from the informer cache rather than listing them live on every pass, and is woken by each child that finishes, with the 30-second interval as the backstop for a missed event. Before it releases its own finalizer it confirms with a **live** list, since an empty cache isn't proof that nothing is left, and releasing on one would abandon a child, its AWX host and its teardown playbook. That's what makes every child's own finalizer run to completion before the binding disappears, including any `onDeleted` hook for the children whose VMs went first. Waiting is reported as waiting rather than as a failure, a teardown playbook taking minutes isn't an error to retry on a backoff.

If the service itself is uninstalled while bindings still exist, nothing is left to drain them and they hang in `Terminating`, see the [uninstall notes](README.md#uninstalling) and [how to find leftover hosts](FAQ.md#how-do-i-find-awx-hosts-a-supervisor-left-behind).

### `cleanupPolicy: Retain`

No AWX host is ever deleted, not when a VM goes, not when it's relabelled out, not when the binding is deleted, not on a rename, and the orphan sweep is disabled too. This is for when you manage AWX inventory by hand.

### A host leaks because the controller was killed mid-cleanup

Every four host-check periods (2400s by default), a binding lists the AWX hosts carrying **its own** ownership marker, in the inventories its children actually use, and deletes any that no child and no matched VM accounts for. An adopted host carries no marker and can never be returned here.

Before deleting anything, the candidate list is re-checked against a **fresh list of children read from the API server**. Reaping is rare and it's destructive, which is exactly the case where a quorum read is worth paying for. Without it, a child created moments ago but not yet in the cache would have the host it's about to use deleted out from under a running playbook.

The sweep is skipped entirely on any pass that still has child writes outstanding, or that hit an error, since the children that would claim those hosts don't exist yet.

## One-off runs

An `AnsibleRun` isn't standing state. It launches one AWX job, follows it to a terminal status, and is then over. The spec is immutable and the re-run annotation is ignored, so nothing restarts it. Everything below is about that "once".

### A run is created

The controller resolves the `AWXConnection` and the template, gathers `spec.extraVars` and whatever `spec.varsFrom` reads off live objects, resolves an inventory host for each target (`spec.vmRef`, or each entry in `spec.hosts`, or none at all), writes `status.launchAttemptedAt`, and launches, scoped with `--limit` to the hosts it targeted, or unscoped if it targets none. `state` goes `Pending` to `Running` to `Ready`, or `Failed`.

Resolving a target isn't the same as owning it. A host that is already in the inventory is used exactly as it stands, only one that isn't there gets created, and only a created host is ever written to or deleted. The attempt marker is written with the resourceVersion the pass read, so a stale copy of the run can't authorize a second launch.

### The host a run targets already exists

The run uses it rather than claiming it, whoever it belongs to keeps it. Its variables, address, groups and ownership marker are untouched, `status.hosts[].awxHostCreated` records that this run didn't create it, and cleanup leaves it alone. This is what lets a one-off run execute against a host an `AnsibleBindingVM` manages without the two contesting it.

Host-level fields for such a host, meaning `spec.hosts[].address`, `spec.hosts[].variables` and `spec.hostVariables`, are refused before anything launches, because the run can't apply them and running the playbook as though it had would be worse. Per-execution values go in `spec.extraVars`/`spec.varsFrom`, which AWX passes as the job's `extra_vars`, and Ansible ranks those above inventory variables, so they apply to that job and nothing else.

### A `varsFrom` object, or a `vmRef` VM, has not appeared yet

This gets retried rather than failed. An orchestrator may well create the run before the object it names has settled, and the same applies to a field that exists but is empty, a `VirtualMachine` whose guest hasn't reported an address yet being the usual one. The run stays `Pending` with the reason in `status.message`. `spec.activeDeadlineSeconds` is what bounds the wait, without one it waits indefinitely.

A `varsFrom` path that resolves to a list or an object is a different matter, that's a fact about an immutable spec, and it fails the run.

### AWX is unreachable, or answers 5xx

Retried as well. That includes the template lookup, a template that can't be resolved *right now* is an outage, and only one that is genuinely absent or ambiguous ends the run.

### The launch is sent and the answer never arrives

The run does not launch again, not on the strength of not knowing and not on anything else. It reads the template's recent jobs in AWX and matches on the `--limit` each ran with, among those AWX created at or after `launchAttemptedAt`:

- one match, so that job is this run's and it is adopted. Its id, URL and status are recorded and polling carries on from there.
- no match, after AWX has had a minute to show one, so the run ends `Failed`, saying when the launch was sent and what to check in AWX. Not finding a job isn't proof that none ran, a trimmed job list, a restricted view, or an AWX whose clock disagrees all read the same way, and a second decommission is worse than a run that has to be looked at.
- more than one match, so the run fails rather than guessing, naming the candidates.

AWX refusing the launch outright with a 4xx needs none of that. Nothing was created, so the run simply retries.

### The AWX job fails

This one is terminal. The controller did its job, AWX ran the playbook and the playbook failed, so `state: Failed` with a link to the job's output, and no retry. `finishedAt` is stamped either way, which is what a TTL counts from.

### `activeDeadlineSeconds` expires

The AWX job is **canceled**, then the run goes terminally `Failed`, the way a Kubernetes Job terminates its pods when its own deadline expires. If AWX can't be reached to cancel it, the run still finishes, that's what the deadline is for, and `status.failureReason` says the job may still be running.

### You repoint the `AWXConnection` while a run is in flight

The run ends with an explanation. Every id it recorded, the job and the hosts, was issued by the old instance and means something else entirely on the new one, and unlike a binding there's no next run to rediscover anything for. Its cleanup won't delete hosts there either. Create a new `AnsibleRun` for the new instance.

### You delete the run

Its finalizer cancels the AWX job if one is still running, **waits for AWX to confirm it stopped**, and only then deletes the inventory hosts the run created. Hosts that were already there are never deleted. `cleanupPolicy: Retain` keeps the created hosts too, but it doesn't leave the job running, that's not what the policy is about.

The wait matters because AWX answers a cancel with `202 Accepted`, meaning it has taken the request, not that it has stopped the playbook. Deleting the inventory entry a job is running against on the strength of that answer races the work being torn down. `status.cancelRequestedAt` records when the request was accepted, and bounds the wait, a job that still hasn't reached a terminal status well past that is reported and the run released, rather than wedging the object forever.

A cancel that *fails* holds both the finalizer and the hosts, nothing destructive happens in a pass that couldn't stop the job.

If the run's launch answer was never recorded, cleanup takes one last look for the job it may have started, under the same matching rules the reconcile path uses, and cancels it if it can be identified. If it can't, that's logged with the template to check. There's no later state in which it resolves itself, so the run is released rather than wedged.

### `ttlSecondsAfterFinished` elapses

The controller deletes the CR itself, which runs the same finalizer and takes its AWX hosts with it. Granularity is `resync_period`, which is plenty for a garbage collector.

### A run and a binding in one namespace share a name

They don't contest the same AWX host. The ownership marker a run writes carries its kind, so the two markers differ. A binding refuses to touch a host marked as a run's, and a run doesn't need to touch a binding's at all, it executes against it and writes nothing.

## How long each of these takes

| Event | Latency |
|---|---|
| Binding created or edited, re-run annotation bumped | immediate (watch) |
| A child's status changes, binding rollup updates | immediate (the binding watches its children) |
| A VM starts matching, or is relabelled out | up to `resync_period` (60s) |
| A VM is deleted, its child is deleted | immediate (garbage collector) |
| A host deleted or hand-edited in AWX is repaired | up to `host_check_period` (600s) |
| A leaked host is reaped | up to four host-check periods (2400s) |
| An `AnsibleRun` is created, or its job's status changes | immediate (watch), then polled each `resync_period` |
| A finished `AnsibleRun` is collected after its TTL | up to `resync_period` past the TTL |

In a steady state that's one AWX request per VM per host-check period, plus one per binding per sweep. Everything else in an idle pass is served from the informer caches this process already maintains.
