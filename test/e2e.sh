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

child_name() {    # child_name <binding> <vm> -> prints the AnsibleBindingVM's name
  # Child names carry a hash of the binding/VM pair, so they are no
  # longer <binding>-<vm> and are not worth reconstructing here. The
  # binding label plus spec.vmName is how the controller finds them too.
  kubectl get ansiblebindingvm -n "$TEST_NS" -l "field.vmware.com/binding=$1" \
    -o jsonpath="{.items[?(@.spec.vmName=='$2')].metadata.name}" 2>/dev/null
}

vm_field() {      # vm_field <binding> <vm> <status field> -> prints the value
  # The per-VM detail lives on one AnsibleBindingVM per VM.
  local name
  name=$(child_name "$1" "$2")
  [[ -n "$name" ]] || return 0
  kubectl get ansiblebindingvm "$name" -n "$TEST_NS" -o jsonpath="{.status.$3}" 2>/dev/null
}

# Host and template requests the controller has made to one fake AWX.
# Pings are excluded: the AWXConnection validates itself every resync
# whatever the bindings under it are doing.
awx_work_requests() {  # awx_work_requests <addr> -> prints a count
  curl -sf "http://$1/_test/request-count" \
    | python3 -c "import json,sys; c=json.load(sys.stdin); print(c.get('hosts',0)+c.get('templates',0))"
}

# wait_for runs its command with "$@", which for a `bash -c "..."` check is
# a brand new shell: without exporting these, a helper used inside one is
# "command not found" and the check passes or fails for the wrong reason.
export -f host_deleted host_field vm_field child_name awx_work_requests
export TEST_NS

log "creating kind cluster $CLUSTER_NAME"
kind create cluster --name "$CLUSTER_NAME" --kubeconfig "$WORK_DIR/admin.kubeconfig" >/dev/null
export KUBECONFIG="$WORK_DIR/admin.kubeconfig"

log "installing CRDs"
kubectl apply -f "$ROOT_DIR/controller/manifests/crd.yml" >/dev/null
kubectl apply -f "$ROOT_DIR/test/fixtures/vm-crd.yml" >/dev/null
kubectl wait --for=condition=Established --timeout=30s \
  crd/awxconnections.field.vmware.com \
  crd/ansiblebindings.field.vmware.com \
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
# --host-check-period is deliberately a few resyncs long, not equal to
# one: the drift checks below still have to see a host repaired on a
# timer, and the idle-traffic check has to see the passes in between make
# no AWX requests at all.
KUBECONFIG="$WORK_DIR/sa.kubeconfig" "$WORK_DIR/controller" --resync-period=2 --host-check-period=6 > "$WORK_DIR/controller.log" 2>&1 &
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
  "[[ \$(vm_field e2e-config web-1 phase) == Succeeded ]]"

HOST_ID=$(vm_field e2e-config web-1 awxHostID)
JOB_ID=$(vm_field e2e-config web-1 lastJobID)
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
  "[[ \$(vm_field e2e-config web-1 lastJobID) != ${JOB_ID} ]]"
wait_for "the re-run reaches Succeeded" 30 bash -c \
  "[[ \$(vm_field e2e-config web-1 phase) == Succeeded ]]"
RERUN_JOB_ID=$(vm_field e2e-config web-1 lastJobID)
log "re-run launched and succeeded: lastJobID=$RERUN_JOB_ID"

# --- powering a VM off must not clobber its run phase, and must not
#     swallow a re-run requested while it was down ---
log "powering the VM off, expecting its completed phase to survive"
kubectl patch virtualmachine web-1 -n "$TEST_NS" --type=merge \
  -p '{"status":{"powerState":"PoweredOff"}}' >/dev/null
sleep 6   # several resync passes

PHASE_WHILE_OFF=$(vm_field e2e-config web-1 phase)
JOB_WHILE_OFF=$(vm_field e2e-config web-1 lastJobID)
if [[ "$PHASE_WHILE_OFF" != "Succeeded" ]]; then
  echo "expected a powered-off VM to keep its Succeeded phase, got '$PHASE_WHILE_OFF'"
  exit 1
fi
log "phase survived power-off: $PHASE_WHILE_OFF"

log "requesting a re-run while the VM is off, expecting it to be honored once it returns"
kubectl annotate ansiblebinding e2e-config -n "$TEST_NS" \
  ansible.field.vmware.com/reconcile-requested-at="offline-$(date -u +%s)" --overwrite >/dev/null
sleep 6

if [[ "$(vm_field e2e-config web-1 lastJobID)" != "$JOB_WHILE_OFF" ]]; then
  echo "a job was launched against a powered-off VM"
  exit 1
fi

kubectl patch virtualmachine web-1 -n "$TEST_NS" --type=merge \
  -p '{"status":{"powerState":"PoweredOn"}}' >/dev/null
wait_for "the deferred re-run launches once the VM is back" 30 bash -c \
  "[[ \$(vm_field e2e-config web-1 lastJobID) != ${JOB_WHILE_OFF} ]]"
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

# --- an idle child must stop calling AWX between host checks ---
# Every pass used to resolve the template and look up the host, so with
# one object per VM the AWX request rate scaled with the number of VMs
# rather than the number of bindings. What bounds it now is the host
# check running on its own period, with the passes in between deciding
# from status alone that there is nothing to do.
log "checking an idle child makes no AWX requests between host checks"
wait_for "the binding settles before measuring" 30 bash -c \
  "[[ \$(vm_field e2e-config web-1 phase) == Succeeded ]]"

QUIET=0
MAX_QUIET=0
PREV=$(awx_work_requests "$AWX_ADDR")
for _ in $(seq 1 30); do   # 15s at 0.5s per sample
  sleep 0.5
  NOW=$(awx_work_requests "$AWX_ADDR")
  if [[ "$NOW" == "$PREV" ]]; then
    QUIET=$((QUIET + 1))
    if [[ $QUIET -gt $MAX_QUIET ]]; then MAX_QUIET=$QUIET; fi
  else
    QUIET=0
  fi
  PREV="$NOW"
done
# The resync is 2s and the host check period is 6s. If every pass hit AWX
# no quiet run could reach 2s (4 samples); a working bail-out leaves
# roughly 6s (12 samples) of silence between checks.
if [[ $MAX_QUIET -lt 7 ]]; then
  echo "expected AWX to go quiet between host checks, longest quiet run was only $((MAX_QUIET / 2))s"
  exit 1
fi
log "AWX quiet for $((MAX_QUIET / 2))s at a stretch between host checks"

# --- the binding's rollup must reflect its children ---
SUMMARY_TOTAL=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.summary.total}')
SUMMARY_OK=$(kubectl get ansiblebinding e2e-config -n "$TEST_NS" -o jsonpath='{.status.summary.succeeded}')
if [[ "$SUMMARY_TOTAL" != "1" || "$SUMMARY_OK" != "1" ]]; then
  echo "expected the rollup to show 1 of 1 succeeded, got total=$SUMMARY_TOTAL succeeded=$SUMMARY_OK"
  exit 1
fi
log "binding rollup reflects its child: total=$SUMMARY_TOTAL succeeded=$SUMMARY_OK"

# --- a host deleted out of band must be recreated, not trusted ---
# Deleting the inventory host in AWX is drift like any other. Status
# alone cannot see it, and every later run would fail with "--limit does
# not match any hosts", forever, with nothing to repair it.
log "deleting the AWX host out of band, expecting the next reconcile to recreate it"
curl -sf -X DELETE "http://${AWX_ADDR}/api/v2/hosts/${HOST_ID}/" >/dev/null
wait_for "AWX host recreated under a new id" 30 bash -c \
  "id=\$(vm_field e2e-config web-1 awxHostID); [[ -n \$id && \$id != ${HOST_ID} ]]"
HOST_ID=$(vm_field e2e-config web-1 awxHostID)
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

wait_for "AnsibleBindingVM removed for the unmatched VM" 30 bash -c \
  "[[ -z \$(child_name e2e-config web-1) ]]"

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
  "[[ -n \$(vm_field e2e-adopt web-2 lastJobID) ]]"

ADOPTED_ID=$(vm_field e2e-adopt web-2 awxHostID)
CREATED_FLAG=$(vm_field e2e-adopt web-2 awxHostCreated)
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
  "{ vm_field e2e-foreign web-3 phase; vm_field e2e-foreign web-3 message; } | grep -q 'owned by another'"

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
  "[[ \$(vm_field e2e-foreign web-3 awxHostName) == sup-b-web-3 ]]"
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
  "[[ \$(vm_field e2e-retain web-4 awxHostCreated) == true ]]"
RETAINED_ID=$(vm_field e2e-retain web-4 awxHostID)

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
  "[[ \$(vm_field e2e-retain web-4 awxHostID) == ${RETAINED_ID} ]]"

RECLAIMED=$(vm_field e2e-retain web-4 awxHostCreated)
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
  "[[ \$(vm_field e2e-gateway web-5 phase) == Succeeded ]]"
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
  "[[ \$(vm_field e2e-nofilter web-6 phase) == Succeeded ]]"

NOFILTER_HOSTNAME=$(vm_field e2e-nofilter web-6 awxHostName)
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

# --- a hand-made AnsibleBindingVM must be refused, not reconciled ---
# Nothing garbage-collects a child with no owner and no parent reaps one
# with no binding label, but it would otherwise reconcile happily:
# creating AWX hosts, launching jobs, and - because spec.bindingName keys
# the AWX ownership marker - able to point itself at another binding's
# hosts.
log "checking a hand-made AnsibleBindingVM with no VirtualMachine owner is refused"
JOBS_BEFORE_HANDMADE=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleBindingVM
metadata:
  name: e2e-handmade
  namespace: ${TEST_NS}
spec:
  vmName: web-1
  bindingName: e2e-not-a-binding
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "hand-made child is refused" 30 bash -c \
  "kubectl get ansiblebindingvm e2e-handmade -n ${TEST_NS} -o jsonpath='{.status.message}' | grep -q ownerReference"

sleep 4   # a couple of resyncs to relaunch on, if it were going to
JOBS_AFTER_HANDMADE=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)
if [[ "$JOBS_BEFORE_HANDMADE" != "$JOBS_AFTER_HANDMADE" ]]; then
  echo "a hand-made child launched a job: count went $JOBS_BEFORE_HANDMADE -> $JOBS_AFTER_HANDMADE"
  exit 1
fi
kubectl delete ansiblebindingvm e2e-handmade -n "$TEST_NS" --timeout=30s >/dev/null
log "hand-made child refused and launched nothing"

# --- a controller restart must not relaunch anything ---
# A child records what it last ran for in its own status, so a restart
# has to re-derive "this VM is already done" from the object rather than
# from anything the process remembered. Getting this wrong relaunches
# every playbook in the fleet the moment the controller comes back, which
# is the single most expensive mistake this controller can make.
log "checking a controller restart launches nothing"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-7
  namespace: ${TEST_NS}
  labels:
    app: restart
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.77"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-restart
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: restart
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "the binding runs once" 60 bash -c \
  "[[ \$(vm_field e2e-restart web-7 phase) == Succeeded ]]"

RESTART_HOST=$(vm_field e2e-restart web-7 awxHostID)
RESTART_JOB=$(vm_field e2e-restart web-7 lastJobID)
log "provisioned: host=$RESTART_HOST job=$RESTART_JOB"

JOBS_BEFORE_RESTART=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)

log "restarting the controller"
kill "$CONTROLLER_PID" 2>/dev/null || true
wait "$CONTROLLER_PID" 2>/dev/null || true

KUBECONFIG="$WORK_DIR/sa.kubeconfig" "$WORK_DIR/controller" --resync-period=2 --host-check-period=6 >> "$WORK_DIR/controller.log" 2>&1 &
CONTROLLER_PID=$!
wait_for "controller restarted" 30 bash -c \
  "[[ \$(grep -c 'controller started successfully' '$WORK_DIR/controller.log') -ge 2 ]]"

# Several passes for it to relaunch in, if it were going to.
sleep 8

if [[ "$(vm_field e2e-restart web-7 lastJobID)" != "$RESTART_JOB" ]]; then
  echo "the restart relaunched: job went $RESTART_JOB -> $(vm_field e2e-restart web-7 lastJobID)"
  exit 1
fi
if [[ "$(vm_field e2e-restart web-7 awxHostID)" != "$RESTART_HOST" ]]; then
  echo "the restart did not recognise its own AWX host: $RESTART_HOST -> $(vm_field e2e-restart web-7 awxHostID)"
  exit 1
fi
JOBS_AFTER_RESTART=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)
if [[ "$JOBS_BEFORE_RESTART" != "$JOBS_AFTER_RESTART" ]]; then
  echo "expected NO launches across a restart, but the count went $JOBS_BEFORE_RESTART -> $JOBS_AFTER_RESTART"
  exit 1
fi
log "restart kept host $RESTART_HOST and job $RESTART_JOB with zero new launches"

kubectl delete ansiblebinding e2e-restart -n "$TEST_NS" --timeout=60s >/dev/null
wait_for "the AWX host is cleaned up" 30 host_deleted "$AWX_ADDR" "$RESTART_HOST"
log "restart binding deleted cleanly"

log "ALL CHECKS PASSED"
