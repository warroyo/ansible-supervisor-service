# Contributing

## Layout

| Path | What's in it |
|---|---|
| `controller/` | the Go controller - one file per concern (`awxconnection.go`, `ansiblebinding.go`, `awx_client.go`, `engine.go`) |
| `controller/manifests/crd.yml` | both CRDs; copied into `config/` at release time |
| `config/` | ytt templates for the deployed service: `Deployment`, `ServiceAccount`, RBAC, values schema |
| `examples/` | the three manifests a user applies |
| `test/` | the e2e suite and its fake AWX server, plus the live pre-release gate: `install-supervisor-service.sh`, `fixture.sh`, `verify-supervisor.sh` |

## Testing

You don't need a supervisor or a real AWX/Tower instance. The controller's output is plain Kubernetes objects and an AWX API client, so the whole suite runs against a local [kind](https://kind.sigs.k8s.io/) cluster and a fake AWX server (`test/fakeawx`).

```bash
make test-unit   # go vet + unit tests
make test-e2e    # full kind-based suite (requires kind, kubectl, ytt, go)
```

Before cutting a release there is one more gate that *does* need both, because it validates the packaged artifact rather than the code: [Pre-release validation](#pre-release-validation).

The e2e suite runs the controller authenticated as its own service account (`ansible-supervisor`) with a minted token - every API call is authorized against the real `ClusterRole` from `config/deploy.yml`, so a missing RBAC rule fails the suite with `Forbidden` just like it would in a real deployment. It covers:

- `AWXConnection` validation against the fake AWX
- launching a job and polling it to completion
- a re-run triggered by the annotation actually launching a new job
- a powered-off VM keeping its completed phase, and a re-run requested during downtime being honored once it returns
- an unchanged host never being re-PATCHed across resyncs
- refusing to launch against a template without `ask_limit_on_launch` (and asserting **no** job was started)
- rejecting an empty `vmSelector`
- adopting a pre-existing AWX host without wiping its variables, and not deleting it on cleanup
- tolerating an instance that ignores the `?name=` host filter (the unrelated host is left untouched and never deleted, and the binding still creates its own)
- detecting `/api/v2` vs `/api/controller/v2` against two fake instances, and running a full launch/poll cycle through the AAP 2.5-style gateway path
- refusing a host owned by another supervisor (nothing written, no job launched), and `hostNamePrefix` resolving that collision
- `cleanupPolicy: Retain` → delete → recreate reclaiming ownership of the retained host, which is then deletable again
- per-VM AWX host cleanup when a VM drops out of `vmSelector`, and finalizer-driven cleanup on delete

Debugging: `test/e2e.sh --keep` leaves the kind cluster, controller, and fake AWX server running on failure, and prints both logs on any failed assertion.

### What the suite can't cover

`test/fixtures/vm-crd.yml` is a simplified stand-in rather than the real `vmoperator.vmware.com` CRD, and `test/fakeawx` stands in for a real AWX. The suite also runs the controller as a host binary rather than the built image, so nothing it does exercises the Dockerfile, the Carvel package, the pinned digest, or the real `ClusterRole`.

Those gaps only a live environment closes, and `make verify-supervisor` closes them - see [Pre-release validation](#pre-release-validation). It covers:

- `status.network.primaryIP4` read through the negotiated API version against a live VM
- a real AWX token and Machine credential SSHing into a VM Service VM, with `--limit` honored
- the in-cluster startup path under the service's own `ClusterRole`
- one binding fanning out to several VMs, each with its own inventory host and run

Run it after you change VM lookup, the AWX client, RBAC, or the packaging.

## Pre-release validation

CI proves the code works. It cannot prove the *release* works: a hosted runner has no route to a Supervisor or to an AWX instance, so the built image, the packaging, the digest pinning and the in-cluster RBAC all reach users unexercised. This is the gate that covers them, run by hand before every tag.

Nothing here is wired into a workflow. `make test-unit` and `make test-e2e` remain the whole of what GitHub Actions runs.

**1. Cut a dev release.** An ordinary release under a pre-release version, so it goes through the real build, the real digest pinning and the real package assembly rather than a parallel path that could drift from them:

```bash
make dev-release DEV_VERSION=1.0.1-rc1
```

**2. Install it.** The Supervisor's own RBAC refuses even a full vSphere administrator the right to create CRDs, ClusterRoles, PackageInstalls, namespaces or ServiceAccounts, so `kubectl apply` is not a fallback here - the vCenter service-install path is the only way this service lands. Broadcom documents it as a UI flow, but the REST endpoints exist and are drivable, so it is scripted:

```bash
read -rs "P?vCenter password: " && printf '%s' "$P" > /tmp/vc_pw && unset P
export VC_HOST=vc01.example.lab VC_USER=administrator@vsphere.local VC_PASSWORD_FILE=/tmp/vc_pw
export KUBECONFIG=/path/to/supervisor.kubeconfig    # optional, enables the rollout check
make install-supervisor-service DEV_VERSION=1.0.1-rc1
```

It probes whether this vCenter serves `/namespace-management/clusters` or `/supervisors` rather than assuming (the widely-circulated PowerShell example uses the latter, which 404s on some vCenters), registers the service if it is new or adds the version if it is not, then installs or upgrades in place. `refName` and version are read out of the generated package, so it cannot install a version the file does not contain. The password goes to curl through a config file on stdin, not `-u`, so it never appears in the process list.

Two things worth knowing about the API: **every successful write returns an empty body** - no id, no confirmation, not even `{}` - so the script reads each one back rather than trusting the status code. And **`current_version` flips before the Deployment rolls**; it reflects the PackageInstall, not the workload, and has been seen reporting the new version with a 12-minute-old pod still on the old image. With `KUBECONFIG` set the script waits for the rollout; either way step 3 asserts the running digest, which is the real answer.

The service installs cluster-wide into an auto-named `svc-<name>-<random>` namespace that you don't choose and that is stable across upgrades. Nothing lands in the tenant namespace you test in.

**3. Validate against the live environment.**

```bash
export KUBECONFIG=/path/to/supervisor.kubeconfig
make verify-supervisor DEV_VERSION=1.0.1-rc1 \
  SUPERVISOR_NS=my-namespace \
  AWX_URL=https://awx.example.com \
  AWX_TOKEN=... \
  AWX_TEMPLATE="Configure Webserver" \
  SSH_PUBLIC_KEY_FILE=~/.ssh/awx_ansible.pub
```

`AWX_TEMPLATE` must have Prompt on Launch enabled for Limit and must have an inventory. It is checked in a preflight pass before anything is created, so a misconfigured run fails in seconds rather than after an install.

**The VM it runs against is part of the harness.** With no `VM_LABEL`, `verify-supervisor` creates its own fixture VM and destroys it afterwards, discovering the image, class and storage class from the namespace. It needs a public key whose private half AWX already holds in the Machine credential on the template:

```bash
export SSH_PUBLIC_KEY_FILE=~/.ssh/awx_ansible.pub
```

For a lab you own, `MANAGE_AWX_CREDENTIAL=1` instead generates a fresh keypair per run and writes the private half into that credential. It is destructive - the credential's previous private key is overwritten and cannot be read back first, so anything else authenticating with it breaks. `AWX_CREDENTIAL_ID` picks the credential explicitly when the template has more than one.

Set `VM_LABEL` to point at a VM you manage yourself and the harness leaves fixtures alone entirely. `make test-fixture-up` / `make test-fixture-down` manage one by hand, which is worth doing when iterating: reusing a fixture across several verify runs saves the boot each time.

An IP is not a running sshd - vm-operator reports the address while cloud-init is still writing `authorized_keys` - so the fixture waits `FIXTURE_SETTLE_SECONDS` (default 90) after the IP appears before handing over.

What it asserts, in order:

1. **The build under test is what is installed.** Exactly one controller Deployment, Available, running the digest `dev-release` just pushed - so a run cannot silently validate last week's install. The image must be pinned by digest, not a tag: a tag sends kapp-controller back to the registry on every deploy, which unpins it and breaks air-gapped installs.
2. **It starts clean in-cluster.** CRDs Established, `controller started successfully` in the log, and no `Forbidden` anywhere in it - a missing `ClusterRole` rule shows up here and nowhere else.
3. **A real run completes.** `AWXConnection` goes Ready with the API base path the harness independently detected, every matched VM reaches `Succeeded`, the binding's fan-out matches the selector, and the inventory host is read back *out of AWX* to confirm its `ansible_host` is the IP the live VM reports.
4. **An idle binding costs nothing.** `metadata.resourceVersion` and the AWX host's `modified` timestamp must both be unchanged across three or more resync passes. This is the assertion that would have caught the per-pass status write fixed in 1.0.1, and it is cheap to keep honest. A *child* is not quite as quiet: it records `status.lastHostCheck` each time it reconciles its inventory host against AWX, so an idle VM costs one status write per `host_check_period` (600s by default) and nothing in between. That timestamp is in status rather than in memory on purpose - the decision has to survive a controller restart and be derivable from the object rather than from what the process remembers doing.
5. **Teardown removes what it created.** The finalizer releases, a host the controller created is gone from AWX, and a host it merely adopted is still there.

It always removes its own `AnsibleBinding`, `AWXConnection` and `Secret`, including on failure - it runs in a real tenant namespace, and a leaked binding keeps launching jobs. `--keep` (via `test/verify-supervisor.sh --keep`) leaves them for debugging, and any failure dumps the controller log and the binding's status first.

## Releasing

Validate first ([above](#pre-release-validation)), then push a `v*` tag and GitHub Actions does the rest.

```bash
git tag v1.0.0
git push origin v1.0.0
```

The pipeline:

1. Builds and pushes the controller image to `ghcr.io/warroyo/ansible-supervisor-service/controller:<version>`
2. Runs `kctrl package release` to build the Carvel package
3. Assembles `ansible-supervisor.yml` from the generated package metadata and spec
4. Creates a GitHub Release with `ansible-supervisor.yml` attached and auto-generated release notes

`ansible-supervisor.yml` is what you upload as a supervisor service. Release steps only run on tag pushes; `workflow_dispatch` builds but doesn't push or publish.

**Local release:**

```bash
export VERSION=1.0.0
make build-controller
make release-controller
make release
```

The controller image is built from `controller/Dockerfile`, whose base image has to satisfy the `go` directive in `controller/go.mod`. Bumping one without the other fails the build at `go mod download`.
