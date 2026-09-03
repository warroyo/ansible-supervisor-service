#!/usr/bin/env bash
# End-to-end suite: spins up a local kind cluster, applies this service's
# real CRDs and RBAC (rendered through the real config/deploy.yml via
# ytt, exactly as it would be installed), runs the controller as its own
# constrained service account (so a missing RBAC rule fails the suite
# with Forbidden, same as it would in a real deployment), and points it
# at a fake AWX server (test/fakeawx) instead of a real AWX/Tower
# instance. No Supervisor and no real AWX needed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
CLUSTER_NAME="ansible-supervisor-e2e"
SYSTEM_NS="ansible-supervisor-system"
TEST_NS="test-ns"
AWX_ADDR="127.0.0.1:8756"
AAP_ADDR="127.0.0.1:8757"   # second instance serving the AAP 2.5+ gateway API root
NOFILTER_ADDR="127.0.0.1:8758"  # third instance that ignores the ?name= host filter
WORK_DIR="$(mktemp -d)"
KEEP=0

for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
  esac
done

CONTROLLER_PID=""
FAKEAWX_PID=""
FAKEAAP_PID=""
FAKENOFILTER_PID=""

cleanup() {
  status=$?
  if [[ $status -ne 0 ]]; then
    echo "=== FAILURE: dumping logs ==="
    echo "--- controller log ---"
    tail -n 200 "$WORK_DIR/controller.log" 2>/dev/null || true
    echo "--- fakeawx log ---"
    tail -n 200 "$WORK_DIR/fakeawx.log" 2>/dev/null || true
    echo "--- fakeaap log ---"
    tail -n 200 "$WORK_DIR/fakeaap.log" 2>/dev/null || true
    echo "--- fakenofilter log ---"
    tail -n 200 "$WORK_DIR/fakenofilter.log" 2>/dev/null || true
  fi

  [[ -n "$CONTROLLER_PID" ]] && kill "$CONTROLLER_PID" 2>/dev/null || true
  [[ -n "$FAKEAWX_PID" ]] && kill "$FAKEAWX_PID" 2>/dev/null || true
  [[ -n "$FAKEAAP_PID" ]] && kill "$FAKEAAP_PID" 2>/dev/null || true
  [[ -n "$FAKENOFILTER_PID" ]] && kill "$FAKENOFILTER_PID" 2>/dev/null || true

  if [[ $KEEP -eq 1 ]]; then
    echo "--keep set: leaving kind cluster '$CLUSTER_NAME' and $WORK_DIR up"
  else
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
    rm -rf "$WORK_DIR"
  fi
  exit $status
}
trap cleanup EXIT

log() { echo "[e2e] $*"; }

# A stale fakeawx from an interrupted run would serve the previous
# fixture set and produce baffling failures - fail loudly instead.
if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q "${AWX_ADDR}"; then
  echo "something is already listening on ${AWX_ADDR} (stale fakeawx from an earlier run?)"
  exit 1
fi

wait_for() {
  local desc="$1"; shift
  local timeout="$1"; shift
  local waited=0
  until "$@" >/dev/null 2>&1; do
    sleep 1
    waited=$((waited + 1))
    if [[ $waited -ge $timeout ]]; then
      echo "timed out waiting for: $desc"
      return 1
    fi
  done
}

# The fake AWX returns hosts as a single-line JSON array, so grepping it
# happily matches across object boundaries. Parse it instead.
host_deleted() {  # host_deleted <addr> <host id> -> true if AWX deleted it
  curl -sf "http://$1/_test/deleted-hosts" \
    | python3 -c "import json,sys; sys.exit(0 if int(sys.argv[1]) in json.load(sys.stdin) else 1)" "$2"
}

# Deleted hosts keep their name in the fake's store, so a host that was
# deleted and recreated appears twice - and the store is a map, so which
# one comes back first is luck. Only the live one is the answer.
host_field() {    # host_field <addr> <host name> <field> -> prints the value
  curl -sf "http://$1/_test/hosts" \
    | python3 -c "
import json, sys
name, field = sys.argv[1], sys.argv[2]
for h in json.load(sys.stdin):
    if h.get('name') == name and not h.get('deleted'):
        print(h.get(field, ''))
        break
else:
    print('')
" "$2" "$3"
}

# Counting "launched job" log lines proves a run happened. Only the launch
# body proves it ran against what the CR asked for - that a targetless run
# sent no limit at all, or that varsFrom reached extra_vars.
launch_body() {   # launch_body <addr> <job id> -> prints the launch body as JSON
  curl -sf "http://$1/_test/launches" \
    | python3 -c "
import json, sys
job = int(sys.argv[1])
for l in json.load(sys.stdin):
    if l['job'] == job:
        print(json.dumps(l['body']))
        break
else:
    print('{}')
" "$2"
}

launch_limit() {  # launch_limit <addr> <job id> -> prints the limit, empty if none was sent
  launch_body "$1" "$2" | python3 -c "import json,sys; print(json.load(sys.stdin).get('limit',''))"
}

launch_var() {    # launch_var <addr> <job id> <name> -> prints one extra var
  launch_body "$1" "$2" | python3 -c "
import json, sys
body = json.load(sys.stdin)
print(json.loads(body.get('extra_vars') or '{}').get(sys.argv[1], ''))
" "$3"
}

launch_count() { grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true; }

log "creating kind cluster $CLUSTER_NAME"
kind create cluster --name "$CLUSTER_NAME" --kubeconfig "$WORK_DIR/admin.kubeconfig" >/dev/null
export KUBECONFIG="$WORK_DIR/admin.kubeconfig"

log "installing CRDs"
kubectl apply -f "$ROOT_DIR/controller/manifests/crd.yml" >/dev/null
kubectl apply -f "$ROOT_DIR/test/fixtures/vm-crd.yml" >/dev/null
kubectl wait --for=condition=Established --timeout=30s \
  crd/awxconnections.field.vmware.com \
  crd/ansiblebindings.field.vmware.com \
  crd/ansibleruns.field.vmware.com \
  crd/virtualmachines.vmoperator.vmware.com >/dev/null

kubectl create namespace "$SYSTEM_NS" >/dev/null
kubectl create namespace "$TEST_NS" >/dev/null

log "rendering and applying RBAC/deployment manifests (real config/deploy.yml, via ytt)"
# Only deploy.yml + values.yml: config-release.yml is a kbld Config doc,
# consumed by the real `ytt | kbld | kapp` pipeline to resolve the
# "controller" image reference. This suite runs the controller via
# `go run` on the host instead (see below) so it never needs a built
# image or kbld; the Deployment object still gets applied for
# completeness, it just won't have a real image to pull.
ytt -f "$ROOT_DIR/config/deploy.yml" -f "$ROOT_DIR/config/values.yml" \
  --data-value namespace="$SYSTEM_NS" \
  --data-value resync_period="2" \
  | kubectl apply -f - >/dev/null

log "minting a token for the controller's own service account (RBAC gaps must surface as Forbidden, not be silently skipped)"
TOKEN=$(kubectl create token ansible-supervisor -n "$SYSTEM_NS" --duration=1h)
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
kubectl config view --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$WORK_DIR/ca.crt"

cat > "$WORK_DIR/sa.kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: kind
  cluster:
    server: ${SERVER}
    certificate-authority: ${WORK_DIR}/ca.crt
users:
- name: ansible-supervisor
  user:
    token: ${TOKEN}
contexts:
- name: ansible-supervisor
  context:
    cluster: kind
    user: ansible-supervisor
    namespace: ${TEST_NS}
current-context: ansible-supervisor
EOF

log "starting fakeawx"
# Build and run the real binaries rather than `go run`: `go run` execs a
# child, so the PID we'd capture is the wrapper's and killing it would
# leave the server holding its port for the next run.
( cd "$ROOT_DIR/test/fakeawx" && go build -o "$WORK_DIR/fakeawx" . )
( cd "$ROOT_DIR/controller" && go build -o "$WORK_DIR/controller" . )

"$WORK_DIR/fakeawx" --addr="$AWX_ADDR" > "$WORK_DIR/fakeawx.log" 2>&1 &
FAKEAWX_PID=$!
wait_for "fakeawx listening" 15 curl -sf "http://${AWX_ADDR}/api/v2/me/"

# A second instance serving only the AAP 2.5+ gateway API root, to prove
# base-path detection rather than assuming /api/v2 everywhere.
"$WORK_DIR/fakeawx" --addr="$AAP_ADDR" --api-base-path=/api/controller/v2 > "$WORK_DIR/fakeaap.log" 2>&1 &
FAKEAAP_PID=$!
wait_for "fakeaap listening" 15 curl -sf "http://${AAP_ADDR}/api/controller/v2/me/"

# A third instance that ignores ?name= on host lookups: the parameter is
# not in the published API schema, so the controller must not assume the
# first result is the host it asked for.
"$WORK_DIR/fakeawx" --addr="$NOFILTER_ADDR" --ignore-name-filter > "$WORK_DIR/fakenofilter.log" 2>&1 &
FAKENOFILTER_PID=$!
wait_for "fakenofilter listening" 15 curl -sf "http://${NOFILTER_ADDR}/api/v2/me/"

log "starting controller as the ansible-supervisor service account"
KUBECONFIG="$WORK_DIR/sa.kubeconfig" "$WORK_DIR/controller" --resync-period=2 > "$WORK_DIR/controller.log" 2>&1 &
CONTROLLER_PID=$!
wait_for "controller started" 30 grep -q "controller started successfully" "$WORK_DIR/controller.log"

log "applying AWXConnection + Secret"
kubectl -n "$TEST_NS" create secret generic awx-token --from-literal=token=fake-token >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: e2e-awx
  namespace: ${TEST_NS}
spec:
  url: "http://${AWX_ADDR}"
  secretRef: "awx-token"
EOF

wait_for "AWXConnection Ready" 30 bash -c \
  "[[ \$(kubectl get awxconnection e2e-awx -n ${TEST_NS} -o jsonpath='{.status.ready}') == true ]]"
log "AWXConnection is Ready"

log "creating a fake VM Service VirtualMachine"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-1
  namespace: ${TEST_NS}
  labels:
    app: webserver
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.5"
EOF

log "applying AnsibleBinding"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-config
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: webserver
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  extraVars:
    environment: e2e
EOF

wait_for "AnsibleBinding Ready" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-config -n ${TEST_NS} -o jsonpath='{.status.ready}') == true ]]"

wait_for "VM job reaches Succeeded" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-config -n ${TEST_NS} -o jsonpath='{.status.vms[0].phase}') == Succeeded ]]"

HOST_ID=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.vms[0].awxHostID}')
JOB_ID=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.vms[0].lastJobID}')
if [[ -z "$HOST_ID" || -z "$JOB_ID" ]]; then
  echo "expected awxHostID and lastJobID to be set, got hostID=$HOST_ID jobID=$JOB_ID"
  exit 1
fi
log "VM run succeeded: awxHostID=$HOST_ID lastJobID=$JOB_ID"

# --- a re-run request must actually launch a new run ---
log "bumping the reconcile-requested-at annotation, expecting a fresh run"
kubectl annotate ansiblebinding e2e-config -n "$TEST_NS" \
  ansible.field.vmware.com/reconcile-requested-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite >/dev/null

wait_for "a new job is launched" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-config -n ${TEST_NS} -o jsonpath='{.status.vms[0].lastJobID}') != ${JOB_ID} ]]"
wait_for "the re-run reaches Succeeded" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-config -n ${TEST_NS} -o jsonpath='{.status.vms[0].phase}') == Succeeded ]]"
RERUN_JOB_ID=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.vms[0].lastJobID}')
log "re-run launched and succeeded: lastJobID=$RERUN_JOB_ID"

# --- powering a VM off must not clobber its run phase, and must not
#     swallow a re-run requested while it was down ---
log "powering the VM off, expecting its completed phase to survive"
kubectl patch virtualmachine web-1 -n "$TEST_NS" --type=merge \
  -p '{"status":{"powerState":"PoweredOff"}}' >/dev/null
sleep 6   # several resync passes

PHASE_WHILE_OFF=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.vms[0].phase}')
JOB_WHILE_OFF=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.vms[0].lastJobID}')
if [[ "$PHASE_WHILE_OFF" != "Succeeded" ]]; then
  echo "expected a powered-off VM to keep its Succeeded phase, got '$PHASE_WHILE_OFF'"
  exit 1
fi
log "phase survived power-off: $PHASE_WHILE_OFF"

log "requesting a re-run while the VM is off, expecting it to be honored once it returns"
kubectl annotate ansiblebinding e2e-config -n "$TEST_NS" \
  ansible.field.vmware.com/reconcile-requested-at="offline-$(date -u +%s)" --overwrite >/dev/null
sleep 6

if [[ "$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.vms[0].lastJobID}')" != "$JOB_WHILE_OFF" ]]; then
  echo "a job was launched against a powered-off VM"
  exit 1
fi

kubectl patch virtualmachine web-1 -n "$TEST_NS" --type=merge \
  -p '{"status":{"powerState":"PoweredOn"}}' >/dev/null
wait_for "the deferred re-run launches once the VM is back" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-config -n ${TEST_NS} -o jsonpath='{.status.vms[0].lastJobID}') != ${JOB_WHILE_OFF} ]]"
log "re-run requested during downtime was honored, not swallowed"

# --- steady state must not re-PATCH an unchanged host every resync ---
# The controller re-reads the host from AWX on every pass (see the drift
# check below), so what keeps a steady state quiet is the write being
# conditional on the variables actually differing - not the read.
PATCH_COUNT=$(grep -c "fakeawx: patched host" "$WORK_DIR/fakeawx.log" || true)
if [[ "$PATCH_COUNT" != "0" ]]; then
  echo "expected 0 host PATCHes for an unchanged host across many resyncs, got $PATCH_COUNT"
  exit 1
fi
log "unchanged host was never re-PATCHed across resyncs"

# --- a host deleted out of band must be recreated, not trusted ---
# Deleting the inventory host in AWX is drift like any other. Status
# alone cannot see it, and every later run would fail with "--limit does
# not match any hosts", forever, with nothing to repair it.
log "deleting the AWX host out of band, expecting the next reconcile to recreate it"
curl -sf -X DELETE "http://${AWX_ADDR}/api/v2/hosts/${HOST_ID}/" >/dev/null
wait_for "AWX host recreated under a new id" 30 bash -c \
  "id=\$(kubectl get ansiblebinding e2e-config -n ${TEST_NS} -o jsonpath='{.status.vms[0].awxHostID}'); [[ -n \$id && \$id != ${HOST_ID} ]]"
HOST_ID=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.vms[0].awxHostID}')
if [[ "$(host_field "$AWX_ADDR" web-1 name)" != "web-1" ]]; then
  echo "expected the recreated host to be back in the inventory"
  exit 1
fi
log "out-of-band host deletion was repaired (new id=$HOST_ID)"

# --- host variables edited in AWX must be put back ---
log "editing the host's variables in AWX, expecting the controller to repair them"
curl -sf -X PATCH -H 'Content-Type: application/json' \
  -d '{"variables":"{\"ansible_host\": \"10.99.99.99\"}"}' \
  "http://${AWX_ADDR}/api/v2/hosts/${HOST_ID}/" >/dev/null
wait_for "ansible_host restored to the VM's real IP" 30 bash -c \
  "[[ \$(host_field ${AWX_ADDR} web-1 variables) != *10.99.99.99* ]]"
log "hand-edited host variables were reconciled back"

log "dropping the VM out of vmSelector, expecting its AWX host to be cleaned up"
kubectl label virtualmachine web-1 -n "$TEST_NS" app- >/dev/null

wait_for "VM removed from status.vms" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-config -n ${TEST_NS} -o jsonpath='{.status.vms}') == '[]' ]]"

wait_for "AWX host deleted" 15 host_deleted "$AWX_ADDR" "$HOST_ID"
log "unmatched VM's AWX host was cleaned up (id=$HOST_ID)"

log "deleting AnsibleBinding, expecting the finalizer to let it disappear"
kubectl delete ansiblebinding e2e-config -n "$TEST_NS" --timeout=30s >/dev/null
log "AnsibleBinding deleted cleanly"

# --- a template without Prompt on Launch must be refused, not launched ---
# AWX silently drops a limit the template won't accept and runs the
# playbook against its whole inventory, so the controller must not launch.
log "checking a template without ask_limit_on_launch is refused instead of launched"
JOBS_BEFORE=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)

kubectl label virtualmachine web-1 -n "$TEST_NS" app=webserver >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-noprompt
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: webserver
  awxConnectionRef: e2e-awx
  template:
    name: "No Prompt Template"
    type: JobTemplate
EOF

wait_for "no-prompt config reports Failed" 30 bash -c \
  "kubectl get ansiblebinding e2e-noprompt -n ${TEST_NS} -o jsonpath='{.status.message}' | grep -q ask_limit_on_launch"

JOBS_AFTER=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)
if [[ "$JOBS_BEFORE" != "$JOBS_AFTER" ]]; then
  echo "expected NO job to be launched for a template without ask_limit_on_launch, but launch count went $JOBS_BEFORE -> $JOBS_AFTER"
  exit 1
fi
log "refused to launch, and no job was started ($JOBS_AFTER launches total, unchanged)"
kubectl delete ansiblebinding e2e-noprompt -n "$TEST_NS" --timeout=30s >/dev/null

# --- an empty vmSelector must be rejected outright ---
log "checking an empty vmSelector is rejected by the CRD schema"
if cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-emptyselector
  namespace: ${TEST_NS}
spec:
  vmSelector: {}
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF
then
  echo "expected an empty vmSelector to be rejected, but it was accepted"
  exit 1
fi
log "empty vmSelector rejected"

# --- a pre-existing AWX host must be adopted, not clobbered or deleted ---
log "seeding a pre-existing AWX host, expecting adoption (vars preserved, never deleted)"
SEEDED_ID=$(curl -sf -X POST -H 'Content-Type: application/json' \
  -d '{"inventory":1,"name":"web-2","variables":"{\"custom\":\"keepme\"}"}' \
  "http://${AWX_ADDR}/_test/hosts" | grep -o '"id":[0-9]*' | cut -d: -f2)

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-2
  namespace: ${TEST_NS}
  labels:
    app: adopted
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.6"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-adopt
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: adopted
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "adopted VM reaches a run" 30 bash -c \
  "[[ -n \$(kubectl get ansiblebinding e2e-adopt -n ${TEST_NS} -o jsonpath='{.status.vms[0].lastJobID}') ]]"

ADOPTED_ID=$(kubectl get ansiblebinding e2e-adopt -n "$TEST_NS" -o jsonpath='{.status.vms[0].awxHostID}')
CREATED_FLAG=$(kubectl get ansiblebinding e2e-adopt -n "$TEST_NS" -o jsonpath='{.status.vms[0].awxHostCreated}')
if [[ "$ADOPTED_ID" != "$SEEDED_ID" ]]; then
  echo "expected the pre-existing host $SEEDED_ID to be adopted, got $ADOPTED_ID"
  exit 1
fi
if [[ "$CREATED_FLAG" == "true" ]]; then
  echo "expected awxHostCreated=false for an adopted host"
  exit 1
fi
if ! curl -sf "http://${AWX_ADDR}/_test/hosts" | grep -q 'keepme'; then
  echo "adopting a pre-existing host wiped its existing variables"
  exit 1
fi
log "pre-existing host adopted: id=$SEEDED_ID, awxHostCreated=${CREATED_FLAG:-false}, existing vars preserved"

kubectl delete ansiblebinding e2e-adopt -n "$TEST_NS" --timeout=30s >/dev/null
if host_deleted "$AWX_ADDR" "$SEEDED_ID"; then
  echo "cleanup deleted AWX host $SEEDED_ID, which this controller did not create"
  exit 1
fi
log "adopted host survived cleanup, as it must"

# --- a host owned by a DIFFERENT supervisor must be refused, not stolen ---
# One AWX shared by several supervisors: host names are unique per
# inventory, so without an ownership check the second supervisor would
# silently repoint the first one's host at its own VM.
log "seeding a host owned by another supervisor, expecting refusal rather than takeover"
FOREIGN_ID=$(curl -sf -X POST -H 'Content-Type: application/json' \
  -d '{"inventory":1,"name":"web-3","description":"ansible-supervisor:other-supervisor:other-ns/other-config","variables":"{\"ansible_host\":\"192.168.99.99\"}"}' \
  "http://${AWX_ADDR}/_test/hosts" | grep -o '"id":[0-9]*' | cut -d: -f2)

JOBS_BEFORE_FOREIGN=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-3
  namespace: ${TEST_NS}
  labels:
    app: foreign
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.7"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-foreign
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: foreign
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "foreign-owned host is refused" 30 bash -c \
  "kubectl get ansiblebinding e2e-foreign -n ${TEST_NS} -o jsonpath='{.status.vms[0].phase}{.status.message}' | grep -q 'owned by another'"

if ! curl -sf "http://${AWX_ADDR}/_test/hosts" | grep -q '192.168.99.99'; then
  echo "the other supervisor's host variables were overwritten"
  exit 1
fi
JOBS_AFTER_FOREIGN=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)
if [[ "$JOBS_BEFORE_FOREIGN" != "$JOBS_AFTER_FOREIGN" ]]; then
  echo "expected no job against a foreign-owned host, launches went $JOBS_BEFORE_FOREIGN -> $JOBS_AFTER_FOREIGN"
  exit 1
fi
log "refused: host $FOREIGN_ID untouched, no job launched"

log "giving the binding its own hostNamePrefix, expecting it to stop colliding"
kubectl patch awxconnection e2e-awx -n "$TEST_NS" --type=merge \
  -p '{"spec":{"hostNamePrefix":"sup-b-"}}' >/dev/null

wait_for "prefixed host is created and the run succeeds" 60 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-foreign -n ${TEST_NS} -o jsonpath='{.status.vms[0].awxHostName}') == sup-b-web-3 ]]"
if ! curl -sf "http://${AWX_ADDR}/_test/hosts" | grep -q 'sup-b-web-3'; then
  echo "expected a host named sup-b-web-3 to be created"
  exit 1
fi
log "prefix resolved the collision: host sup-b-web-3 created alongside the other supervisor's web-3"

kubectl delete ansiblebinding e2e-foreign -n "$TEST_NS" --timeout=30s >/dev/null
if host_deleted "$AWX_ADDR" "$FOREIGN_ID"; then
  echo "cleanup deleted host $FOREIGN_ID, which belongs to another supervisor"
  exit 1
fi
kubectl patch awxconnection e2e-awx -n "$TEST_NS" --type=merge -p '{"spec":{"hostNamePrefix":""}}' >/dev/null
log "the other supervisor's host survived cleanup"

# --- Retain, delete, recreate: ownership must be reclaimed ---
# Ownership lives in the AWX host description, not just CR status, so a
# recreated binding recognises the host it left behind instead of
# permanently downgrading it to "adopted, never deletable".
log "running with cleanupPolicy: Retain, then deleting the binding"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-4
  namespace: ${TEST_NS}
  labels:
    app: retained
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.8"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-retain
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: retained
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  cleanupPolicy: Retain
EOF

wait_for "retained binding creates its host" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-retain -n ${TEST_NS} -o jsonpath='{.status.vms[0].awxHostCreated}') == true ]]"
RETAINED_ID=$(kubectl get ansiblebinding e2e-retain -n "$TEST_NS" -o jsonpath='{.status.vms[0].awxHostID}')

kubectl delete ansiblebinding e2e-retain -n "$TEST_NS" --timeout=30s >/dev/null
if host_deleted "$AWX_ADDR" "$RETAINED_ID"; then
  echo "cleanupPolicy: Retain still deleted host $RETAINED_ID"
  exit 1
fi
log "host $RETAINED_ID retained after the binding was deleted"

log "recreating the same binding, expecting it to reclaim ownership of the retained host"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-retain
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: retained
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "recreated binding adopts the same host" 30 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-retain -n ${TEST_NS} -o jsonpath='{.status.vms[0].awxHostID}') == ${RETAINED_ID} ]]"

RECLAIMED=$(kubectl get ansiblebinding e2e-retain -n "$TEST_NS" -o jsonpath='{.status.vms[0].awxHostCreated}')
if [[ "$RECLAIMED" != "true" ]]; then
  echo "expected the recreated binding to reclaim ownership (awxHostCreated=true), got '$RECLAIMED'"
  exit 1
fi
log "ownership reclaimed via the AWX-side marker: same host $RETAINED_ID, awxHostCreated=true"

kubectl delete ansiblebinding e2e-retain -n "$TEST_NS" --timeout=30s >/dev/null
wait_for "reclaimed host is now deletable on cleanup" 30 host_deleted "$AWX_ADDR" "$RETAINED_ID"
log "reclaimed host was cleaned up, no longer orphaned"

# --- an AWXConnection finalizer left by an older controller must be stripped ---
# AWXConnection creates nothing outside Kubernetes, so it no longer
# carries a finalizer. One left behind by an upgrade would otherwise hang
# the resource in Terminating with nothing to release it.
log "adding the legacy AWXConnection finalizer by hand, expecting the controller to strip it"
kubectl patch awxconnection e2e-awx -n "$TEST_NS" --type=merge \
  -p '{"metadata":{"finalizers":["field.vmware.com/awx-connection-cleanup"]}}' >/dev/null
wait_for "legacy finalizer stripped" 30 bash -c \
  "[[ -z \$(kubectl get awxconnection e2e-awx -n ${TEST_NS} -o jsonpath='{.metadata.finalizers}' | tr -d '[]') ]]"
log "legacy AWXConnection finalizer was removed"

# --- AAP 2.5+ moved the controller API; detection must find it ---
# AWX/Tower/AAP<=2.4 serve /api/v2, AAP 2.5+ serve /api/controller/v2.
# Aria's own integration breaks on this exact change (Broadcom KB 394498).
log "checking the API base path was detected for the AWX-flavored instance"
DETECTED_AWX=$(kubectl get awxconnection e2e-awx -n "$TEST_NS" -o jsonpath='{.status.apiBasePath}')
if [[ "$DETECTED_AWX" != "/api/v2" ]]; then
  echo "expected /api/v2 to be detected for the AWX-flavored instance, got '$DETECTED_AWX'"
  exit 1
fi
log "detected $DETECTED_AWX"

log "pointing a connection at an AAP 2.5-style gateway, expecting /api/controller/v2 to be detected"
kubectl -n "$TEST_NS" create secret generic aap-token --from-literal=token=fake-token >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: e2e-aap
  namespace: ${TEST_NS}
spec:
  url: "http://${AAP_ADDR}"
  secretRef: "aap-token"
EOF

wait_for "AAP connection becomes Ready" 30 bash -c \
  "[[ \$(kubectl get awxconnection e2e-aap -n ${TEST_NS} -o jsonpath='{.status.ready}') == true ]]"

DETECTED_AAP=$(kubectl get awxconnection e2e-aap -n "$TEST_NS" -o jsonpath='{.status.apiBasePath}')
if [[ "$DETECTED_AAP" != "/api/controller/v2" ]]; then
  echo "expected /api/controller/v2 to be detected for the AAP-flavored instance, got '$DETECTED_AAP'"
  exit 1
fi
log "detected $DETECTED_AAP"

log "running a full binding through the AAP-flavored instance"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-5
  namespace: ${TEST_NS}
  labels:
    app: viagateway
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.9"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-gateway
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: viagateway
  awxConnectionRef: e2e-aap
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "the run through the gateway API succeeds" 60 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-gateway -n ${TEST_NS} -o jsonpath='{.status.vms[0].phase}') == Succeeded ]]"
if ! grep -q "fakeawx: launched job" "$WORK_DIR/fakeaap.log"; then
  echo "expected the job to be launched against the AAP-flavored instance"
  exit 1
fi
log "full launch/poll cycle works through /api/controller/v2"

kubectl delete ansiblebinding e2e-gateway -n "$TEST_NS" --timeout=30s >/dev/null

# --- an instance that ignores ?name= must not cause the wrong host to be used ---
log "pointing a binding at an instance that ignores the ?name= host filter"
UNRELATED_ID=$(curl -sf -X POST -H 'Content-Type: application/json' \
  -d '{"inventory":1,"name":"totally-unrelated","variables":"{\"ansible_host\":\"172.16.0.1\",\"owner\":\"someone-else\"}"}' \
  "http://${NOFILTER_ADDR}/_test/hosts" | grep -o '"id":[0-9]*' | cut -d: -f2)

kubectl -n "$TEST_NS" create secret generic nofilter-token --from-literal=token=fake-token >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: e2e-nofilter
  namespace: ${TEST_NS}
spec:
  url: "http://${NOFILTER_ADDR}"
  secretRef: "nofilter-token"
---
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-6
  namespace: ${TEST_NS}
  labels:
    app: nofilter
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.10"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-nofilter
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: nofilter
  awxConnectionRef: e2e-nofilter
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "the run completes against the non-filtering instance" 60 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-nofilter -n ${TEST_NS} -o jsonpath='{.status.vms[0].phase}') == Succeeded ]]"

NOFILTER_HOSTNAME=$(kubectl get ansiblebinding e2e-nofilter -n "$TEST_NS" -o jsonpath='{.status.vms[0].awxHostName}')
if [[ "$NOFILTER_HOSTNAME" != "web-6" ]]; then
  echo "expected the binding to use host web-6, got '$NOFILTER_HOSTNAME'"
  exit 1
fi
UNRELATED_VARS=$(host_field "$NOFILTER_ADDR" "totally-unrelated" variables)
if [[ "$UNRELATED_VARS" != *someone-else* || "$UNRELATED_VARS" == *10.0.0.10* ]]; then
  echo "the unrelated host's variables were modified - the ?name= filter was trusted blindly: $UNRELATED_VARS"
  exit 1
fi
if [[ -z "$(host_field "$NOFILTER_ADDR" "web-6" id)" ]]; then
  echo "expected a host named web-6 to have been created"
  exit 1
fi
log "unrelated host untouched, own host web-6 created despite the unfiltered lookup"

kubectl delete ansiblebinding e2e-nofilter -n "$TEST_NS" --timeout=30s >/dev/null
if host_deleted "$NOFILTER_ADDR" "$UNRELATED_ID"; then
  echo "cleanup deleted the unrelated host $UNRELATED_ID"
  exit 1
fi
log "unrelated host survived cleanup"


# =====================================================================
# AnsibleRun: a single execution - one AWX job, launched once, terminal
# forever. Everything below is about that "once", and about the two
# independent axes: where a run points, and where its variables come from.
# =====================================================================

# --- standalone: no target at all means no inventory writes and no limit ---
# This is the shape a `hosts: localhost` playbook needs. The binding refuses
# to launch when it cannot scope a run; a run with no target deliberately
# does the opposite and accepts the template's own scope.
log "applying a standalone AnsibleRun (no hosts, no vmRef)"
HOSTS_BEFORE_STANDALONE=$(curl -sf "http://${AWX_ADDR}/_test/hosts" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))")
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-standalone
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  extraVars:
    summary: "standalone run"
EOF

wait_for "standalone run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-standalone -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"

STANDALONE_JOB=$(kubectl get ansiblerun e2e-standalone -n "$TEST_NS" -o jsonpath='{.status.jobID}')
if [[ -n "$(launch_limit "$AWX_ADDR" "$STANDALONE_JOB")" ]]; then
  echo "a run with no target sent a limit: $(launch_limit "$AWX_ADDR" "$STANDALONE_JOB")"
  exit 1
fi
if [[ "$(launch_var "$AWX_ADDR" "$STANDALONE_JOB" summary)" != "standalone run" ]]; then
  echo "extraVars did not reach the launch body"
  exit 1
fi
HOSTS_AFTER_STANDALONE=$(curl -sf "http://${AWX_ADDR}/_test/hosts" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))")
if [[ "$HOSTS_BEFORE_STANDALONE" != "$HOSTS_AFTER_STANDALONE" ]]; then
  echo "a run with no target touched the inventory ($HOSTS_BEFORE_STANDALONE -> $HOSTS_AFTER_STANDALONE hosts)"
  exit 1
fi
RUN_HOSTS=$(kubectl get ansiblerun e2e-standalone -n "$TEST_NS" -o jsonpath='{.status.hosts}')
if [[ -n "$RUN_HOSTS" ]]; then
  echo "expected no status.hosts on a targetless run, got: $RUN_HOSTS"
  exit 1
fi
log "standalone run: job $STANDALONE_JOB launched with no limit and no inventory host"

# --- a finished run is finished: nothing re-triggers it ---
# The re-run annotation is what an AnsibleBinding exists for. A run must
# ignore it, or "single execution" means nothing.
log "bumping the re-run annotation on a finished run, expecting no second job"
JOBS_BEFORE_RERUN=$(launch_count)
kubectl annotate ansiblerun e2e-standalone -n "$TEST_NS" \
  ansible.field.vmware.com/reconcile-requested-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite >/dev/null
sleep 6   # several resyncs at --resync-period=2
JOBS_AFTER_RERUN=$(launch_count)
if [[ "$JOBS_BEFORE_RERUN" != "$JOBS_AFTER_RERUN" ]]; then
  echo "a terminal run launched again ($JOBS_BEFORE_RERUN -> $JOBS_AFTER_RERUN)"
  exit 1
fi
log "terminal run stayed terminal, no second job"

# --- spec is immutable ---
log "editing a run's spec, expecting the API server to reject it"
if kubectl patch ansiblerun e2e-standalone -n "$TEST_NS" --type=merge \
     -p '{"spec":{"extraVars":{"summary":"changed"}}}' >/dev/null 2>&1; then
  echo "spec was editable; an AnsibleRun must be immutable"
  exit 1
fi
log "spec edit rejected"
kubectl delete ansiblerun e2e-standalone -n "$TEST_NS" --timeout=30s >/dev/null

# --- varsFrom off a ConfigMap: the pure external-API case ---
# A DNS/CMDB playbook runs on localhost and needs the record as variables,
# not as an inventory host. Reading them off a live object is the point.
log "applying a run that reads variables off a ConfigMap"
kubectl -n "$TEST_NS" create configmap dns-config --from-literal=zone=corp.example.com >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-varsfrom-cm
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  varsFrom:
    - resource:
        apiVersion: v1
        kind: ConfigMap
        name: dns-config
      vars:
        zone: "{.data.zone}"
EOF

wait_for "ConfigMap varsFrom run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-varsfrom-cm -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
CM_JOB=$(kubectl get ansiblerun e2e-varsfrom-cm -n "$TEST_NS" -o jsonpath='{.status.jobID}')
if [[ "$(launch_var "$AWX_ADDR" "$CM_JOB" zone)" != "corp.example.com" ]]; then
  echo "varsFrom value did not reach extra_vars: $(launch_body "$AWX_ADDR" "$CM_JOB")"
  exit 1
fi
if [[ -n "$(launch_limit "$AWX_ADDR" "$CM_JOB")" ]]; then
  echo "varsFrom must not imply a target, but a limit was sent"
  exit 1
fi
RESOLVED=$(kubectl get ansiblerun e2e-varsfrom-cm -n "$TEST_NS" -o jsonpath='{.status.resolvedVars[0]}')
if [[ "$RESOLVED" != "zone" ]]; then
  echo "expected status.resolvedVars to name 'zone', got '$RESOLVED'"
  exit 1
fi
log "varsFrom read the ConfigMap into extra_vars, inventory untouched"
kubectl delete ansiblerun e2e-varsfrom-cm -n "$TEST_NS" --timeout=30s >/dev/null

# --- varsFrom off a VirtualMachine: the DNS registration case ---
log "creating a VM for the run scenarios"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: run-vm
  namespace: ${TEST_NS}
  labels:
    app: runtarget
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.77"
EOF

log "applying a run that reads a VM's name and IP into variables"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-varsfrom-vm
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  extraVars:
    record_state: present
  varsFrom:
    - resource:
        apiVersion: vmoperator.vmware.com/v1alpha2
        kind: VirtualMachine
        name: run-vm
      vars:
        record_name: "{.metadata.name}"
        record_ip: "{.status.network.primaryIP4}"
EOF

wait_for "VM varsFrom run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-varsfrom-vm -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
VM_JOB=$(kubectl get ansiblerun e2e-varsfrom-vm -n "$TEST_NS" -o jsonpath='{.status.jobID}')
if [[ "$(launch_var "$AWX_ADDR" "$VM_JOB" record_name)" != "run-vm" \
   || "$(launch_var "$AWX_ADDR" "$VM_JOB" record_ip)" != "10.0.0.77" ]]; then
  echo "VM fields did not reach extra_vars: $(launch_body "$AWX_ADDR" "$VM_JOB")"
  exit 1
fi
if [[ -n "$(launch_limit "$AWX_ADDR" "$VM_JOB")" ]]; then
  echo "reading a VM's fields must not turn it into a target, but a limit was sent"
  exit 1
fi
log "VM name and IP arrived as variables, with no inventory host created for it"
kubectl delete ansiblerun e2e-varsfrom-vm -n "$TEST_NS" --timeout=30s >/dev/null

# --- varsFrom refusals: each terminal, each launching nothing ---
run_must_fail_without_launching() {  # <name> <manifest on stdin> <needle in message>
  local name="$1" needle="$2"
  local before after msg
  before=$(launch_count)
  kubectl apply -f - >/dev/null
  wait_for "$name reaches Failed" 60 bash -c \
    "[[ \$(kubectl get ansiblerun $name -n ${TEST_NS} -o jsonpath='{.status.state}') == Failed ]]"
  msg=$(kubectl get ansiblerun "$name" -n "$TEST_NS" -o jsonpath='{.status.message}')
  if [[ "$msg" != *"$needle"* ]]; then
    echo "$name: expected the message to mention '$needle', got: $msg"
    exit 1
  fi
  # A terminal failure must stamp finishedAt, or the TTL can never collect it.
  if [[ -z "$(kubectl get ansiblerun "$name" -n "$TEST_NS" -o jsonpath='{.status.finishedAt}')" ]]; then
    echo "$name: terminal failure did not set finishedAt"
    exit 1
  fi
  after=$(launch_count)
  if [[ "$before" != "$after" ]]; then
    echo "$name: refused but still launched a job ($before -> $after)"
    exit 1
  fi
  kubectl delete ansiblerun "$name" -n "$TEST_NS" --timeout=30s >/dev/null
}

log "checking varsFrom refuses to read a Secret"
run_must_fail_without_launching e2e-varsfrom-secret "Credential" <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-varsfrom-secret
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  varsFrom:
    - resource:
        apiVersion: v1
        kind: Secret
        name: awx-token
      vars:
        leaked: "{.data.token}"
EOF
log "Secret refused"

log "checking varsFrom refuses an API group outside vars_from_api_groups"
run_must_fail_without_launching e2e-varsfrom-group "vars_from_api_groups" <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-varsfrom-group
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  varsFrom:
    - resource:
        apiVersion: field.vmware.com/v1
        kind: AWXConnection
        name: e2e-awx
      vars:
        url: "{.spec.url}"
EOF
log "disallowed group refused"

log "checking a varsFrom key colliding with extraVars is refused"
run_must_fail_without_launching e2e-varsfrom-clash "already set in spec.extraVars" <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-varsfrom-clash
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  extraVars:
    zone: literal
  varsFrom:
    - resource:
        apiVersion: v1
        kind: ConfigMap
        name: dns-config
      vars:
        zone: "{.data.zone}"
EOF
log "collision refused"

log "checking a varsFrom path resolving to a non-scalar is refused"
run_must_fail_without_launching e2e-varsfrom-nonscalar "scalar" <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-varsfrom-nonscalar
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  varsFrom:
    - resource:
        apiVersion: v1
        kind: ConfigMap
        name: dns-config
      vars:
        everything: "{.data}"
EOF
log "non-scalar refused"

# --- hosts and vmRef are mutually exclusive, rejected by the schema ---
log "checking hosts and vmRef together are rejected by the CRD"
if cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-both-targets
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  vmRef:
    name: run-vm
  hosts:
    - name: somewhere
EOF
then
  echo "hosts and vmRef were accepted together"
  kubectl delete ansiblerun e2e-both-targets -n "$TEST_NS" --timeout=30s >/dev/null 2>&1 || true
  exit 1
fi
log "both targets rejected"

# --- inline hosts: one adopted, one created ---
# The interesting half is adoption. A host that already exists keeps its
# variables and is never deleted; only the one this run created goes.
log "seeding a pre-existing inventory host, then targeting it and a new one"
SEEDED_RUN_ID=$(curl -sf -X POST "http://${AWX_ADDR}/_test/hosts" \
  -d '{"inventory":1,"name":"db-prod-01","variables":"{\"ansible_host\":\"10.20.5.11\",\"backup_window\":\"02:00-04:00\"}"}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-hosts
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  hosts:
    - name: db-prod-01
    - name: db-prod-02
      address: 10.20.5.12
      variables:
        ansible_user: dbadmin
  extraVars:
    package_name: openssl
EOF

wait_for "inline hosts run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-hosts -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"

HOSTS_JOB=$(kubectl get ansiblerun e2e-hosts -n "$TEST_NS" -o jsonpath='{.status.jobID}')
if [[ "$(launch_limit "$AWX_ADDR" "$HOSTS_JOB")" != "db-prod-01,db-prod-02" ]]; then
  echo "expected the limit to name both hosts, got '$(launch_limit "$AWX_ADDR" "$HOSTS_JOB")'"
  exit 1
fi
# The pre-existing host is adopted: its hand-set variables survive, and no
# address in the CR means ansible_host was left exactly as it was.
ADOPTED_VARS=$(host_field "$AWX_ADDR" db-prod-01 variables)
if [[ "$ADOPTED_VARS" != *"backup_window"* || "$ADOPTED_VARS" != *"10.20.5.11"* ]]; then
  echo "adopted host lost its own variables: $ADOPTED_VARS"
  exit 1
fi
CREATED_VARS=$(host_field "$AWX_ADDR" db-prod-02 variables)
if [[ "$CREATED_VARS" != *"10.20.5.12"* || "$CREATED_VARS" != *"dbadmin"* ]]; then
  echo "created host has the wrong variables: $CREATED_VARS"
  exit 1
fi
# awxHostCreated is omitempty, so "not ours" is an absent field rather than
# an explicit false - read it as JSON instead of through jsonpath.
OWNED=$(kubectl get ansiblerun e2e-hosts -n "$TEST_NS" -o json | python3 -c "
import json, sys
hosts = {h['name']: h.get('awxHostCreated', False) for h in json.load(sys.stdin)['status']['hosts']}
print(json.dumps(hosts, sort_keys=True))
")
if [[ "$OWNED" != '{"db-prod-01": false, "db-prod-02": true}' ]]; then
  echo "ownership recorded wrongly: $OWNED"
  exit 1
fi
CREATED_ID=$(kubectl get ansiblerun e2e-hosts -n "$TEST_NS" \
  -o jsonpath='{range .status.hosts[?(@.name=="db-prod-02")]}{.awxHostID}{end}')
log "adopted db-prod-01 (vars intact), created db-prod-02 (id=$CREATED_ID), limit covered both"

kubectl delete ansiblerun e2e-hosts -n "$TEST_NS" --timeout=30s >/dev/null
if ! host_deleted "$AWX_ADDR" "$CREATED_ID"; then
  echo "the host this run created was not cleaned up"
  exit 1
fi
if host_deleted "$AWX_ADDR" "$SEEDED_RUN_ID"; then
  echo "cleanup deleted the adopted host $SEEDED_RUN_ID, which it did not create"
  exit 1
fi
log "cleanup removed only the created host, leaving the adopted one"

# --- inline host names are literals: hostNamePrefix must not touch them ---
# Prefixing a name the user typed would match nothing in the inventory,
# create a duplicate, and run the playbook against the wrong machine.
log "setting hostNamePrefix, expecting inline host names to ignore it"
kubectl patch awxconnection e2e-awx -n "$TEST_NS" --type=merge \
  -p '{"spec":{"hostNamePrefix":"sup-c-"}}' >/dev/null

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-hosts-prefix
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  hosts:
    - name: literal-host-01
      address: 10.20.9.1
EOF

wait_for "literal-host run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-hosts-prefix -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
PREFIX_JOB=$(kubectl get ansiblerun e2e-hosts-prefix -n "$TEST_NS" -o jsonpath='{.status.jobID}')
if [[ "$(launch_limit "$AWX_ADDR" "$PREFIX_JOB")" != "literal-host-01" ]]; then
  echo "an inline host name was prefixed: limit was '$(launch_limit "$AWX_ADDR" "$PREFIX_JOB")'"
  exit 1
fi
if curl -sf "http://${AWX_ADDR}/_test/hosts" | grep -q 'sup-c-literal-host-01'; then
  echo "a prefixed duplicate of an inline host was created"
  exit 1
fi
log "inline host name used verbatim, no prefixed duplicate"
kubectl delete ansiblerun e2e-hosts-prefix -n "$TEST_NS" --timeout=30s >/dev/null

# --- a name this service DERIVES does carry the prefix ---
log "running against a VM by reference, expecting the derived name to be prefixed"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-vmref
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  vmRef:
    name: run-vm
EOF

wait_for "vmRef run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-vmref -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
VMREF_JOB=$(kubectl get ansiblerun e2e-vmref -n "$TEST_NS" -o jsonpath='{.status.jobID}')
if [[ "$(launch_limit "$AWX_ADDR" "$VMREF_JOB")" != "sup-c-run-vm" ]]; then
  echo "expected the derived host name to carry the prefix, limit was '$(launch_limit "$AWX_ADDR" "$VMREF_JOB")'"
  exit 1
fi
if [[ "$(host_field "$AWX_ADDR" sup-c-run-vm variables)" != *"10.0.0.77"* ]]; then
  echo "the host built from the VM has the wrong ansible_host"
  exit 1
fi
log "vmRef built host sup-c-run-vm from the VM's reported IP and scoped the run to it"
kubectl delete ansiblerun e2e-vmref -n "$TEST_NS" --timeout=30s >/dev/null
kubectl patch awxconnection e2e-awx -n "$TEST_NS" --type=merge -p '{"spec":{"hostNamePrefix":""}}' >/dev/null

# --- a template that would silently drop the limit must be refused ---
log "targeting hosts with a template that has no Prompt on Launch for Limit"
run_must_fail_without_launching e2e-run-noprompt "ask_limit_on_launch" <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-run-noprompt
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "No Prompt Template"
    type: JobTemplate
  hosts:
    - name: never-touched-01
      address: 10.20.9.9
EOF
if curl -sf "http://${AWX_ADDR}/_test/hosts" | grep -q 'never-touched-01'; then
  log "note: the host was upserted before the launch was refused, and cleaned up with the run"
fi
log "refused rather than running against the whole inventory"

# --- activeDeadlineSeconds ends a run wedged on a retryable condition ---
# A referenced object that never appears is deliberately retryable, so
# without a deadline this run would wait forever.
log "applying a run whose varsFrom object never appears, with a short deadline"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-deadline
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  activeDeadlineSeconds: 5
  varsFrom:
    - resource:
        apiVersion: v1
        kind: ConfigMap
        name: never-created
      vars:
        nope: "{.data.nope}"
EOF

wait_for "deadline run reaches Failed" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-deadline -n ${TEST_NS} -o jsonpath='{.status.state}') == Failed ]]"
DEADLINE_MSG=$(kubectl get ansiblerun e2e-deadline -n "$TEST_NS" -o jsonpath='{.status.message}')
if [[ "$DEADLINE_MSG" != *"activeDeadlineSeconds"* ]]; then
  echo "expected the deadline to be named in the message, got: $DEADLINE_MSG"
  exit 1
fi
if [[ -z "$(kubectl get ansiblerun e2e-deadline -n "$TEST_NS" -o jsonpath='{.status.finishedAt}')" ]]; then
  echo "a deadline expiry must set finishedAt so the TTL can collect it"
  exit 1
fi
log "deadline expiry ended the run: $DEADLINE_MSG"
kubectl delete ansiblerun e2e-deadline -n "$TEST_NS" --timeout=30s >/dev/null

# --- ttlSecondsAfterFinished collects the run and its hosts ---
log "applying a run with a short TTL, expecting it to delete itself"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-ttl
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  hosts:
    - name: ttl-host-01
      address: 10.20.9.21
  ttlSecondsAfterFinished: 5
EOF

wait_for "TTL run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-ttl -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
TTL_HOST_ID=$(kubectl get ansiblerun e2e-ttl -n "$TEST_NS" -o jsonpath='{.status.hosts[0].awxHostID}')
wait_for "TTL run deletes itself" 60 bash -c \
  "! kubectl get ansiblerun e2e-ttl -n ${TEST_NS} >/dev/null 2>&1"
if ! host_deleted "$AWX_ADDR" "$TTL_HOST_ID"; then
  echo "the TTL deleted the run but left AWX host $TTL_HOST_ID behind"
  exit 1
fi
log "TTL collected the run and its AWX host $TTL_HOST_ID"

# --- cleanupPolicy: Retain keeps the host when the run goes ---
log "applying a Retain run with a short TTL, expecting the host to survive"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-ttl-retain
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  hosts:
    - name: retained-host-01
      address: 10.20.9.31
  cleanupPolicy: Retain
  ttlSecondsAfterFinished: 5
EOF

wait_for "Retain run reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-ttl-retain -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
RETAIN_HOST_ID=$(kubectl get ansiblerun e2e-ttl-retain -n "$TEST_NS" -o jsonpath='{.status.hosts[0].awxHostID}')
wait_for "Retain run deletes itself" 60 bash -c \
  "! kubectl get ansiblerun e2e-ttl-retain -n ${TEST_NS} >/dev/null 2>&1"
if host_deleted "$AWX_ADDR" "$RETAIN_HOST_ID"; then
  echo "cleanupPolicy: Retain still deleted host $RETAIN_HOST_ID"
  exit 1
fi
log "Retain kept host $RETAIN_HOST_ID after the run was collected"

# --- a run and a binding sharing a name must not share host ownership ---
# The ownership marker lives in the AWX host description. If both kinds
# produced the same marker, each would believe it could delete the other's
# host - so the run's marker carries its kind.
log "creating an AnsibleRun and an AnsibleBinding with the same name, contesting one host"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-contested
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  hosts:
    - name: contested-host
      address: 10.20.9.41
EOF
wait_for "the run claims the host" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-contested -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
CONTESTED_ID=$(kubectl get ansiblerun e2e-contested -n "$TEST_NS" -o jsonpath='{.status.hosts[0].awxHostID}')

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-contested
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: runtarget
  awxConnectionRef: e2e-awx
  hostName: contested-host
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "the binding refuses the run's host" 60 bash -c \
  "kubectl get ansiblebinding e2e-contested -n ${TEST_NS} -o jsonpath='{.status.message}' | grep -q 'already owned'"
if host_deleted "$AWX_ADDR" "$CONTESTED_ID"; then
  echo "the binding deleted the run's host $CONTESTED_ID"
  exit 1
fi
log "same-named binding refused the run's host instead of taking it over"

kubectl delete ansiblebinding e2e-contested -n "$TEST_NS" --timeout=30s >/dev/null
if host_deleted "$AWX_ADDR" "$CONTESTED_ID"; then
  echo "deleting the binding took the run's host $CONTESTED_ID with it"
  exit 1
fi
kubectl delete ansiblerun e2e-contested -n "$TEST_NS" --timeout=30s >/dev/null
if ! host_deleted "$AWX_ADDR" "$CONTESTED_ID"; then
  echo "the run that created host $CONTESTED_ID did not clean it up"
  exit 1
fi
log "ownership stayed with the run throughout"

log "ALL CHECKS PASSED"
