# Demo

`make demo` is a guided walkthrough of the service for showing it to people. It prints every manifest and command before it runs it, then waits for Enter, so you can talk over each step. It uses the same `.env` as the [pre-release gate](CONTRIBUTING.md#pre-release-validation), so if `make verify-supervisor` works, this works.

If you want to learn the service rather than show it, [QUICKSTART.md](QUICKSTART.md) is the better place to start. This page assumes the service is already installed and AWX is already set up.

- [What it shows](#what-it-shows)
- [Before you start](#before-you-start)
- [Running it](#running-it)
- [Cleaning up](#cleaning-up)
- [Things that trip it up](#things-that-trip-it-up)
- [Standing up a test AWX on a VKS cluster](#standing-up-a-test-awx-on-a-vks-cluster)

## What it shows

1. **Connect the namespace to AWX.** A Secret holding the token and an `AWXConnection` pointing at it, which goes Ready once the controller has authenticated.
2. **Create a VM.** An ordinary VM Service VM. The only thing that matters to this service is its label, `app=asb-demo`.
3. **Bind it to a template.** An `AnsibleBinding` selecting that label. The controller adds the VM to the template's inventory and launches the job limited to that one host. This is the step to switch to the AWX UI for.
4. **Scale out.** A second VM with the same label. Nothing about the binding changes, and it picks the VM up on its own.
5. **A one-off run.** An `AnsibleRun` against the first VM. It borrows the host the binding already made rather than creating its own.
6. **Delete a VM.** The binding's `onDeleted` hook runs the deprovision template for it, and then its host leaves the inventory. The outcome lands as an Event on the binding.

Everything is left running at the end, so you can keep poking at it afterwards.

## Before you start

- The service installed on the Supervisor (`kubectl get crd | grep field.vmware.com`)
- `.env` filled in from `.env.example`. The demo reads `SUPERVISOR_NS`, `AWX_URL`, `AWX_TOKEN`, `AWX_TEMPLATE` and `SSH_PUBLIC_KEY_FILE`
- `AWX_DEPROVISION_TEMPLATE` set too if you want step 6. Without it that step is skipped
- The AWX templates set up the way [QUICKSTART.md § 2](QUICKSTART.md#2-prepare-the-awx-template) describes: Prompt on Launch for Limit, and for Variables as well since the demo passes `extraVars`. The deprovision template needs both, and must use the same inventory
- Storage quota for two VMs in the namespace. See [below](#things-that-trip-it-up)

## Running it

```bash
make demo
```

Each VM takes a few minutes to boot, and the script waits for cloud-init to finish before it moves on (`FIXTURE_SETTLE_SECONDS`, 90 by default). Doing the whole thing live takes about fifteen minutes. If you'd rather not wait in front of people, run the first two steps before the meeting and start talking at step 3.

`DEMO_PAUSE=0 make demo` runs it straight through without waiting for Enter, which is useful for checking the demo still works before you rely on it.

Every object the demo creates is named `asb-demo-*`, and its VMs carry `app=asb-demo`, so it never touches anything else in the namespace. `DEMO_PREFIX` changes the prefix if two people want to run it in the same namespace.

## Cleaning up

```bash
make demo-clean
```

It deletes the `AnsibleRun` and the `AnsibleBinding` first, while the VMs and the `AWXConnection` still exist. The binding's finalizer needs both to take its hosts back out of AWX. Then it removes the VMs, the connection, and the token Secret.

## Things that trip it up

**The IP shows up before SSH is ready.** vm-operator reports the address while cloud-init is still writing `authorized_keys`. The script waits for this, but if you drive the same steps by hand, give it a minute and a half after the IP appears before you create the binding. Otherwise the first job fails at the SSH step.

**No images in the namespace.** The VMs boot from whatever single `VirtualMachineImage` the namespace has. A namespace that can only see cluster-wide images (`kubectl get cvmi`) has to name one with `VM_IMAGE` in `.env`.

**Storage quota.** Every image VKS ships has a 20Gi disk, so the demo needs about 40Gi of free quota in the namespace. When it runs out, the VM never gets a disk and the script times out waiting for an IP. `kubectl get storagepolicyquota -o yaml` shows the limit and what's used.

**`hello_world.yml` doesn't change anything.** If your templates run AWX's Demo Project, the jobs succeed without changing the VM. That's fine for showing the flow. If you want the audience to see a change on the VM, point the template at a playbook that installs something visible, such as nginx.

## Standing up a test AWX on a VKS cluster

You can't put AWX on the Supervisor itself. The awx-operator Helm chart installs CRDs and a ClusterRole, and the Supervisor's RBAC won't let anyone create those, not even a vSphere administrator. A VKS cluster in the same namespace works fine, and AWX on it can reach the VMs over SSH because they share the namespace network.

This is how the lab this repo is validated against is set up. It is a test AWX, not a production one: plain HTTP, and the admin user's token.

```bash
# The cluster's admin kubeconfig, from the Supervisor
kubectl -n my-namespace get secret <cluster>-kubeconfig \
  -o jsonpath='{.data.value}' | base64 -d > vks.kubeconfig
export KUBECONFIG=$PWD/vks.kubeconfig

helm repo add awx-operator https://ansible-community.github.io/awx-operator-helm/
kubectl create ns awx
kubectl label ns awx pod-security.kubernetes.io/enforce=privileged
helm install awx-operator awx-operator/awx-operator -n awx --wait

# VKS clusters have no default StorageClass, so name one for Postgres.
kubectl apply -f - <<'EOF'
apiVersion: awx.ansible.com/v1beta1
kind: AWX
metadata:
  name: awx
  namespace: awx
spec:
  service_type: LoadBalancer
  postgres_storage_class: <a storage class from `kubectl get sc`>
  postgres_storage_requirements:
    requests:
      storage: 2Gi
  postgres_data_volume_init: true
EOF

kubectl -n awx get svc awx-service          # EXTERNAL-IP is AWX_URL
kubectl -n awx get secret awx-admin-password -o jsonpath='{.data.password}' | base64 -d
```

It takes five to ten minutes to come up, most of it database migrations. The Postgres volume counts against the Supervisor namespace's storage quota like any other, which is why the example asks for 2Gi rather than the default 8Gi.

Then in AWX, add:

- a Machine credential: user `ansible`, plus the private half of `SSH_PUBLIC_KEY_FILE`
- an inventory for the controller to write hosts into
- the provisioning template, with Prompt on Launch for Limit and Variables, that inventory, and the credential
- the deprovision template, set up the same way
- a `write`-scoped token for your user, which goes in `AWX_TOKEN`

Using the Demo Project's `hello_world.yml` for both templates is enough to run the demo and the release gate.
