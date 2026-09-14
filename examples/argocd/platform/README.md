# platform/

The one-time Argo CD configuration. It's installed once by whoever
administers Argo CD, alongside this service, rather than by every
application.

## What is here

- [`argocd-cm.yml`](argocd-cm.yml) - health checks for `AnsibleRun`,
  `AnsibleBinding`, `AWXConnection` and `VirtualMachine`.

## Why it is not optional

Argo CD ships a health check for `batch/v1 Job` and none for custom
resources. An unknown CR is reported `Healthy` the moment it's created,
so without this:

- a sync wave does not wait for a playbook,
- a `PostSync` hook does not gate on one,
- the application goes green while the AWX job is still queued.

That's a silent failure. Nothing errors, the sync is just meaningless.

## Applying it

```bash
kubectl apply -f argocd-cm.yml
kubectl -n argocd rollout restart deploy/argocd-server
```

If you already maintain `argocd-cm`, merge the four `data` keys rather
than replacing the ConfigMap.

Confirm it took against a live object:

```bash
argocd app resources <app> --output tree
kubectl get ansiblerun -n <namespace>     # READY / STATE / JOB columns
```

The key format is `resource.customizations.health.<group>_<Kind>`, so group
and kind separated by an underscore. Getting it wrong is indistinguishable
from not installing it at all, which is why the check above is worth doing
once.

## What is deliberately not here

The `AWXConnection` and the `Secret` holding the AWX API token. Those are
per-namespace, created out of band by the platform team, and must not live
in an application's Git repo. See
[ARGOCD.md](../../../ARGOCD.md#2-create-the-awxconnection-out-of-band).
