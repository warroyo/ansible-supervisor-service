# tracked-once/

One execution, owned by Argo, tied to the application's own lifecycle.

## Prerequisites

- [`../platform/`](../platform/) applied to the Argo CD installation.
- An `AWXConnection` named `awx`, `Ready`, in `my-namespace`.
- An AWX job template `Register DNS Record` with **Prompt on Launch**
  enabled for **Variables** (the run supplies `extraVars`).

## What happens

| Event | Result |
|---|---|
| First sync | The `AnsibleRun` is created, the controller launches one AWX job, and the wave does not advance until it finishes |
| The job succeeds | `state: Ready`, and Argo reports the resource `Healthy` |
| The job fails | `state: Failed` with a link to the job output, and Argo reports `Degraded`. It is **not** retried - the playbook ran and failed |
| Re-sync, manifest unchanged | Nothing. The object is already there and its spec has not changed |
| Re-sync after editing the manifest's `spec` | The API server **rejects** it: `spec is immutable`. See below |
| Controller restart mid-flight | It picks the job back up by id and keeps polling. No second job |

## To run it again

Change `metadata.name`. A new name is a new object, which is a new request,
which is a new execution. Do not reach for `Replace=true` - a replace is
still an update and is rejected the same way, and `Replace=true` plus
`Force=true` degrades to delete-then-create, which re-runs a playbook that
already ran. That is fine for a DNS registration and an incident for a
decommission.

If you want "re-run when this input changes", derive the name from the
input and set `prune: true` so the superseded object is removed:

```yaml
metadata:
  name: register-dns-v2.1.4     # or a hash of the whole spec
```

## Applying it

```bash
kubectl apply -f ansiblerun.yml
kubectl get ansiblerun register-dns -n my-namespace -w
```

## Cleaning up

```bash
kubectl delete ansiblerun register-dns -n my-namespace
```

Deletion cancels the AWX job if one is still running and waits for AWX to
confirm it stopped before releasing. This run creates no inventory hosts,
so there is nothing else to remove.
