# Backlog

Planned work; each item records its scope, decisions, and acceptance criteria.

- [AWX host identity, ownership, and descriptions](#awx-host-identity-ownership-and-descriptions)
- [Teardown retries and waiting for AWX recovery](#teardown-retries-and-waiting-for-awx-recovery)
- [AnsibleRun replacement through Argo CD](#ansiblerun-replacement-through-argo-cd)

## AWX host identity, ownership, and descriptions

Status: backlog; implementation has not started.

Recorded: 2026-09-07.

### Problem and outcome

Controller-created AWX hosts currently store an ownership marker in
`description`. Replacing that marker with a useful description changes how the
controller classifies the host and can prevent cleanup. Users need to describe
their hosts without affecting reconciliation, ownership, or deprovisioning.

Move VM identity into `instance_id` and ownership into a reserved host variable.
After migration, descriptions must be independent of controller metadata. A
binding transfer must leave the VM's `instance_id` unchanged.

This is a backlog design, not a claim that the proposed API filters or migration
behavior have been validated against a live AWX/AAP instance.

### Current implementation

- [util.go](controller/util.go) builds binding markers as
  `ansible-supervisor:<supervisor-id>:<namespace>/<binding-name>` and run markers
  with an additional `ansibleruns` path segment. Neither includes the owner UID.
- [awx_client.go](controller/awx_client.go) writes the marker into descriptions,
  compares it before updating hosts, and does not read or write `instance_id`.
  Variable merging currently accepts JSON objects and controller inputs are
  `map[string]string`.
- [ansiblebindingvm.go](controller/ansiblebindingvm.go) uses the description for
  ownership checks during provisioning, host relocation, and teardown. The
  child's VM owner reference already records the VM UID; `spec.bindingUID`
  records the binding incarnation.
- [ansiblebinding.go](controller/ansiblebinding.go) discovers orphan hosts with
  an exact description filter. This scan runs every four host-check periods;
  normal per-VM host checks remain separate.
- [ansiblerun.go](controller/ansiblerun.go) also depends on description ownership
  for hosts it creates and cleans up. Runs can borrow existing hosts without
  claiming them.

### Proposed metadata contract

| AWX field | Purpose | Proposed value |
|---|---|---|
| `instance_id` | Stable external VM identity | `ansible-supervisor:<supervisor-id>:vm:<vm-uid>` |
| `variables.ansible_supervisor_owner` | Controller ownership of this AWX host | Versioned object containing supervisor and Kubernetes owner identity |
| `description` | User-facing description | User-controlled text; empty by default on new hosts |

Example of the reserved variable, shown as YAML for readability:

```yaml
ansible_supervisor_owner:
  schema_version: 1
  supervisor_id: <supervisor-id>
  kind: AnsibleBinding
  namespace: <namespace>
  name: <binding-name>
  uid: <binding-uid>
```

Use `kind: AnsibleRun` and the run UID for run-created hosts. Namespace and name
support diagnosis; UID distinguishes deletion and recreation under the same
name. The controller must support this nested object internally without requiring
an unrelated expansion of every public string-valued variable field.

Use the Kubernetes `VirtualMachine.metadata.uid`, not a vCenter instance UUID,
IP address, VM name, or `AnsibleBindingVM` UID. Read the expected UID from the
recorded VM owner reference during cleanup, including after the VM is gone.
Recreating a child for the same binding and VM preserves both identities.

Do not include the binding in `instance_id`. Retaining or transferring a host
between bindings for the same VM changes ownership only. A replacement VM with
the same name has a different identity and must not silently inherit the host.
An identity match alone never authorizes transfer or deletion.

`AnsibleRun` with `vmRef` uses the same VM identity when it creates a host. For
explicit hosts without a VM reference, leave `instance_id` unset rather than
inventing a VM identity from an address or run UID. Run ownership still uses the
run UID. Record any VM UID needed for finalization before external side effects,
so run cleanup does not depend on resolving a possibly replaced VM by name.

Preserve existing `instance_id`, owner metadata, and description when borrowing
or adopting externally managed hosts. Do not stamp our ownership on them. Define
explicit conflict handling for foreign instance IDs; a populated field is not
permission to overwrite another inventory source's identity.

### Ownership and description behavior

- Reserve `ansible_supervisor_owner` across binding/run host-variable inputs and
  all paths that write host variables. Reject attempts to supply it as ordinary
  host configuration; do not silently let a merge replace ownership.
- Read ownership from the stored AWX host variables, not effective Ansible
  variables, group variables, facts, or job `extra_vars`.
- Parse and validate the existing ownership before writing anything. Centralize
  classification of owned, foreign, legacy, unmanaged, and invalid metadata.
  Missing or malformed metadata on a host with our instance-ID prefix must not
  turn it into an unmanaged host eligible for automatic adoption.
- Unknown schema versions and ownership conflicts must not authorize mutations,
  managed-host teardown, or deletion. Report an actionable status/event. Keep
  template-mode teardown's existing independent targeting contract.
- Preserve unrelated variables, including nested values, through metadata writes
  and temporary deprovision-variable restoration. Explicitly decide whether this
  feature retains the current JSON-only merge contract or adds safe YAML parsing.
- Re-read the host and validate identity and owner before destructive actions.
  Coordinate binding transfers with old finalizers and running hooks; two API
  requests are not an atomic compare-and-delete. Do not promise race protection
  based solely on a prior GET.
- New hosts start with an empty description. Reconciliation preserves later AWX
  UI/API edits, even when the text resembles a legacy marker on a migrated host.
  A declarative `hostDescription` CRD field is a separate follow-up, not required
  to free the existing AWX description field.

The reserved variable is an integration convention, not an AWX access-control
boundary. It is visible to playbooks and editable by actors with host-write
access. Inventory synchronization can modify both variables and `instance_id`;
document which inventories this controller can manage without competing writers.

### Efficient orphan discovery

Normal host checks should use the existing host response for both identity and
ownership. New-host creation should include metadata in the existing POST;
unchanged hosts should not receive a PATCH. Keep zero AWX requests for idle
children between scheduled checks.

Replacing the exact description filter requires a measured discovery strategy.
AWX stores variables as text, so do not assume a nested JSON-key filter exists.

1. Validate a candidate filter such as `variables__contains=<owner-uid>` on the
   supported AWX/AAP versions and both API base paths. Parse every returned
   candidate and compare the complete owner identity locally. A substring match
   is only a way to narrow results, never proof of ownership.
2. Measure database/query latency, response size, and pagination against the
   current description filter. A text search can still scan many database rows
   even when it returns few hosts.
3. If that filter is unavailable or unsuitable, evaluate a shared, bounded scan
   scoped by endpoint, inventory, supervisor, and credential/access context,
   using the `instance_id` prefix where supported. Index parsed owners locally.
   Do not introduce a complete inventory scan for every binding. Cached discovery
   must not replace fresh validation before deletion.
4. Handle ignored filters, false positives, malformed variables, API errors, and
   pagination limits explicitly. An incomplete scan must not be recorded as a
   successful full scan. Preserve the existing orphan-scan cadence.

The discovery choice is a prerequisite to rollout. Record request counts and
latency for one binding, many bindings sharing an inventory, multiple pages, and
an inventory dominated by unrelated hosts. Compare the same fixtures before and
after, including migration traffic separately from steady-state traffic.

### Migration and rollout

Legacy descriptions contain no VM or binding UID. Migration must not manufacture
proof of historical identity merely because a current object has the same name.

1. Add readers/classifiers for both formats before enabling migration writes.
   Recognize legacy markers only in the legacy path. A valid new-format record
   uses the new contract; description text no longer controls its ownership.
2. Define which legacy hosts can be migrated using an intact child/run record,
   expected endpoint/inventory/host coordinates, recorded owner UIDs, and VM
   identity. Enumerate ambiguous cases, including retained hosts, missing child
   status, replaced VMs, recreated bindings, and already edited descriptions.
   Where identity cannot be established, preserve the host and require an
   explicit recovery/adoption procedure. Status alone is not ownership proof.
3. Write identity and ownership together in one host PATCH where applicable,
   preserving description and unrelated variables. Verify persisted metadata
   before removing an old marker. Make each step repeatable after timeouts and
   controller restarts, without relaunching jobs or recreating hosts.
4. Remove only a description that still exactly matches the expected legacy
   marker. Preserve arbitrary user text. Establish how migration serializes
   against description edits, ownership transfer, and teardown; re-reading alone
   does not make a later PATCH conditional. If safe clearing cannot be ensured,
   leave the old text as editable text after metadata migration and document it.
5. During transition, discovery must include legacy and migrated hosts without
   double processing. Ensure invalid/conflicting new metadata cannot fall back
   to legacy matching to gain deletion authority.
6. Document controller-version coexistence and rollback before release. Old code
   may classify migrated hosts as unmanaged when descriptions are cleared.
   Keep a rollback-capable checkpoint before that step; do not promise rollback
   to a description-only controller after removing its markers.

Make retained-host reclaim and intentional binding transfer behavior explicit.
Recreating a binding changes its owner UID, so reclaiming a retained host now
requires a verified transfer instead of the existing name-only match. Designing
that procedure is required; adding an automatic transfer feature is not assumed.

### Work items

- [ ] Confirm live API field/filter behavior and choose an orphan-discovery
  strategy with recorded performance evidence.
- [ ] Implement metadata types, validation, reserved-key handling, and shared
  ownership classification in `awx_client.go` and `util.go`.
- [ ] Apply the contract to bindings, child finalizers, relocation, orphan
  cleanup, runs, and run-created/borrowed hosts. Extend persisted run identity
  and CRD status schemas where needed.
- [ ] Implement resumable migration and document ambiguous-host recovery,
  retained-host transfer, upgrade ordering, and rollback limits.
- [ ] Extend HTTP fixtures and fake AWX to support `instance_id`, nested metadata,
  candidate filtering, pagination, and request-count assertions.
- [ ] Update README, FAQ, architecture, lifecycle scenarios, contributing/live
  validation instructions, and the changelog when the behavior ships.

### Acceptance and validation

- An operator can edit a migrated host's description; reconciliation preserves
  it, and normal cleanup still works.
- Child recreation preserves identity; replacing a VM or binding under the same
  name does not inherit ownership. An intentional binding transfer preserves
  `instance_id`, and an old finalizer cannot act under the new owner's claim.
- Host rename or inventory/endpoint changes retain the existing cleanup rules
  and validate identity at the destination; no host is claimed by name alone.
- A run borrowing a binding-owned host leaves metadata, description, variables,
  and lifetime unchanged. Run-created VM and explicit hosts follow their
  respective identity rules. Reusing a run name does not grant the new run
  ownership of an earlier run's host.
- Reserved-key injection, malformed metadata, foreign IDs/owners, missing VM
  UIDs, and unknown schema versions produce the documented conservative result.
- Migration covers legacy retained hosts and in-progress teardown, lost PATCH
  responses, restart between stages, partially migrated inventories, and user
  description edits. No migration step triggers an extra job.
- Orphan tests cover unrelated-variable substring matches, ignored filters,
  pagination exhaustion, multiple bindings, and mixed metadata versions.
- Request-count checks show no extra routine per-host reads, no unchanged-host
  PATCHes, and no whole-inventory scan repeated per binding. Record any new
  cleanup verification reads separately as a correctness cost.

Run `make test-unit` and `make test-e2e` for implementation. Add live contract
checks to the pre-release validation documented in [CONTRIBUTING.md](CONTRIBUTING.md):
field round trips, filter behavior, migration, description preservation, and
inventory-source interactions. Fake AWX tests alone do not establish API
compatibility or database performance.

### Reference evidence

- AWX's [host model](https://github.com/ansible/awx/blob/devel/awx/main/models/inventory.py)
  defines `instance_id` as the remote source's host identifier (up to 1,024
  characters), stores host variables as text, and exports them to inventory.
- The [host API serializer](https://github.com/ansible/awx/blob/devel/awx/api/serializers.py)
  exposes writable `instance_id` and variables.
- [API filtering documentation](https://docs.ansible.com/projects/awx/en/latest/rest_api/filtering.html)
  describes field lookups, including `contains` and `startswith`. Specific host
  queries and performance still need validation against supported deployments.
- The [inventory importer](https://github.com/ansible/awx/blob/devel/awx/main/management/commands/inventory_import.py)
  can update host `instance_id` and variables during synchronization.

These upstream references were reviewed during the design discussion; pin the
versions used for live implementation validation.

## Teardown retries and waiting for AWX recovery

Status: backlog; implementation has not started.

Recorded: 2026-09-07.

### Problem and outcome

A failed `onDeleted` job is terminal to the controller. It records the outcome,
continues inventory-host cleanup, and releases the child finalizer once cleanup
finishes. Operators need a clear retry contract and assurance that cleanup waits
for configured Ansible retries and AWX workflow recovery steps.

Keep teardown retry policy in the playbook or AWX workflow. The controller owns
transient API retries and tracking the execution it launched. Do not add automatic
whole-job relaunch to the controller in this item.

### Current behavior and contract to document

- Ansible task retries stay within the launched job; the controller keeps waiting
  while that job is nonterminal.
- For a `WorkflowJobTemplate`, the controller polls the overall workflow job,
  rather than individual nodes. Recovery nodes within that execution remain
  covered while the workflow is nonterminal.
- A separately relaunched or scheduled job has a different execution ID and is
  not followed after the original execution finishes. AWX does not automatically
  rerun every failed job; retries or recovery must be explicitly configured.
- `onDeleted.timeoutSeconds` bounds the whole hook, including preparation,
  waiting for provisioning, AWX queueing, execution, and retries. The default is
  900 seconds. Size it for the complete retry budget plus queueing margin.
- On deadline expiry, the controller marks the hook `TimedOut` and proceeds with
  cleanup. It does not currently cancel the AWX execution, which may still be
  running. Transient inventory-host cleanup errors can continue holding the
  finalizer after the hook finishes or times out.
- A launch failure or an uncertain launch outcome is not automatically relaunched.
  Recovery inside AWX cannot cover an execution that never successfully started.

Relevant code: [hook tracking](controller/ansiblebindingvm.go),
[job/workflow polling](controller/util.go), and
[controller retries](controller/workqueue.go).

### Work items

- [ ] Document this ownership of retries in README, FAQ, and teardown scenarios,
  including the distinction between retries within one execution and a separate
  relaunch, timeout behavior, and failure notifications/manual recovery.
- [ ] Add an example of bounded task retries using `until`, `retries`, and `delay`.
  Retry transient failures; surface permanent configuration errors. Make cleanup
  idempotent: an already absent record succeeds, and repeated execution cannot
  remove a replacement VM's records.
- [ ] Add an AWX workflow example with explicit, bounded recovery attempts under
  one workflow execution. Ensure exhausted recovery produces a failed overall
  workflow; a successful notification or rescue node must not mask incomplete
  cleanup. Verify this behavior against the supported AWX/AAP version.
- [ ] Add focused regression coverage proving the controller holds the finalizer
  and inventory host while the overall workflow is pending/running through
  recovery, then cleans up only after a terminal outcome or hook deadline.
- [ ] Validate the example against AWX/AAP and document how operators calculate
  the hook deadline from retries, execution time, and queueing allowance.

### Acceptance and validation

- A workflow with a failed first attempt and a successful recovery attempt stays
  tracked by the same workflow ID. The controller launches it once and waits for
  its final result before inventory-host cleanup.
- Exhausted recovery reports `Failed`; cleanup proceeds and the existing outcome
  reporting identifies the AWX execution. Transient polling errors retry without
  launching another execution or resetting the hook deadline.
- Controller restart resumes tracking the recorded workflow. A separately
  relaunched job is explicitly outside the tracking contract.
- Deadline coverage confirms the current timeout behavior, including that cleanup
  may proceed while AWX is still running. No indefinite hook wait is introduced.
- Run the relevant controller unit tests and validate the workflow example on a
  supported AWX/AAP instance; a mocked overall status alone does not prove AWX
  workflow failure propagation.

### Follow-up boundary

Guaranteed eventual cleanup after long outages needs a durable cleanup record
that survives VM, child, and inventory-host deletion, with enough identity and
external record information for safe recovery. This item does not implement that
mechanism or change timeout cancellation behavior. Document the limitation; logs
and Kubernetes Events are diagnostic records, not a durable retry queue.

### References

- [Ansible task retries](https://docs.ansible.com/projects/ansible-core/devel/playbook_guide/playbooks_loops.html#retrying-a-task-until-a-condition-is-met)
- [AWX workflow execution and recovery branches](https://github.com/ansible/awx/blob/devel/docs/workflow.md)
- [AWX job templates, relaunch, and scheduling](https://docs.ansible.com/projects/awx/en/24.6.1/userguide/job_templates.html)

## AnsibleRun replacement through Argo CD

Status: backlog; implementation has not started.

Recorded: 2026-09-07.

### Problem and outcome

The Argo CD guide discourages replacement and implies that Job immutability
behaves differently in this respect. `Replace=true` updates an existing object
and does not bypass immutable execution fields on either resource. Explicit
delete-and-recreate is already a new execution for `AnsibleRun`, including when
the replacement uses the same name.

Keep the execution spec immutable and one execution per resource UID. Document
explicit replacement as a supported way to request another run, and validate
the complete Argo CD lifecycle before claiming the examples are tested.

### Work items

- [ ] Correct the Job comparison and blanket replacement discouragement in
  `ARGOCD.md` and the tracked-once example. Explain `Replace=true` versus
  `Force=true,Replace=true`: the latter deletes and recreates the resource,
  requesting another execution even if its spec is unchanged.
- [ ] Add a fixed-name `PostSync` example using
  `hook-delete-policy: BeforeHookCreation,HookSucceeded`. Explain that the old
  hook is deleted before its replacement is created, and a failed hook remains
  available until the next replacement. Keep `generateName` as the alternative
  for separate execution history.
- [ ] Document when to use a tracked one-time resource, a repeating hook, or
  explicit force replacement. Include the required Argo health customization,
  a bounded run deadline, and the fact that replacement requests another
  execution rather than resuming the previous job.
- [ ] Explain that deletion can wait on the run finalizer: active AWX work is
  canceled and confirmed stopped before owned hosts are cleaned up. Cover
  transient AWX outages, existing abandonment paths, and inventory ownership
  when the replacement reuses the same name. Do not imply an immediate reset.
- [ ] Add focused regression coverage where missing and validate fixed-name
  hook replacement and force replacement end to end on a recorded Argo CD and
  Kubernetes version. Fix lifecycle issues exposed by those checks without
  weakening spec immutability.

### Acceptance and validation

- Reapplying an unchanged existing run does not launch another job. An update
  that changes the execution spec, including `Replace=true`, is rejected.
- Deleting and recreating the same name produces a new UID and one new execution.
  Stale status writes, queued reconciles, and TTL cleanup from the old UID cannot
  mutate or delete its replacement.
- Replacing a completed or failed run completes its required host cleanup first.
  Replacing an active run requests cancellation and waits for confirmation before
  deleting owned hosts and allowing same-name recreation. Transient failures
  retain the finalizer; existing permanent-abandonment behavior is documented.
- A fixed-name PostSync hook executes on successive full syncs and Argo waits for
  its terminal outcome using the health customization. Document that hooks do not
  execute during selective sync.
- The force-replacement example produces a new execution on the tested sync path;
  neither example requires making the spec mutable or bypassing finalizers.
- Run the relevant controller tests and a live Argo CD integration check. Unit
  tests alone do not establish Argo deletion ordering or hook completion behavior.

### References

- [Argo CD replacement and force sync](https://argo-cd.readthedocs.io/en/stable/user-guide/sync-options/#force-sync)
- [Argo CD hook lifecycle](https://argo-cd.readthedocs.io/en/stable/user-guide/sync-waves/#hook-lifecycle-and-cleanup)
- [Kubernetes Job validation](https://github.com/kubernetes/kubernetes/blob/master/pkg/apis/batch/validation/validation.go)
- Current implementation: [run lifecycle and finalization](controller/ansiblerun.go)
  and [CRD immutability](controller/manifests/crd.yml).
