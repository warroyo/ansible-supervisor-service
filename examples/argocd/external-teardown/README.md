# external-teardown/

Decommissioning a guest before its VM is destroyed - the one thing that has
no reliable Argo shape, sequenced from outside instead.

## The problem

A decommission playbook that logs into the guest - flushing a queue,
deregistering from a cluster, taking a final backup - has to run while the
VM is still up.

- `PostDelete` runs after the application's resources are gone. The VM is
  already destroyed and the playbook SSHes into nothing.
- A finalizer on the `VirtualMachine` does not help either: vm-operator's
  own finalizer destroys the vSphere VM during its finalization.
- VM Service has a real pre-delete mechanism, and this service is not
  allowed to use it -
  [why](../../../FAQ.md#why-is-there-no-pre-delete-hook).
- Newer Argo CD documents a `PreDelete` hook, which fires before the
  application's resources are removed and would be the right shape. **This
  repo does not pin a minimum version for it and does not test it.** Verify
  it against your own Argo CD before depending on it; the cost of being
  wrong is a guest destroyed with its decommission unexecuted.

So the ordering becomes the caller's problem. That is a genuine regression
from what `Cloud.Ansible.Tower` could do with `templates.de-provision[]`,
and it is worth being straight about.

## What is here

- [`ansiblerun.yml`](ansiblerun.yml) - the decommission run. Applied by the
  script, not by Argo.
- [`teardown.sh`](teardown.sh) - run it, wait for it, and only then delete
  the application.

## Prerequisites

- [`../platform/`](../platform/) applied (the script reads `status.state`
  directly, but you want the health checks for everything else).
- An `AWXConnection` named `awx`, `Ready`, in the namespace.
- An AWX job template `Decommission Guest` sharing an inventory with
  whatever manages the VM's host, with **Prompt on Launch** enabled for
  **Limit** and **Variables**.
- `argocd` CLI logged in, and `kubectl` pointed at the Supervisor.

## Using it

```bash
./teardown.sh my-app my-namespace
```

## What it guarantees

| Outcome | What the script does |
|---|---|
| Run reaches `Ready` | Deletes the application |
| Run reaches `Failed` | **Stops**, prints `status.message`, exits non-zero. The VM is left alive |
| Run never finishes within `TIMEOUT` | **Stops**, exits non-zero. The VM is left alive |

`activeDeadlineSeconds` in the manifest is the controller's own bound: on
expiry it cancels the AWX job and marks the run `Failed`, so the script's
timeout is a backstop rather than the only thing ending it.

## Why the name carries a timestamp

Every teardown creates a fresh object. Reusing a fixed name would either be
rejected - the object still exists and its spec is immutable - or, worse,
find a **successful run left over from an earlier attempt** and read it as
this attempt's success, authorizing the deletion of a VM that was never
decommissioned.

## Afterwards

The `AnsibleRun` outlives the application deliberately: it is the record
that the decommission ran, with a link to the AWX job output. Delete it
when you no longer need that.

Note that it needs the `AWXConnection` and its `Secret` to still exist while
it finalizes, which is one more reason to keep those out of the application
([one-time setup](../../../ARGOCD.md#2-create-the-awxconnection-out-of-band)).
