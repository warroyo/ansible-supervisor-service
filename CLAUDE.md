# Working in this repo

## "Cut a release" means the live gate, not just CI

`make test-unit` and `make test-e2e` are the whole of what GitHub Actions
runs, and they are *not* sufficient to call a build safe. They run the
controller as a host binary against a fake AWX and a stand-in VM CRD, so
they never touch the Dockerfile, the Carvel package, the pinned digest or
the real `ClusterRole`.

Any request to cut a dev release, run the harness, or check a build is
safe to ship means the full three-step loop in
[CONTRIBUTING.md § Pre-release validation](CONTRIBUTING.md#pre-release-validation):

1. `make dev-release` - builds, pushes the image, assembles the package
2. `make install-supervisor-service` - registers and
   installs it on the Supervisor through the vCenter REST API. There is
   no `kubectl apply` fallback; Supervisor RBAC refuses even a vSphere
   administrator the right to create the CRDs and ClusterRoles
3. `make verify-supervisor` - the assertions that
   only a live environment can make, including that the digest under test
   is the one actually running

Stopping after step 1 validates nothing about the release.

All three steps read their settings from `.env` at the repo root
(gitignored; `.env.example` is the committed template). Check whether one
exists before asking for values - and if one does not, `.env.example`
lists exactly what is missing. `DEV_VERSION` lives there too, which is
why the commands above take no arguments.

## The lab this is validated against

The Supervisor is the `sup-gpu` kubecontext. AWX runs on the dev
workload cluster and is reached at the URL on the `lab-awx`
`AWXConnection` in the `dev-1-y8qw4` namespace, whose `awx-token` Secret
holds the API token. Steps 2 and 3 need vCenter credentials and an AWX
job template that has Prompt on Launch enabled for Limit - ask for them
rather than guessing.

`verify-supervisor` creates and destroys its own fixture VM unless
`VM_LABEL` names one to reuse. `MANAGE_AWX_CREDENTIAL=1` is destructive:
it overwrites the private key on the AWX Machine credential, which
breaks anything else authenticating with it. Never set it unprompted.
