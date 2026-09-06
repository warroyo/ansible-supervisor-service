# Changelog

What changed in each release, written for the people installing it. The
GitHub release for a tag is this file's section for that version, so a
tag whose version is missing here fails the release workflow before it
builds anything.

Versions follow [semver](https://semver.org). Unreleased work collects
under the top heading and is renamed to the version when the tag is cut.

## [1.2.0] - 2026-09-06

### Added
- `spec.onDeleted` on an `AnsibleBinding`: a job or workflow template
  launched when a matched `VirtualMachine` is **deleted**, before that
  VM's AWX inventory host is removed - for deregistering it from DNS,
  IPAM, a CMDB or monitoring. The finalizer holds the object until the
  job reaches a terminal state, resuming across retries and controller
  restarts rather than relaunching, and the run is scoped to that host
  with `--limit`.
  - The guest is already destroyed by then, so the host is pinned with
    `ansible_connection: local` before launching and the playbook should
    act on the external record rather than the machine.
  - A hook that fails, cannot launch, or overruns
    `spec.onDeleted.timeoutSeconds` (default 900) is recorded and
    released, never blocked: a broken teardown playbook cannot hold a VM
    or its namespace in `Terminating`.
  - The outcome is logged with the AWX job URL and recorded as an Event
    on the `AnsibleBinding`, which outlives the child that ran it. The
    controller now needs `create` on `events`, added to its ClusterRole.
  - It fires only for a VM that is genuinely gone. One that merely
    stopped matching the selector is still running and gets no playbook,
    only the usual inventory-host cleanup. A host carrying another
    binding's ownership marker is not touched at all, and the hook's
    template must share `spec.template`'s inventory - a limit only
    selects within the job's own inventory, so a mismatch would target
    an unrelated host or none.
  - On a host that outlives the hook (`cleanupPolicy: Retain`, or one
    adopted rather than created here) the `ansible_connection: local`
    override is taken back off afterwards, restoring what it said
    before, so the next provisioning run still reaches the machine.
    Switching a binding to `Retain` while a hook runs is honored, and a
    restart mid-hook does not lose what the variable originally said.
  - `timeoutSeconds` is enforced before the hook's AWX work, so an
    expired hook under `Retain` finishes even when AWX is unreachable.
    Under `Delete` an unreachable AWX is still retried rather than
    leaking an inventory host.

- `spec.onDeleted.targeting`: `ManagedHost` (the default, and what an
  existing manifest gets) or `Template`. `ManagedHost` aims the hook at
  this VM's inventory host with a `--limit`, which is what it always
  did. `Template` supplies no inventory and no limit at all, leaving the
  aiming to the AWX template - for a decommission whose records live
  somewhere other than the machine: a workflow whose nodes each carry
  their own inventory, or a `hosts: localhost` playbook that calls an
  API. A `Template`-targeted hook does not need this VM's inventory host
  to exist, to be reachable, or to belong to this binding, and neither
  pins nor edits it. The mode a hook started under is recorded with its
  deadline, so editing the spec mid-teardown cannot re-aim a running
  hook. A `ManagedHost` template that stops accepting a limit still
  fails rather than being widened to whatever it does target.
- Exactly one `AnsibleBinding` may own a `VirtualMachine`. Selectors may
  overlap; ownership may not. `AnsibleBindingVM`s are now named after
  the VM alone, so the name is the claim and the create that wins is the
  arbitration. A binding matching a VM another binding owns reports
  `Conflict` with `status.summary.conflicted` / `conflictedVMs` naming
  the owner, is not `Ready`, and does nothing to that VM - no child, no
  job, no host changes - while its other VMs carry on. It retries on a
  jittered ~30s interval and takes over once the owner's child has
  finished cleaning up. To run several playbooks for one VM, compose
  them into one AWX workflow under a single binding.
  - Children now carry `spec.bindingUID`, and `vmName`, `bindingName`
    and `bindingUID` are immutable in the schema, which needs a
    Kubernetes 1.25 or newer API server. A binding deleted and recreated
    under the same name claims its VMs afresh rather than inheriting a
    live claim from the previous incarnation.
  - A `VirtualMachine` replaced under the same name waits for its
    predecessor's cleanup to finish before the replacement is claimed.
- [ARCHITECTURE.md](ARCHITECTURE.md): how the objects relate, how the
  claim is arbitrated, what the running process looks like and where
  each piece of state lives, with diagrams.

### Changed
- **Upgrading requires a drain.** Children created by earlier versions
  are named after the binding and the VM, and cannot coexist with the
  claim scheme above: two owners for one VM, and possibly a second
  provisioning job against a machine already being configured. The
  controller now checks for them at startup - before any worker runs and
  before it talks to AWX - and refuses to start, listing what it found.
  See [Upgrading](README.md#upgrading-to-the-next-release): stop GitOps,
  let the previous version finish in-flight jobs, delete the bindings so
  their children drain, then deploy this version and recreate them.
  Recreated bindings provision again.
- A binding waiting for its children to finish cleaning up now reports
  waiting rather than a reconcile failure, and looks again on a fixed
  interval instead of an escalating backoff.
- A hook whose job is already running now polls that job and nothing
  else - one live read and one AWX request per pass, where it used to
  re-look-up the inventory host and re-read the VirtualMachine as well.
  The poll is guarded by the AWX instance fingerprint recorded at launch,
  so a repointed `AWXConnection` cannot make it follow the same job
  number to an unrelated job elsewhere.
- Hook polls are jittered by up to three seconds, so a namespace deleted
  in one go does not leave every hook polling on the same beat, and a
  poll is never scheduled past the hook's own deadline.
- A terminating resource is no longer re-queued by its own status
  writes. The informer's deletion test is now an edge - the pass where a
  `deletionTimestamp` first appears - rather than a level, so a
  deprovision hook recording its progress no longer skips the poll
  interval its own cleanup asked for. Spec changes, re-run requests and
  the periodic resync still wake a terminating object, which is how a
  mid-teardown switch to `cleanupPolicy: Retain` still takes effect.
- A binding waiting on its children reads them from the informer cache
  instead of listing them live every few seconds, and confirms with a
  live list before releasing its finalizer - an empty cache is never
  taken as proof that nothing is left. The backstop interval is 30s
  rather than 5s, since each child that finishes wakes the parent.
- An `onDeleted` hook's deadline is now stamped and persisted before the
  AWXConnection is read and before the inventory host is looked up.
  Under `cleanupPolicy: Retain` a single transient host-lookup failure
  used to release the finalizer with the hook unrun and no record that
  it was ever owed; it is now retried until the hook's own deadline.
- Every AWX instance fingerprint a child recorded - the hook's included -
  now guards host deletion and host edits, not only job polling. A
  child whose host was rediscovered rather than remembered has no
  provisioning fingerprint, and a connection repointed mid-teardown
  could get as far as deleting a same-named host on the new instance.
- The hook's `ansible_connection` override is restored only onto the
  host it was written to, checked by id and instance rather than by
  name, so a host deleted out of band and recreated during the hook is
  not given a variable nothing ever set on it.
- A terminal hook outcome is written down before any retry of the host
  cleanup that follows it, so a host that will not delete cannot cause
  the hook to be relaunched or its outcome to be rewritten as skipped.
- `--log-level` (`info`, default, or `debug`). The per-pass reconcile
  lines are now debug-only: a thousand terminating VMs polling their
  hooks would otherwise write hundreds of lines a second saying nothing
  had changed. Launches, terminal outcomes and errors stay at `info`.

## [1.1.0] - 2026-09-05

### Added
- `AnsibleBindingVM`, one child per matched VM, carrying that VM's phase,
  observed IP, AWX host and inventory ids, last job and a bounded run
  history. `kubectl get ansiblebindingvm -l field.vmware.com/binding=<name>`
  is now how you look at a fan-out. Each child is owned by its
  `VirtualMachine`, so deleting a VM cleans up through ordinary garbage
  collection, and each child's finalizer removes the AWX host it created.
- A reaper that picks up the AWX host of a VM that stopped matching a
  selector while the controller was not running.

### Changed
- Per-VM detail moved off `AnsibleBinding.status.vms[]` and onto the
  children. The binding's status now summarizes; the detail is per VM.
- The controller reads VMs and children from the informer caches it
  already runs instead of issuing its own reads, and bounds the work left
  over, so a large binding costs the API server much less per pass.
- The launch path no longer looks up the job template it was about to
  launch. The launch response carries what the lookup was fetching, and
  the lookup's result was never trusted anyway - it cost an AWX request
  per run for nothing.

### Fixed
- A launch could be issued and not recorded, leaving a run in flight that
  nothing polled and no status ever showed.
- Six correctness gaps found in review, including status writes racing a
  concurrent update and a repointed `AWXConnection` being acted on with
  ids that belong to a different AWX instance.

### Upgrading from 1.0.x
No action needed, but **each matched VM re-runs its playbook once**: a
child starts with no record of what its VM last ran. Inventory hosts are
not affected - children adopt the hosts 1.0.x created rather than
creating second ones. Full detail, including how to defer the re-runs
with `cleanupPolicy: Retain`, in
[README § Upgrading from 1.0.x](https://github.com/warroyo/ansible-supervisor-service/blob/main/README.md#upgrading-from-10x).

## [1.0.1] - 2026-09-04

### Added
- A live pre-release gate: `make dev-release`, then
  `make install-supervisor-service`, then `make verify-supervisor`. It
  builds and pushes the real image, assembles the real Carvel package,
  installs it on a Supervisor through the vCenter REST API and asserts
  the digest under test is the one actually running. See
  [CONTRIBUTING.md § Pre-release validation](https://github.com/warroyo/ansible-supervisor-service/blob/main/CONTRIBUTING.md#pre-release-validation).
- The Supervisor service install is scripted rather than clicked, and the
  harness creates and destroys its own fixture VM instead of requiring
  one to exist.

### Changed
- An idle `AnsibleBinding` costs far less in steady state: inventory
  hosts are re-checked on a period rather than every pass, so passes in
  between make no AWX requests at all.

### Fixed
- Three harness bugs the first live run found.
- `creationTimestamp` no longer emitted into the generated `PackageBuild`.
- Documentation errors found in review.

## [1.0.0] - 2026-09-03

Initial release.

### Added
- `AWXConnection` and `AnsibleBinding`, both namespace-scoped, binding VM
  Service `VirtualMachine`s selected by label to an AWX/Tower job or
  workflow template, with day-2 re-runs via the
  `ansible.field.vmware.com/reconcile-requested-at` annotation.
- AWX inventory tracking: the controller creates and deletes the hosts it
  owns and never deletes a host it adopted.
- Private-CA trust for AWX endpoints, and AAP 2.5 support.
- Carvel package installed as a Supervisor service, with the controller
  image pinned by digest so an install needs no registry re-resolution.

[Unreleased]: https://github.com/warroyo/ansible-supervisor-service/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/warroyo/ansible-supervisor-service/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/warroyo/ansible-supervisor-service/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/warroyo/ansible-supervisor-service/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/warroyo/ansible-supervisor-service/releases/tag/v1.0.0
