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

job_canceled() {  # job_canceled <addr> <job id> -> true if AWX was asked to stop it
  curl -sf "http://$1/_test/canceled-jobs" \
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
  # A child is named after its VM alone, with a hash, so its name is not
  # worth reconstructing here. The binding label plus spec.vmName is how
  # the controller finds its own children too.
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

# Launches recorded by one fake AWX, filtered to a template id. The
# deprovision hook is asserted through these rather than through the log:
# what matters is the limit it carried, since a hook launched without one
# would run against every host in the inventory.
hook_launches() { # hook_launches <addr> <template id> -> prints a count
  curl -sf "http://$1/_test/launches" \
    | python3 -c "import json,sys; t=int(sys.argv[1]); print(sum(1 for l in json.load(sys.stdin) if l['templateID']==t))" "$2"
}

hook_launch_field() { # hook_launch_field <addr> <template id> <limit|extraVars> -> prints the last one's value
  curl -sf "http://$1/_test/launches" \
    | python3 -c "
import json, sys
t, field = int(sys.argv[1]), sys.argv[2]
matches = [l for l in json.load(sys.stdin) if l['templateID'] == t]
print(matches[-1][field] if matches else '')
" "$2" "$3"
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
export -f host_deleted host_field vm_field child_name awx_work_requests hook_launches hook_launch_field
export TEST_NS

# Counting "launched job" log lines proves a run happened. Only the launch
# body proves it ran against what the CR asked for - that a targetless run
# sent no limit at all, or that varsFrom reached extra_vars.
launch_body() {   # launch_body <addr> <job id> -> prints the launch body as JSON
  curl -sf "http://$1/_test/launches" \
    | python3 -c "
import json, sys
job = int(sys.argv[1])
for l in json.load(sys.stdin):
    if l['jobID'] == job:
        print(json.dumps({'limit': l['limit'], 'extra_vars': l['extraVars']}))
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

# --hold-template=4 keeps deprovision-hook jobs running until the suite
# releases them: a job that goes terminal on its second poll would finish
# before anything could look at the window that matters, when the
# playbook is running and the host it targets must still exist.
"$WORK_DIR/fakeawx" --addr="$AWX_ADDR" --hold-template=4 > "$WORK_DIR/fakeawx.log" 2>&1 &
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
# hosts. It is also not the claim on the VM it names: children are named
# after the VM alone, so an object under any other name is by definition
# not the one binding that owns that VM's lifecycle.
log "checking a hand-made AnsibleBindingVM that is not a VM's claim is refused"
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
  "kubectl get ansiblebindingvm e2e-handmade -n ${TEST_NS} -o jsonpath='{.status.message}' | grep -q 'not the claim'"

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

# --- deleting a VM must run its onDeleted hook before the host goes ---
# The whole point of the hook: a VM that is deleted gets a deregistration
# playbook run against its inventory host, and the host survives until
# that job is terminal. The child is deleted by the garbage collector
# when its owning VirtualMachine goes, so this exercises the real path -
# no kubectl delete on the child itself.
log "checking the onDeleted hook runs when a VirtualMachine is deleted"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-8
  namespace: ${TEST_NS}
  labels:
    app: deprovision
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.88"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-deprovision
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: deprovision
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  onDeleted:
    template:
      name: "Deregister Host"
      type: JobTemplate
    timeoutSeconds: 120
EOF

wait_for "the deprovision binding runs once" 60 bash -c \
  "[[ \$(vm_field e2e-deprovision web-8 phase) == Succeeded ]]"

DEPROV_HOST=$(vm_field e2e-deprovision web-8 awxHostID)
DEPROV_HOST_NAME=$(vm_field e2e-deprovision web-8 awxHostName)
DEPROV_CHILD=$(child_name e2e-deprovision web-8)
log "provisioned: host=$DEPROV_HOST ($DEPROV_HOST_NAME) child=$DEPROV_CHILD"

if [[ "$(hook_launches "$AWX_ADDR" 4)" != "0" ]]; then
  echo "the deprovision hook ran before anything was deleted"
  exit 1
fi

log "deleting the VirtualMachine"
kubectl delete virtualmachine web-8 -n "$TEST_NS" --wait=false >/dev/null

wait_for "the hook launches" 60 bash -c \
  "[[ \$(hook_launches ${AWX_ADDR} 4) == 1 ]]"

# The host must still be there while the job runs against it: deleting it
# first would leave the playbook with a --limit matching nothing.
if host_deleted "$AWX_ADDR" "$DEPROV_HOST"; then
  echo "the inventory host was deleted out from under the running hook"
  exit 1
fi

HOOK_LIMIT=$(hook_launch_field "$AWX_ADDR" 4 limit)
if [[ "$HOOK_LIMIT" != "$DEPROV_HOST_NAME" ]]; then
  echo "the hook was not scoped to its own host: limit=$HOOK_LIMIT want=$DEPROV_HOST_NAME"
  exit 1
fi

HOOK_VARS=$(hook_launch_field "$AWX_ADDR" 4 extraVars)
for want in '"asb_hook": "onDeleted"' '"asb_vm_name": "web-8"' '"asb_binding": "e2e-deprovision"' '"asb_last_known_ip": "10.0.0.88"'; do
  echo "$HOOK_VARS" | python3 -c "
import json, sys
key, value = sys.argv[1].split(': ', 1)
vars = json.loads(sys.stdin.read() or '{}')
sys.exit(0 if vars.get(json.loads(key)) == json.loads(value) else 1)
" "$want" || { echo "the hook did not carry $want: $HOOK_VARS"; exit 1; }
done

# The guest is destroyed by now and its address may already be re-leased,
# so the run has to be pinned to the control node.
HOOK_HOST_VARS=$(host_field "$AWX_ADDR" "$DEPROV_HOST_NAME" variables)
if ! echo "$HOOK_HOST_VARS" | grep -q '"ansible_connection":"local"'; then
  echo "the host was not pinned to the control node before the hook ran: $HOOK_HOST_VARS"
  exit 1
fi
log "hook launched with limit=$HOOK_LIMIT, pinned to the control node, host still present"

# The hook's own state has to be on the object, not in the process, or
# the next pass would start again from the beginning.
wait_for "the hook's progress is recorded on the child" 30 bash -c \
  "[[ \$(kubectl get ansiblebindingvm ${DEPROV_CHILD} -n ${TEST_NS} -o jsonpath='{.status.deprovision.phase}') == Running ]]"

HOOK_JOB=$(hook_launch_field "$AWX_ADDR" 4 jobID)
log "releasing held hook job $HOOK_JOB"
curl -sf -X POST "http://${AWX_ADDR}/_test/finish-job?id=${HOOK_JOB}" >/dev/null

wait_for "the host is removed once the hook is terminal" 90 host_deleted "$AWX_ADDR" "$DEPROV_HOST"
wait_for "the child finishes finalizing" 60 bash -c \
  "[[ -z \$(kubectl get ansiblebindingvm ${DEPROV_CHILD} -n ${TEST_NS} --ignore-not-found -o name) ]]"

# The child took its status with it, so the record of what the teardown
# did has to be somewhere that outlives it.
wait_for "the outcome is recorded on the binding" 30 bash -c \
  "kubectl get events -n ${TEST_NS} --field-selector involvedObject.name=e2e-deprovision -o jsonpath='{.items[*].reason}' | grep -q DeprovisionHook"
log "hook completed, host removed, outcome recorded on the binding"

# --- a VM that merely stops matching must NOT get the hook ---
# Same object, same finalizer, entirely different intent: the VM is alive
# and running, and a decommission playbook against it would be damage
# rather than cleanup.
log "checking a live VM that leaves the binding does not get the hook"
HOOK_LAUNCHES_BEFORE_DETACH=$(hook_launches "$AWX_ADDR" 4)

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-9
  namespace: ${TEST_NS}
  labels:
    app: deprovision
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.99"
EOF

wait_for "the second VM runs" 60 bash -c \
  "[[ \$(vm_field e2e-deprovision web-9 phase) == Succeeded ]]"
DETACH_HOST=$(vm_field e2e-deprovision web-9 awxHostID)

log "relabelling it out of the selector"
kubectl label virtualmachine web-9 -n "$TEST_NS" app=somewhere-else --overwrite >/dev/null

wait_for "its host is cleaned up" 60 host_deleted "$AWX_ADDR" "$DETACH_HOST"
if [[ "$(hook_launches "$AWX_ADDR" 4)" != "$HOOK_LAUNCHES_BEFORE_DETACH" ]]; then
  echo "the hook fired for a VM that is still running"
  exit 1
fi
log "relabelled VM kept its guest and got no deprovision playbook"

kubectl delete virtualmachine web-9 -n "$TEST_NS" --timeout=60s >/dev/null
kubectl delete ansiblebinding e2e-deprovision -n "$TEST_NS" --timeout=90s >/dev/null
log "deprovision binding deleted cleanly"

# --- switching to Retain while the hook runs must undo the pin --------
# The hook pins ansible_connection: local so a playbook that forgets
# delegate_to cannot reach an address the VM no longer owns. Under Delete
# the host goes anyway, so the pin never matters. Change the policy while
# the job is in flight - which the parent deliberately propagates into a
# terminating child - and the host now survives carrying an override that
# would send the next provisioning run to the AWX control node instead of
# the machine. It has to come back off.
log "checking a mid-hook switch to Retain hands the host back unpinned"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-10
  namespace: ${TEST_NS}
  labels:
    app: retain-hook
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.90"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-retain-hook
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: retain-hook
  awxConnectionRef: e2e-awx
  cleanupPolicy: Delete
  template:
    name: "Configure Webserver"
    type: JobTemplate
  onDeleted:
    template:
      name: "Deregister Host"
      type: JobTemplate
    timeoutSeconds: 120
EOF

wait_for "the retain-hook binding runs once" 60 bash -c \
  "[[ \$(vm_field e2e-retain-hook web-10 phase) == Succeeded ]]"

RETAIN_HOOK_HOST=$(vm_field e2e-retain-hook web-10 awxHostID)
RETAIN_HOOK_NAME=$(vm_field e2e-retain-hook web-10 awxHostName)
RETAIN_HOOK_CHILD=$(child_name e2e-retain-hook web-10)
RETAIN_HOOK_LAUNCHES=$(hook_launches "$AWX_ADDR" 4)

# The host the controller created has no ansible_connection of its own,
# so the pin is the only thing that can put one there and an absent value
# is what "restored" has to mean.
RETAIN_VARS_BEFORE=$(host_field "$AWX_ADDR" "$RETAIN_HOOK_NAME" variables)
if echo "$RETAIN_VARS_BEFORE" | grep -q ansible_connection; then
  echo "host $RETAIN_HOOK_NAME already had an ansible_connection: $RETAIN_VARS_BEFORE"
  exit 1
fi

kubectl delete virtualmachine web-10 -n "$TEST_NS" --wait=false >/dev/null

wait_for "the retain-hook hook launches" 60 bash -c \
  "[[ \$(hook_launches ${AWX_ADDR} 4) == $((RETAIN_HOOK_LAUNCHES + 1)) ]]"
wait_for "the hook is recorded as running on the child" 30 bash -c \
  "[[ \$(kubectl get ansiblebindingvm ${RETAIN_HOOK_CHILD} -n ${TEST_NS} -o jsonpath='{.status.deprovision.phase}') == Running ]]"

RETAIN_HOOK_VARS=$(host_field "$AWX_ADDR" "$RETAIN_HOOK_NAME" variables)
if ! echo "$RETAIN_HOOK_VARS" | grep -q '"ansible_connection":"local"'; then
  echo "the host was not pinned before the hook ran: $RETAIN_HOOK_VARS"
  exit 1
fi

# Mid-flight, with the job still held: at launch time this host was not
# expected to survive at all.
log "switching e2e-retain-hook to cleanupPolicy: Retain with the hook still running"
kubectl patch ansiblebinding e2e-retain-hook -n "$TEST_NS" --type=merge \
  -p '{"spec":{"cleanupPolicy":"Retain"}}' >/dev/null

RETAIN_HOOK_JOB=$(hook_launch_field "$AWX_ADDR" 4 jobID)
curl -sf -X POST "http://${AWX_ADDR}/_test/finish-job?id=${RETAIN_HOOK_JOB}" >/dev/null

wait_for "the child finishes finalizing" 60 bash -c \
  "[[ -z \$(kubectl get ansiblebindingvm ${RETAIN_HOOK_CHILD} -n ${TEST_NS} --ignore-not-found -o name) ]]"

if host_deleted "$AWX_ADDR" "$RETAIN_HOOK_HOST"; then
  echo "the policy change to Retain did not take effect: host $RETAIN_HOOK_HOST was deleted"
  exit 1
fi

RETAIN_VARS_AFTER=$(host_field "$AWX_ADDR" "$RETAIN_HOOK_NAME" variables)
if echo "$RETAIN_VARS_AFTER" | grep -q ansible_connection; then
  echo "the hook's pin outlived a hook that ended under Retain: $RETAIN_VARS_AFTER"
  exit 1
fi
if [[ "$RETAIN_VARS_AFTER" != "$RETAIN_VARS_BEFORE" ]]; then
  echo "the retained host was not handed back as it was found: before=$RETAIN_VARS_BEFORE after=$RETAIN_VARS_AFTER"
  exit 1
fi
log "policy changed mid-hook: host kept, pin removed, variables back to what they were"

kubectl delete ansiblebinding e2e-retain-hook -n "$TEST_NS" --timeout=90s >/dev/null
if host_deleted "$AWX_ADDR" "$RETAIN_HOOK_HOST"; then
  echo "deleting the binding removed host $RETAIN_HOOK_HOST, which Retain says to keep"
  exit 1
fi
log "retain-hook binding deleted cleanly, host kept"

# --- one binding per VM ---
# Two bindings may select the same VM, but only one may own its
# lifecycle: the child's name is derived from the VM alone, so the create
# that wins is the arbitration. The loser must say so rather than run a
# second playbook at the same machine.
log "checking a second binding selecting the same VM is refused, not given a claim of its own"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-11
  namespace: ${TEST_NS}
  labels:
    app: claimed
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.111"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-claim-a
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: claimed
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "the first binding claims web-11" 60 bash -c \
  "[[ \$(vm_field e2e-claim-a web-11 phase) == Succeeded ]]"
CLAIM_CHILD=$(child_name e2e-claim-a web-11)
CLAIM_JOBS_BEFORE=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-claim-b
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: claimed
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
EOF

wait_for "the second binding reports Conflict" 60 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-claim-b -n ${TEST_NS} -o jsonpath='{.status.state}') == Conflict ]]"

CLAIM_COUNT=$(kubectl get ansiblebindingvm -n "$TEST_NS" \
  -o jsonpath="{.items[?(@.spec.vmName=='web-11')].metadata.name}" | wc -w)
if [[ "$CLAIM_COUNT" != "1" ]]; then
  echo "expected exactly one AnsibleBindingVM for web-11, found $CLAIM_COUNT"
  exit 1
fi

CONFLICT_MSG=$(kubectl get ansiblebinding e2e-claim-b -n "$TEST_NS" -o jsonpath='{.status.message}')
if ! echo "$CONFLICT_MSG" | grep -q "e2e-claim-a"; then
  echo "the conflict does not name the binding holding the claim: $CONFLICT_MSG"
  exit 1
fi
if [[ "$(kubectl get ansiblebinding e2e-claim-b -n "$TEST_NS" -o jsonpath='{.status.ready}')" == "true" ]]; then
  echo "a binding that owns none of its selected VMs reported Ready"
  exit 1
fi

sleep 4   # a resync or two for a second launch to happen on, if it would
CLAIM_JOBS_AFTER=$(grep -c "fakeawx: launched job" "$WORK_DIR/fakeawx.log" || true)
if [[ "$CLAIM_JOBS_BEFORE" != "$CLAIM_JOBS_AFTER" ]]; then
  echo "the second binding launched a job against a VM it does not own: $CLAIM_JOBS_BEFORE -> $CLAIM_JOBS_AFTER"
  exit 1
fi

# The claim's identity is what the exclusivity rests on, so the schema
# refuses to let it be edited out from under a running job.
if kubectl patch ansiblebindingvm "$CLAIM_CHILD" -n "$TEST_NS" --type=merge \
  -p '{"spec":{"vmName":"web-1"}}' >/dev/null 2>&1; then
  echo "spec.vmName was mutable: the claim can be pointed at another VM"
  exit 1
fi

kubectl delete ansiblebinding e2e-claim-b -n "$TEST_NS" --timeout=60s >/dev/null
sleep 2
if [[ -z "$(kubectl get ansiblebindingvm "$CLAIM_CHILD" -n "$TEST_NS" --ignore-not-found -o name)" ]]; then
  echo "deleting the conflicting binding took the owner's claim with it"
  exit 1
fi
wait_for "the owning binding is still Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblebinding e2e-claim-a -n ${TEST_NS} -o jsonpath='{.status.ready}') == true ]]"
kubectl delete ansiblebinding e2e-claim-a -n "$TEST_NS" --timeout=90s >/dev/null
kubectl delete virtualmachine web-11 -n "$TEST_NS" --ignore-not-found --timeout=60s >/dev/null
log "one claim, one owner: the second binding reported it and launched nothing"

# --- onDeleted targeting: Template ---
# A decommission whose records live somewhere other than the machine.
# The workflow has no inventory of its own and no limit to prompt for -
# refused under the default ManagedHost targeting, which is the point of
# having the other mode - and the controller must supply neither, nor
# touch the inventory host on its way past.
log "checking onDeleted.targeting: Template launches a workflow the managed host cannot narrow"

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: vmoperator.vmware.com/v1alpha2
kind: VirtualMachine
metadata:
  name: web-12
  namespace: ${TEST_NS}
  labels:
    app: records
spec: {}
status:
  powerState: PoweredOn
  network:
    primaryIP4: "10.0.0.112"
---
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-template-hook
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: records
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  onDeleted:
    targeting: Template
    template:
      name: "Decommission Records"
      type: WorkflowTemplate
    timeoutSeconds: 120
EOF

wait_for "the template-targeted binding runs once" 60 bash -c \
  "[[ \$(vm_field e2e-template-hook web-12 phase) == Succeeded ]]"
TMPL_HOST=$(vm_field e2e-template-hook web-12 awxHostID)
TMPL_HOST_NAME=$(vm_field e2e-template-hook web-12 awxHostName)
TMPL_CHILD=$(child_name e2e-template-hook web-12)

log "deleting the VirtualMachine"
kubectl delete virtualmachine web-12 -n "$TEST_NS" --wait=false >/dev/null

wait_for "the template-targeted hook launches" 60 bash -c \
  "[[ \$(hook_launches ${AWX_ADDR} 5) == 1 ]]"

TMPL_LIMIT=$(hook_launch_field "$AWX_ADDR" 5 limit)
if [[ -n "$TMPL_LIMIT" ]]; then
  echo "Template targeting narrowed the run anyway: limit=$TMPL_LIMIT"
  exit 1
fi

TMPL_VARS=$(hook_launch_field "$AWX_ADDR" 5 extraVars)
if ! echo "$TMPL_VARS" | grep -q '"asb_vm_name": *"web-12"'; then
  echo "the deletion context did not survive a launch with no host to read it from: $TMPL_VARS"
  exit 1
fi

# Nothing was aimed at the inventory host, so nothing may have been
# written to it either.
TMPL_HOST_VARS=$(host_field "$AWX_ADDR" "$TMPL_HOST_NAME" variables)
if echo "$TMPL_HOST_VARS" | grep -q ansible_connection; then
  echo "a Template-targeted hook pinned a host it was never aimed at: $TMPL_HOST_VARS"
  exit 1
fi

wait_for "the host is removed once the hook is terminal" 90 host_deleted "$AWX_ADDR" "$TMPL_HOST"
wait_for "the child finishes finalizing" 60 bash -c \
  "[[ -z \$(kubectl get ansiblebindingvm ${TMPL_CHILD} -n ${TEST_NS} --ignore-not-found -o name) ]]"
kubectl delete ansiblebinding e2e-template-hook -n "$TEST_NS" --timeout=90s >/dev/null
log "Template targeting launched with no limit and no inventory, and left the managed host alone"

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

# --- spec cannot be removed either ---
# A CEL transition rule is not evaluated when the field it guards is added
# or removed, so an optional spec could be dropped in one update and
# reintroduced with different contents in the next - a second execution
# from a resource whose spec is documented as immutable. The schema
# requires spec at the root to close that.
log "removing a run's spec, expecting the API server to reject it"
if kubectl patch ansiblerun e2e-standalone -n "$TEST_NS" --type=merge \
     -p '{"spec":null}' >/dev/null 2>&1; then
  echo "spec was removable; the immutability rule can be stepped around"
  exit 1
fi
log "spec removal rejected"
kubectl delete ansiblerun e2e-standalone -n "$TEST_NS" --timeout=30s >/dev/null

log "creating an AnsibleRun with no spec, expecting the API server to reject it"
if kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-nospec
  namespace: ${TEST_NS}
EOF
then
  echo "a spec-less AnsibleRun was accepted"
  kubectl delete ansiblerun e2e-nospec -n "$TEST_NS" --timeout=30s >/dev/null 2>&1 || true
  exit 1
fi
log "spec-less run rejected"

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

log "checking varsFrom refuses a kind in a group vars_from_resources does not name"
run_must_fail_without_launching e2e-varsfrom-group "not permitted to read anything in" <<EOF
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

# The core group is the one that cannot be granted by wildcard: a
# Supervisor's ValidatingAdmissionPolicy refuses a service ClusterRole
# that names resources: ["*"] on it, so config/deploy.yml grants core one
# resource at a time and hands the controller the same list. A core kind
# outside that list has to be refused with an explanation rather than
# sent as a read the ClusterRole never granted.
log "checking varsFrom refuses a kind vars_from_resources does not name, in a group it does"
run_must_fail_without_launching e2e-varsfrom-core "core/services" <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-varsfrom-core
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  varsFrom:
    - resource:
        apiVersion: v1
        kind: Service
        name: kubernetes
      vars:
        ip: "{.spec.clusterIP}"
EOF
log "core resource outside the grant refused"

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

# --- an existing host is borrowed, not written to ---
# Executing a playbook against a machine is not ownership of the record
# that describes it. Host writes the run cannot apply are refused rather
# than dropped, or the playbook would run with values AWX never saw.
log "targeting an existing host with an address, expecting a refusal rather than a silent drop"
curl -sf -X POST "http://${AWX_ADDR}/_test/hosts" \
  -d '{"inventory":1,"name":"db-prod-09","variables":"{\"ansible_host\":\"10.20.5.19\"}"}' >/dev/null
LAUNCHES_BEFORE_REFUSAL=$(launch_count)
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-hostwrite
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  hosts:
    - name: db-prod-09
      address: 10.20.5.99
EOF

wait_for "the run refuses to write onto a host it does not own" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-hostwrite -n ${TEST_NS} -o jsonpath='{.status.state}') == Failed ]]"
REFUSAL=$(kubectl get ansiblerun e2e-hostwrite -n "$TEST_NS" -o jsonpath='{.status.failureReason}')
if [[ "$REFUSAL" != *"spec.extraVars"* ]]; then
  echo "the refusal should point at launch variables as the way to do this: $REFUSAL"
  exit 1
fi
if [[ "$(host_field "$AWX_ADDR" db-prod-09 variables)" != *"10.20.5.19"* ]]; then
  echo "the existing host's address was overwritten anyway"
  exit 1
fi
if [[ "$(launch_count)" != "$LAUNCHES_BEFORE_REFUSAL" ]]; then
  echo "launched a job it could not set up correctly"
  exit 1
fi
log "host writes on a borrowed host refused, inventory untouched, nothing launched"
kubectl delete ansiblerun e2e-hostwrite -n "$TEST_NS" --timeout=30s >/dev/null

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

# --- a one-off run against a host an AnsibleBinding manages ---
# The case the whole ownership split exists for: a smoke test against a
# VM the binding provisioned. Same host, same id, no inventory writes,
# and the binding keeps it when the run goes.
log "binding the VM, then running a one-off AnsibleRun against the host it manages"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: e2e-shared
  namespace: ${TEST_NS}
spec:
  vmSelector:
    app: runtarget
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  hostVariables:
    tier: gold
EOF

wait_for "the binding provisions its host" 90 bash -c \
  "[[ -n \$(host_field ${AWX_ADDR} sup-c-run-vm id) ]]"
SHARED_ID=$(host_field "$AWX_ADDR" sup-c-run-vm id)
SHARED_DESC=$(host_field "$AWX_ADDR" sup-c-run-vm description)
SHARED_VARS=$(host_field "$AWX_ADDR" sup-c-run-vm variables)

cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-shared-run
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Configure Webserver"
    type: JobTemplate
  vmRef:
    name: run-vm
  extraVars:
    tier: platinum
EOF

wait_for "the run against the binding's host reaches Ready" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-shared-run -n ${TEST_NS} -o jsonpath='{.status.state}') == Ready ]]"
SHARED_JOB=$(kubectl get ansiblerun e2e-shared-run -n "$TEST_NS" -o jsonpath='{.status.jobID}')
if [[ "$(launch_limit "$AWX_ADDR" "$SHARED_JOB")" != "sup-c-run-vm" ]]; then
  echo "expected the run scoped to the binding's host, got '$(launch_limit "$AWX_ADDR" "$SHARED_JOB")'"
  exit 1
fi
RUN_HOST_ID=$(kubectl get ansiblerun e2e-shared-run -n "$TEST_NS" -o jsonpath='{.status.hosts[0].awxHostID}')
if [[ "$RUN_HOST_ID" != "$SHARED_ID" ]]; then
  echo "the run made its own host $RUN_HOST_ID instead of using the binding's $SHARED_ID"
  exit 1
fi
RUN_OWNS=$(kubectl get ansiblerun e2e-shared-run -n "$TEST_NS" -o json | python3 -c \
  "import json,sys; print(json.load(sys.stdin)['status']['hosts'][0].get('awxHostCreated', False))")
if [[ "$RUN_OWNS" != "False" ]]; then
  echo "the run recorded a borrowed host as its own to delete"
  exit 1
fi
if [[ "$(host_field "$AWX_ADDR" sup-c-run-vm description)" != "$SHARED_DESC" ]]; then
  echo "the run took over the binding's ownership marker"
  exit 1
fi
if [[ "$(host_field "$AWX_ADDR" sup-c-run-vm variables)" != "$SHARED_VARS" ]]; then
  echo "the run changed the binding's host variables: $(host_field "$AWX_ADDR" sup-c-run-vm variables)"
  exit 1
fi
# The run's own value went to the job, where it overrides the inventory
# for that execution only.
if [[ "$(launch_var "$AWX_ADDR" "$SHARED_JOB" tier)" != "platinum" ]]; then
  echo "the run's extraVars did not reach the launch"
  exit 1
fi
if [[ "$SHARED_VARS" != *"gold"* ]]; then
  echo "expected the binding's own variable to still read gold: $SHARED_VARS"
  exit 1
fi
log "the run used the binding's host $SHARED_ID unchanged, with its own value in the job"

kubectl delete ansiblerun e2e-shared-run -n "$TEST_NS" --timeout=30s >/dev/null
if host_deleted "$AWX_ADDR" "$SHARED_ID"; then
  echo "deleting the run deleted the binding's host"
  exit 1
fi
log "the binding kept its host when the run was deleted"
kubectl delete ansiblebinding e2e-shared -n "$TEST_NS" --timeout=60s >/dev/null

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

# --- a deadline that expires stops the job, it does not just relabel it ---
# A Kubernetes Job terminates its pods when activeDeadlineSeconds elapses.
# A run that marked itself Failed and left the playbook executing would be
# reporting an end state for work still changing machines. "Deregister
# Host" is the held template, so its jobs stay running until released.
log "applying a run against a held job with a short deadline"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-deadline-cancel
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Deregister Host"
    type: JobTemplate
  activeDeadlineSeconds: 5
  hosts:
    - name: deadline-host-01
      address: 10.20.9.31
EOF

wait_for "the held run records its job" 60 bash -c \
  "[[ -n \$(kubectl get ansiblerun e2e-deadline-cancel -n ${TEST_NS} -o jsonpath='{.status.jobID}') ]]"
HELD_JOB=$(kubectl get ansiblerun e2e-deadline-cancel -n "$TEST_NS" -o jsonpath='{.status.jobID}')
wait_for "the deadline expires" 60 bash -c \
  "[[ \$(kubectl get ansiblerun e2e-deadline-cancel -n ${TEST_NS} -o jsonpath='{.status.state}') == Failed ]]"
if ! job_canceled "$AWX_ADDR" "$HELD_JOB"; then
  echo "the deadline expired but AWX job $HELD_JOB was left running"
  exit 1
fi
CANCEL_MSG=$(kubectl get ansiblerun e2e-deadline-cancel -n "$TEST_NS" -o jsonpath='{.status.message}')
if [[ "$CANCEL_MSG" != *"canceled"* ]]; then
  echo "expected the message to say what became of the job, got: $CANCEL_MSG"
  exit 1
fi
log "deadline expiry canceled job $HELD_JOB: $CANCEL_MSG"
kubectl delete ansiblerun e2e-deadline-cancel -n "$TEST_NS" --timeout=30s >/dev/null

# --- deleting a run stops the job it started ---
# Deleting a Kubernetes Job deletes its pods. Leaving the playbook running
# with the object that asked for it gone leaves nothing pointing at the job.
log "deleting a run whose job is still running"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-delete-cancel
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Deregister Host"
    type: JobTemplate
  hosts:
    - name: delete-host-01
      address: 10.20.9.32
EOF

wait_for "the run records its job" 60 bash -c \
  "[[ -n \$(kubectl get ansiblerun e2e-delete-cancel -n ${TEST_NS} -o jsonpath='{.status.jobID}') ]]"
DELETE_JOB=$(kubectl get ansiblerun e2e-delete-cancel -n "$TEST_NS" -o jsonpath='{.status.jobID}')
DELETE_HOST_ID=$(kubectl get ansiblerun e2e-delete-cancel -n "$TEST_NS" \
  -o jsonpath='{range .status.hosts[?(@.name=="delete-host-01")]}{.awxHostID}{end}')
kubectl delete ansiblerun e2e-delete-cancel -n "$TEST_NS" --timeout=60s >/dev/null
if ! job_canceled "$AWX_ADDR" "$DELETE_JOB"; then
  echo "the run was deleted but AWX job $DELETE_JOB was left running"
  exit 1
fi
if ! host_deleted "$AWX_ADDR" "$DELETE_HOST_ID"; then
  echo "the host the deleted run created was left behind"
  exit 1
fi
log "deleting the run canceled job $DELETE_JOB and removed its host"

# --- and it waits for AWX to say the job actually stopped ---
# AWX answers a cancel with 202 Accepted: it has taken the request, not
# stopped the playbook. Deleting the inventory host on the strength of
# that answer races the work being torn down.
log "deleting a run whose job accepts the cancel but does not stop"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: e2e-delete-hang
  namespace: ${TEST_NS}
spec:
  awxConnectionRef: e2e-awx
  template:
    name: "Deregister Host"
    type: JobTemplate
  hosts:
    - name: hang-host-01
      address: 10.20.9.33
EOF

wait_for "the hanging run records its job" 60 bash -c \
  "[[ -n \$(kubectl get ansiblerun e2e-delete-hang -n ${TEST_NS} -o jsonpath='{.status.jobID}') ]]"
HANG_JOB=$(kubectl get ansiblerun e2e-delete-hang -n "$TEST_NS" -o jsonpath='{.status.jobID}')
HANG_HOST_ID=$(kubectl get ansiblerun e2e-delete-hang -n "$TEST_NS" \
  -o jsonpath='{range .status.hosts[?(@.name=="hang-host-01")]}{.awxHostID}{end}')
curl -sf -X POST "http://${AWX_ADDR}/_test/hang-cancel?id=${HANG_JOB}" >/dev/null

kubectl delete ansiblerun e2e-delete-hang -n "$TEST_NS" --wait=false >/dev/null
sleep 8
if ! kubectl get ansiblerun e2e-delete-hang -n "$TEST_NS" >/dev/null 2>&1; then
  echo "the run released its finalizer before AWX confirmed job $HANG_JOB had stopped"
  exit 1
fi
if host_deleted "$AWX_ADDR" "$HANG_HOST_ID"; then
  echo "deleted the inventory host job $HANG_JOB may still be running against"
  exit 1
fi
if [[ -z "$(kubectl get ansiblerun e2e-delete-hang -n "$TEST_NS" -o jsonpath='{.status.cancelRequestedAt}')" ]]; then
  echo "the cancel request was not recorded, so nothing bounds the wait for it"
  exit 1
fi

# AWX finishes stopping it.
curl -sf -X POST "http://${AWX_ADDR}/_test/finish-job?id=${HANG_JOB}" >/dev/null
wait_for "the run finishes deleting once its job has stopped" 60 bash -c \
  "! kubectl get ansiblerun e2e-delete-hang -n ${TEST_NS} >/dev/null 2>&1"
if ! host_deleted "$AWX_ADDR" "$HANG_HOST_ID"; then
  echo "the host was left behind once the run finally released"
  exit 1
fi
log "deletion held the finalizer until job $HANG_JOB stopped, then removed its host"

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
