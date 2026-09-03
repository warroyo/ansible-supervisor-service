# Contributing

## Layout

| Path | What's in it |
|---|---|
| `controller/` | the Go controller - one file per concern (`awxconnection.go`, `ansiblebinding.go`, `ansiblerun.go`, `varsfrom.go`, `awx_client.go`, `engine.go`) |
| `controller/manifests/crd.yml` | all three CRDs; copied into `config/` at release time |
| `config/` | ytt templates for the deployed service: `Deployment`, `ServiceAccount`, RBAC, values schema |
| `examples/` | the manifests a user applies |
| `test/` | the e2e suite and its fake AWX server |

## Testing

You don't need a supervisor or a real AWX/Tower instance. The controller's output is plain Kubernetes objects and an AWX API client, so the whole suite runs against a local [kind](https://kind.sigs.k8s.io/) cluster and a fake AWX server (`test/fakeawx`).

```bash
make test-unit   # go vet + unit tests
make test-e2e    # full kind-based suite (requires kind, kubectl, ytt, go)
```

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

`test/fixtures/vm-crd.yml` is a simplified stand-in rather than the real `vmoperator.vmware.com` CRD, and `test/fakeawx` stands in for a real AWX. Both leave gaps that only a live environment closes, so these were checked by hand against a real supervisor and a real AWX:

- `status.network.primaryIP4` read through `v1alpha2` against a live VM whose CRD stores `v1alpha5`
- a real AWX token and Machine credential SSHing into a VM Service VM, with `--limit` honored
- the in-cluster startup path under the service's own `ClusterRole`
- one binding fanning out to several VMs, each with its own inventory host and run

Re-check these by hand after you change VM lookup, the AWX client, or RBAC.

## Releasing

Push a `v*` tag and GitHub Actions does the rest.

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
