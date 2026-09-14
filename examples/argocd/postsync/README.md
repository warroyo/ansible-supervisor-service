# postsync/

A smoke test on every sync, against a host something else owns.

This is the example that shows the ownership split, which is the part of
`AnsibleRun` most likely to surprise you.

## Prerequisites

- [`../platform/`](../platform/) applied to the Argo CD installation.
- An `AWXConnection` named `awx`, `Ready`, in `my-namespace`.
- A `VirtualMachine` named `web-1` in `my-namespace`, powered on and
  reporting an IP, labelled `app: web`.
- Two AWX job templates, both sharing an inventory:
  - `Configure Webserver` - **Prompt on Launch** for **Limit**
  - `Smoke Test` - **Prompt on Launch** for **Limit** and **Variables**

## What happens

| Event | Result |
|---|---|
| First sync | Wave 1 creates the binding, which provisions the AWX host for `web-1`. Wave 2's hook then runs the smoke test against that same host |
| Every later sync | A new `smoke-test-xxxxx` object, one new AWX job, same host id as before |
| A hook succeeds | Argo deletes the object. The binding's host is untouched - the run never owned it |
| A hook fails | Argo keeps the object, `state: Failed`, `status.jobURL` links to the output. The next sync's hook still runs: it does not have to wrestle the previous one for the host |
| The binding reconciles afterwards | Its host's variables are exactly as it left them. Nothing the run passed has leaked into the inventory |
| Selective sync of one resource | Hooks are **skipped**. "Runs on every sync" means every full sync |

## The rule this demonstrates

A run executes against an existing host and never writes to it. Only a host
the run itself created may be written to, and only that host is deleted when
the run is.

So if you add `hostVariables` here, the run will refuse to launch:

```
inventory host "web-1" already exists and is not this run's to change ...
```

That refusal is the whole point. Silently dropping the values would run the
playbook with a configuration the spec asked for and AWX never saw. Put
per-execution values in `extraVars`/`varsFrom` instead, which is what this
example does.

## Applying it

```bash
kubectl apply -f ansiblebinding.yml
# then sync the application, or apply the hook by hand to see it run:
kubectl create -f ansiblerun.yml
kubectl get ansiblerun -n my-namespace -w
```

Use `kubectl create` rather than `apply`, because `generateName` has no name
to apply against.

## Cleaning up

```bash
kubectl get ansiblerun -n my-namespace          # find the smoke-test-xxxxx names
kubectl delete ansiblerun smoke-test-xxxxx -n my-namespace
kubectl delete ansiblebinding webservers -n my-namespace
```

Delete the runs first if you want to watch it happen. They leave the host
where it is, and the binding's own deletion is what finally removes it.
