# Argo CD examples

Four independently runnable directories. Each one stands alone: its own
manifests, its own prerequisites, its own way of being torn down. There is
no aggregate application that runs all of them.

| Directory | What it shows |
|---|---|
| [`platform/`](platform/) | The one-time Argo CD configuration. Without it, every scenario below reports `Healthy` while its playbook is still queued in AWX |
| [`tracked-once/`](tracked-once/) | A run as a tracked resource: executes on the first sync, never again |
| [`postsync/`](postsync/) | A run as a `PostSync` hook: a smoke test on every sync, against a host an `AnsibleBinding` manages |
| [`external-teardown/`](external-teardown/) | Decommissioning a guest before the VM is destroyed, sequenced from outside Argo |

[ARGOCD.md](../../ARGOCD.md) is the reasoning behind all of it - resource
versus hook, immutability, waves, and what a run may write into the AWX
inventory. Read that first; these are the runnable form of it.

## Common prerequisites

All four assume:

- This service installed on the Supervisor, and an `AWXConnection` named
  `awx` already `Ready` in the target namespace. It is created by the
  platform team out of band, never committed to an application's repo -
  see [ARGOCD.md](../../ARGOCD.md#2-create-the-awxconnection-out-of-band).
- The AWX job templates each example names, with **Prompt on Launch**
  enabled for the fields the run supplies. A template that ignores a limit
  would run against the whole inventory, so the controller refuses to
  launch rather than let that happen.
- `platform/` applied to the Argo CD installation.

Namespaces in these files are `my-namespace`. Change them, or drop the
field and let the `Application`'s destination supply it.
