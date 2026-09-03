# Quickstart

Shortest path from nothing to a playbook running against a VM Service VM. Five steps, about fifteen minutes if AWX is already up.

Everything here is explained in more depth in the [README](README.md); this page is the copy-paste version. For driving this from a VCF Automation 9.x blueprint instead of `kubectl`, see [Using this from a VCFA 9.x blueprint](VCFA-BLUEPRINTS.md).

- [Before you start](#before-you-start)
- [1. Install the service](#1-install-the-service)
- [2. Prepare the AWX template](#2-prepare-the-awx-template)
- [3. Connect the namespace to AWX](#3-connect-the-namespace-to-awx)
- [4. Create a target VM](#4-create-a-target-vm)
- [5. Bind the VM to the template](#5-bind-the-vm-to-the-template)
- [Re-running](#re-running)
- [When it doesn't work](#when-it-doesnt-work)
- [Cleaning up](#cleaning-up)
- [What next](#what-next)

## Before you start

- A VCF supervisor with VM Service enabled, and `kubectl` logged in to a namespace on it
- An AWX / Ansible Tower / AAP instance that can reach your VMs over SSH
- An SSH keypair whose private half is in an AWX **Machine credential**, and whose public half you'll bake into the VM

The controller never touches a VM. It talks to Kubernetes and to AWX's HTTPS API only - AWX does the SSH. So the supervisor needs no route into the workload network, but AWX does.

## 1. Install the service

vCenter → **Workload Management → Services → Add Service**, upload `ansible-supervisor.yml` from the [latest release](https://github.com/warroyo/ansible-supervisor-service/releases), and install. Defaults are fine.

Set `supervisor_id` to something readable (e.g. `sup-lab-01`) if more than one supervisor will share this AWX instance. Left empty it's derived from a namespace UID, which works but makes the AWX inventory hard to read.

Confirm the CRDs landed:

```bash
kubectl get crd | grep field.vmware.com
# ansiblebindings.field.vmware.com
# ansibleruns.field.vmware.com
# awxconnections.field.vmware.com
```

## 2. Prepare the AWX template

In AWX, on the Job Template you want to run:

- **Enable Prompt on Launch for Limit.** Without it AWX discards the limit the controller sends and runs your playbook against every host in the inventory. The controller refuses to launch rather than let that happen - [why](FAQ.md#why-does-my-template-need-prompt-on-launch-for-limit).
- Enable Prompt on Launch for Variables too, if you plan to pass `extraVars`.
- Attach the **Machine credential** holding the private key. This is what logs into the VM.
- Note which **inventory** the template uses. That's where the controller creates host entries.
- **Workflow templates usually have no inventory of their own**, since each node carries one. With no inventory there is no host for the controller to create and nothing for `--limit` to scope, so the workflow runs against whatever its nodes target - the same as `useDefaultLimit: true`. Use a Job Template if you need the run confined to the selected VM. [More](FAQ.md#whats-different-about-workflow-templates)

Then create an API token (AWX → your user → **Tokens** → Add, scope `write`) and keep it handy.

## 3. Connect the namespace to AWX

Write the token to a file first. A token passed as `--from-literal` lands in your shell history and is visible to anyone who can run `ps` while the command runs.

```bash
umask 077 && cat > awx-token   # paste the token, then Ctrl-D

kubectl create secret generic awx-token -n my-namespace \
  --from-file=token=./awx-token

kubectl apply -n my-namespace -f - <<'EOF'
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: sample-awx
spec:
  url: "https://awx.example.com"
  secretRef: "awx-token"
  insecureSkipVerify: false   # true only for self-signed test instances
EOF
```

If AWX is served by a private CA, trust it rather than skipping verification. The bundle goes in a Secret, and the token Secret you just created will do - add the PEM to it under `ca.crt`:

```bash
kubectl create secret generic awx-token -n my-namespace \
  --from-file=token=./awx-token \
  --from-file=ca.crt=/path/to/ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -
```

```yaml
spec:
  url: "https://awx.example.com"
  secretRef: "awx-token"
  caBundleSecretRef:
    name: "awx-token"         # key defaults to ca.crt
```

Delete the local `awx-token` file once the Secret exists.

`insecureSkipVerify: true` and `caBundleSecretRef` together are rejected.

Leave `apiBasePath` unset - the controller probes for `/api/v2` (AWX, Tower, AAP ≤ 2.4) versus `/api/controller/v2` (AAP 2.5+) itself. Within a few seconds:

```bash
kubectl get awxconnection -n my-namespace
# NAME         READY   STATE   API       AGE
# sample-awx   true    Ready   /api/v2   5s
```

Anything other than `Ready` here is a bad URL, a bad token, or TLS - `kubectl get awxconnection sample-awx -n my-namespace -o jsonpath='{.status.message}'` says which. Fix it before moving on; nothing downstream will work until this is `Ready`.

## 4. Create a target VM

Nothing gets installed on the VM. It needs exactly two things: a label for the selector to match, and an SSH user matching the AWX Machine credential.

Fill in `className`, `imageName` and `storageClass` from your own namespace (`kubectl get virtualmachineclass,virtualmachineimage,storageclass`) and apply:

```bash
kubectl apply -n my-namespace -f examples/virtualMachine.yml
```

The only line in that file this service cares about is `labels: {app: webserver}`. The rest is an ordinary VM Service VM, and the cloud-init block is just how the public key gets in - use whatever bootstrap you already have.

The example is written against `vmoperator.vmware.com/v1alpha2`, which VCF 9.x Supervisors still serve alongside newer versions. Any served version works - the controller discovers which one to read at startup - so if the apply is rejected, set `apiVersion` to whatever your Supervisor offers:

```bash
kubectl get crd virtualmachines.vmoperator.vmware.com \
  -o jsonpath='{range .spec.versions[?(@.served)]}{.name}{"\n"}{end}'
```

No need to wait for the VM to boot before moving on. A VM that is powered off or has no IP yet sits in `Pending`, and the controller starts the run when the IP appears - the reconcile loop re-checks every matched VM on each resync (`-resync-period`, 60s by default). Ordering only matters if you want the binding to go `Ready` promptly.

To watch the VM come up anyway:

```bash
kubectl get vm sample-webserver -n my-namespace \
  -o jsonpath='{.status.powerState} {.status.network.primaryIP4}{"\n"}'
# poweredOn 10.10.20.31
```

## 5. Bind the VM to the template

```bash
kubectl apply -n my-namespace -f - <<'EOF'
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: webserver-config
spec:
  vmSelector:
    app: webserver
  awxConnectionRef: sample-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF
```

That's the whole thing. The controller creates an AWX inventory host pointed at the VM's IP and launches the template scoped to it with `--limit`.

```bash
kubectl get ansiblebinding webserver-config -n my-namespace -w
# NAME               READY   STATE     AGE
# webserver-config   false   Pending   2s
# webserver-config   false   Running   8s
# webserver-config   true    Ready     54s
```

`Ready` means the playbook completed successfully against every matched VM. To see the run in AWX:

```bash
kubectl get ansiblebinding webserver-config -n my-namespace \
  -o jsonpath='{range .status.vms[*]}{.name}{"\t"}{.phase}{"\t"}{.lastJobURL}{"\n"}{end}'
# sample-webserver  Succeeded  https://awx.example.com/#/jobs/playbook/412
```

One binding fans out: every VM the selector matches gets its own inventory host, its own run, and its own entry in `status.vms`. Label a second VM `app: webserver` and it's picked up on the next resync - no edit to the binding needed.

## Re-running

Bump the annotation:

```bash
kubectl annotate ansiblebinding webserver-config -n my-namespace \
  ansible.field.vmware.com/reconcile-requested-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

Editing `spec` does the same thing without the annotation, since it bumps `.metadata.generation`.

## When it doesn't work

| What you see | Usually means |
|---|---|
| `AWXConnection` not `Ready` | Wrong URL, bad token, or TLS. Read `.status.message` |
| Binding stuck `Pending` | No VM matches the selector, or the matched VM is powered off / has no reported IP yet. Clears on its own once a matching VM reports an IP - no need to re-apply the binding |
| Binding `Failed` before any job ran | Prompt on Launch for Limit is off on the template, or the inventory host name collides with one another supervisor owns. `.status.message` names which |
| Binding `Failed` with a job URL | AWX ran it and the playbook failed. Open `lastJobURL` |
| Job fails `unreachable` | AWX can't SSH in: wrong user or key in the Machine credential, no route to the VM's IP, or the VM booted without the key. If AWX must reach the VM at a different address than `status.network.primaryIP4`, set `hostVariables: {ansible_host: ...}` on the binding |

Full reference in the [README](README.md), edge cases in the [FAQ](FAQ.md).

## Cleaning up

Delete the `AnsibleBinding` **while the controller is still running** - it carries a finalizer that blocks until its AWX inventory hosts are gone:

```bash
kubectl delete ansiblebinding webserver-config -n my-namespace
```

Remove the service last, after the bindings are gone.

## What next

This walkthrough builds an `AnsibleBinding`: standing state that says "these VMs should be configured like this", re-runnable forever.

The other shape is `AnsibleRun` - one AWX job, launched once, terminal. That is what you want for something that has already happened rather than something that should stay true: registering a VM in DNS, patching two servers tonight, running a decommission playbook before a VM is destroyed, or calling an external API with no host involved at all. See [AnsibleBinding or AnsibleRun?](README.md#ansiblebinding-or-ansiblerun) and `examples/ansibleRun.yml`.
