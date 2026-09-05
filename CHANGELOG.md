# Changelog

What changed in each release, written for the people installing it. The
GitHub release for a tag is this file's section for that version, so a
tag whose version is missing here fails the release workflow before it
builds anything.

Versions follow [semver](https://semver.org). Unreleased work collects
under the top heading and is renamed to the version when the tag is cut.

## [Unreleased]

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

[Unreleased]: https://github.com/warroyo/ansible-supervisor-service/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/warroyo/ansible-supervisor-service/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/warroyo/ansible-supervisor-service/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/warroyo/ansible-supervisor-service/releases/tag/v1.0.0
