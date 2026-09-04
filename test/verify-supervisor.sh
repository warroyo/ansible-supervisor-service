#!/usr/bin/env bash
# Pre-release validation against a live Supervisor and a live AWX.
#
# This is the gate test/e2e.sh cannot be. That suite runs the controller
# as a host binary, against a fake AWX and a stand-in VirtualMachine CRD,
# so it never exercises the built image, the Carvel package, the pinned
# digest, the in-cluster RBAC, or a real vm-operator. Everything listed
# under "What the suite can't cover" in CONTRIBUTING.md is checked here
# instead, plus that an idle binding really does stop writing.
#
# Deliberately not a GitHub Action: it needs a Supervisor and an AWX
# instance reachable on the same network, which a hosted runner has no
# path to. Run it by hand before cutting a tag. CI still runs only
# `make test-unit` and `make test-e2e`, both unchanged.
#
# Usage:
#   export KUBECONFIG=/path/to/supervisor.kubeconfig
#   export SUPERVISOR_NS=my-namespace
#   export AWX_URL=https://awx.example.com
#   export AWX_TOKEN=...
#   export AWX_TEMPLATE="Configure Webserver"
#   export VM_LABEL=app=webserver
#   make verify-supervisor
#
# The template must have Prompt on Launch enabled for Limit (and for
# Variables), and VM_LABEL must match at least one powered-on VM that
# already reports an IP. Both are checked before anything is created.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="$(mktemp -d)"
KEEP=0
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
  esac
done

: "${SUPERVISOR_NS:?set SUPERVISOR_NS to the vSphere namespace to test in}"
: "${AWX_URL:?set AWX_URL to a reachable AWX/AAP instance}"
: "${AWX_TOKEN:?set AWX_TOKEN to an API token for that instance}"
: "${AWX_TEMPLATE:?set AWX_TEMPLATE to a job template name with Prompt on Launch for Limit}"

# The VM to run against. Left unset, the harness creates and destroys its
# own - the fixture is part of the gate rather than something built by
# hand before each release. Set VM_LABEL to point at a VM you already
# have and manage yourself.
OWN_FIXTURE=0
if [[ -z "${VM_LABEL:-}" ]]; then
  OWN_FIXTURE=1
  VM_LABEL="${FIXTURE_LABEL:-app=${FIXTURE_NAME:-ansible-verify-fixture}}"
fi

TEMPLATE_TYPE="${TEMPLATE_TYPE:-JobTemplate}"
# EXPECT_IMAGE, when set, is the exact image reference the release under
# test pushed. Asserting it is what stops a run silently validating
# whatever happened to be installed last week.
EXPECT_IMAGE="${EXPECT_IMAGE:-}"

PREFIX="verify-$(date -u +%H%M%S)"
CONN="${PREFIX}-awx"
BINDING="${PREFIX}-binding"
SECRET="${PREFIX}-token"
CTRL_NS=""
CREATED=0

log()  { echo "[verify] $*"; }
fail() { echo "[verify] FAILED: $*" >&2; exit 1; }

# The per-VM detail lives on one AnsibleBindingVM per matched VM. Their
# names carry a hash of the binding/VM pair, so they are looked up by the
# label the binding stamps on them rather than reconstructed.
children() { kubectl get ansiblebindingvm -n "$SUPERVISOR_NS" -l "field.vmware.com/binding=${BINDING}" "$@"; }
child_field() {  # child_field <vm name> <status field>
  children -o jsonpath="{.items[?(@.spec.vmName=='$1')].status.$2}"
}

cleanup() {
  status=$?
  if [[ $status -ne 0 && -n "$CTRL_NS" ]]; then
    echo "=== FAILURE: last 200 lines of controller log ==="
    kubectl logs -n "$CTRL_NS" -l app=ansible-supervisor --tail=200 2>/dev/null || true
    echo "=== binding status ==="
    kubectl get ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" -o yaml 2>/dev/null || true
  fi

  # Always take our own objects back out, even on failure: this runs in a
  # real tenant namespace, and a leaked binding keeps launching jobs.
  if [[ $CREATED -eq 1 && $KEEP -eq 0 ]]; then
    log "cleaning up"
    kubectl delete ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" --timeout=120s >/dev/null 2>&1 || true
    kubectl delete awxconnection "$CONN" -n "$SUPERVISOR_NS" --timeout=60s >/dev/null 2>&1 || true
    kubectl delete secret "$SECRET" -n "$SUPERVISOR_NS" >/dev/null 2>&1 || true
  elif [[ $KEEP -eq 1 ]]; then
    log "--keep set: leaving $BINDING / $CONN / $SECRET in $SUPERVISOR_NS"
  fi

  # The fixture goes last: the binding's finalizer cleans up AWX hosts
  # derived from the VM, so destroying the VM first would leave the
  # binding reconciling against something that no longer exists.
  if [[ $OWN_FIXTURE -eq 1 && $KEEP -eq 0 ]]; then
    "${HARNESS_DIR}/fixture.sh" down || true
  elif [[ $OWN_FIXTURE -eq 1 ]]; then
    log "--keep set: leaving the fixture VM up. Remove it with test/fixture.sh down"
  fi

  rm -rf "$WORK_DIR"
  exit $status
}
trap cleanup EXIT

wait_for() {
  local desc="$1"; shift
  local timeout="$1"; shift
  local waited=0
  until "$@" >/dev/null 2>&1; do
    sleep 2
    waited=$((waited + 2))
    if [[ $waited -ge $timeout ]]; then
      fail "timed out after ${timeout}s waiting for: $desc"
    fi
  done
}

awx() {  # awx <path> -> GET against the detected base path
  curl -sf -H "Authorization: Bearer ${AWX_TOKEN}" "${AWX_URL}${AWX_BASE}$1"
}

jqp() {  # jqp <python expression over `d`> ; reads JSON on stdin
  python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

# --- phase 0: preflight -----------------------------------------------
# Everything that would make a later assertion lie is checked here, so a
# misconfigured run fails in seconds instead of after an install.

log "phase 0: preflight"
for tool in kubectl curl python3; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

kubectl version -o json >/dev/null 2>&1 || fail "cannot reach the cluster - is KUBECONFIG pointed at the Supervisor?"
kubectl get namespace "$SUPERVISOR_NS" >/dev/null 2>&1 || fail "namespace $SUPERVISOR_NS does not exist"

# AWX/Tower and AAP <= 2.4 serve /api/v2; AAP 2.5+ moved the controller
# API to /api/controller/v2. The controller detects this itself - the
# harness has to do the same to talk to the instance directly.
AWX_BASE=""
for candidate in /api/v2 /api/controller/v2; do
  if curl -sf -H "Authorization: Bearer ${AWX_TOKEN}" "${AWX_URL}${candidate}/me/" >/dev/null 2>&1; then
    AWX_BASE="$candidate"
    break
  fi
done
[[ -n "$AWX_BASE" ]] || fail "cannot authenticate to $AWX_URL on /api/v2 or /api/controller/v2 - check AWX_URL and AWX_TOKEN"
log "AWX reachable at ${AWX_URL}${AWX_BASE}"

TEMPLATE_PATH="job_templates"
[[ "$TEMPLATE_TYPE" == "WorkflowTemplate" ]] && TEMPLATE_PATH="workflow_job_templates"
TMPL_JSON="$(awx "/${TEMPLATE_PATH}/?name=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$AWX_TEMPLATE")")" \
  || fail "listing templates from AWX"
# The ?name= filter is a field lookup rather than published API, so match
# the name here rather than trusting results[0] - the same reason the
# controller re-checks it.
TMPL_INFO="$(echo "$TMPL_JSON" | python3 -c "
import json,sys
d=json.load(sys.stdin)
want=sys.argv[1]
m=[t for t in d.get('results',[]) if t.get('name')==want]
if len(m)!=1:
    sys.exit(1)
print(m[0]['id'])
print(m[0].get('ask_limit_on_launch'))
print(m[0].get('ask_variables_on_launch'))
print(m[0].get('inventory') or '')
" "$AWX_TEMPLATE")" || fail "template '$AWX_TEMPLATE' not found in AWX, or the name is ambiguous"

TMPL_ID="$(echo "$TMPL_INFO" | sed -n 1p)"
TMPL_ASK_LIMIT="$(echo "$TMPL_INFO" | sed -n 2p)"
TMPL_INVENTORY="$(echo "$TMPL_INFO" | sed -n 4p)"
[[ "$TMPL_ASK_LIMIT" == "True" ]] || fail "template '$AWX_TEMPLATE' does not have Prompt on Launch enabled for Limit - the controller will refuse to launch it"
[[ -n "$TMPL_INVENTORY" ]] || fail "template '$AWX_TEMPLATE' has no inventory, so no host is created and there is nothing to assert"
log "template '$AWX_TEMPLATE' id=$TMPL_ID inventory=$TMPL_INVENTORY, Prompt on Launch for Limit is on"

if [[ $OWN_FIXTURE -eq 1 ]]; then
  log "no VM_LABEL given: bringing up the harness's own fixture VM"
  SUPERVISOR_NS="$SUPERVISOR_NS" AWX_URL="$AWX_URL" AWX_TOKEN="$AWX_TOKEN" AWX_TEMPLATE="$AWX_TEMPLATE" \
    "${HARNESS_DIR}/fixture.sh" up || fail "could not bring up the fixture VM"
fi

VM_COUNT="$(kubectl get virtualmachine -n "$SUPERVISOR_NS" -l "$VM_LABEL" -o json | jqp "len(d['items'])")"
[[ "$VM_COUNT" -ge 1 ]] || fail "no VirtualMachine in $SUPERVISOR_NS matches $VM_LABEL"
VM_NAME="$(kubectl get virtualmachine -n "$SUPERVISOR_NS" -l "$VM_LABEL" -o jsonpath='{.items[0].metadata.name}')"
VM_IP="$(kubectl get virtualmachine "$VM_NAME" -n "$SUPERVISOR_NS" -o jsonpath='{.status.network.primaryIP4}')"
VM_POWER="$(kubectl get virtualmachine "$VM_NAME" -n "$SUPERVISOR_NS" -o jsonpath='{.status.powerState}')"
[[ "$VM_POWER" == "PoweredOn" ]] || fail "VM $VM_NAME is $VM_POWER, not PoweredOn"
[[ -n "$VM_IP" ]] || fail "VM $VM_NAME reports no primaryIP4 yet"
log "$VM_COUNT VM(s) match $VM_LABEL; first is $VM_NAME at $VM_IP"

# --- phase 1: the installed service is the build under test -----------

log "phase 1: checking the installed service"
# Located by name rather than by label: config/deploy.yml carries
# app=ansible-supervisor on the pod template only, so the Deployment
# object itself has nothing to select on. Pods do have the label, which
# is what the log reads below use.
CTRL_SEL=(--field-selector metadata.name=ansible-supervisor-controller)
CTRL_NS="$(kubectl get deployment -A "${CTRL_SEL[@]}" -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)"
[[ -n "$CTRL_NS" ]] || fail "no ansible-supervisor-controller Deployment found - is the service installed?"
DEPLOY_COUNT="$(kubectl get deployment -A "${CTRL_SEL[@]}" -o json | jqp "len(d['items'])")"
[[ "$DEPLOY_COUNT" == "1" ]] || fail "found $DEPLOY_COUNT controller Deployments, expected exactly 1"
log "controller installed in namespace $CTRL_NS"

wait_for "controller Deployment Available" 180 bash -c \
  "[[ \$(kubectl get deployment ansible-supervisor-controller -n $CTRL_NS -o jsonpath='{.status.readyReplicas}') == 1 ]]"

RUNNING_IMAGE="$(kubectl get deployment ansible-supervisor-controller -n "$CTRL_NS" -o jsonpath='{.spec.template.spec.containers[0].image}')"
log "running image: $RUNNING_IMAGE"

# A tag here rather than a digest means the kbld override regressed: the
# image ships inside the imgpkg bundle, and a tag sends kapp-controller
# back to the registry on every deploy, which unpins it and breaks
# air-gapped installs.
[[ "$RUNNING_IMAGE" == *@sha256:* ]] || fail "the running image is not pinned by digest: $RUNNING_IMAGE"

if [[ -n "$EXPECT_IMAGE" ]]; then
  [[ "$RUNNING_IMAGE" == "$EXPECT_IMAGE" ]] \
    || fail "installed image is $RUNNING_IMAGE, expected $EXPECT_IMAGE - the release under test is not what is installed"
  log "installed image matches the release under test"
fi

for crd in awxconnections.field.vmware.com ansiblebindings.field.vmware.com; do
  kubectl wait --for=condition=Established --timeout=60s "crd/$crd" >/dev/null \
    || fail "CRD $crd is not Established"
done
log "CRDs established"

# The in-cluster startup path runs under the service's own ClusterRole,
# which the kind suite approximates with a minted token. A missing rule
# shows up here and nowhere else.
kubectl logs -n "$CTRL_NS" -l app=ansible-supervisor --tail=500 > "$WORK_DIR/startup.log" 2>/dev/null || true
grep -q "controller started successfully" "$WORK_DIR/startup.log" \
  || fail "controller did not report a successful start - see $WORK_DIR/startup.log"
if grep -qi "forbidden" "$WORK_DIR/startup.log"; then
  grep -i "forbidden" "$WORK_DIR/startup.log" | head -5
  fail "controller log contains Forbidden - the ClusterRole is missing a rule"
fi
VM_API="$(grep -o 'virtualmachine api: [^ ]*' "$WORK_DIR/startup.log" | tail -1 || true)"
log "started clean, no Forbidden. ${VM_API:-virtualmachine api not logged}"

# The resync period the installed service actually runs with, so the
# quiet-state check below waits the right number of passes rather than a
# guessed number of seconds.
RESYNC="$(kubectl get deployment ansible-supervisor-controller -n "$CTRL_NS" \
  -o jsonpath='{.spec.template.spec.containers[0].args}' \
  | grep -o 'resync-period=[0-9]*' | cut -d= -f2 || true)"
RESYNC="${RESYNC:-60}"
# The host check period likewise: it decides whether a child is expected
# to touch AWX during the idle window at all.
HOST_CHECK_PERIOD="$(kubectl get deployment ansible-supervisor-controller -n "$CTRL_NS" \
  -o jsonpath='{.spec.template.spec.containers[0].args}' \
  | grep -o 'host-check-period=[0-9]*' | cut -d= -f2 || true)"
HOST_CHECK_PERIOD="${HOST_CHECK_PERIOD:-600}"
log "resync period is ${RESYNC}s, host check period is ${HOST_CHECK_PERIOD}s"

# --- phase 2: a real run against a real AWX and a real VM -------------

log "phase 2: running a binding end to end"
CREATED=1

printf '%s' "$AWX_TOKEN" > "$WORK_DIR/token"
kubectl create secret generic "$SECRET" -n "$SUPERVISOR_NS" --from-file=token="$WORK_DIR/token" >/dev/null

kubectl apply -f - >/dev/null <<EOF
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: ${CONN}
  namespace: ${SUPERVISOR_NS}
spec:
  url: "${AWX_URL}"
  secretRef: "${SECRET}"
EOF

wait_for "AWXConnection Ready" 120 bash -c \
  "[[ \$(kubectl get awxconnection $CONN -n $SUPERVISOR_NS -o jsonpath='{.status.ready}') == true ]]"

DETECTED="$(kubectl get awxconnection "$CONN" -n "$SUPERVISOR_NS" -o jsonpath='{.status.apiBasePath}')"
[[ "$DETECTED" == "$AWX_BASE" ]] \
  || fail "controller detected API base path '$DETECTED', harness found '$AWX_BASE'"
log "AWXConnection Ready, detected $DETECTED"

kubectl apply -f - >/dev/null <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: ${BINDING}
  namespace: ${SUPERVISOR_NS}
spec:
  vmSelector:
    ${VM_LABEL%%=*}: "${VM_LABEL#*=}"
  awxConnectionRef: ${CONN}
  template:
    name: "${AWX_TEMPLATE}"
    type: ${TEMPLATE_TYPE}
EOF

# A real playbook against a real VM over SSH, so this is minutes, not
# seconds. A failure here is as likely to be the playbook or the Machine
# credential as the controller - the log dump on exit says which.
wait_for "every matched VM reaches Succeeded" 900 bash -c \
  "kubectl get ansiblebindingvm -n $SUPERVISOR_NS -l field.vmware.com/binding=$BINDING -o json \
   | python3 -c \"import json,sys; v=json.load(sys.stdin).get('items',[]); sys.exit(0 if v and all(x.get('status',{}).get('phase')=='Succeeded' for x in v) else 1)\""

TRACKED="$(children -o json | jqp "len(d['items'])")"
[[ "$TRACKED" == "$VM_COUNT" ]] \
  || fail "binding has $TRACKED child(ren) but $VM_COUNT VM(s) match the selector - fan-out is wrong"
log "all $TRACKED matched VM(s) succeeded"

# The rollup has to agree with the children it is a rollup of.
SUMMARY="$(kubectl get ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" -o jsonpath='{.status.summary.total}/{.status.summary.succeeded}')"
[[ "$SUMMARY" == "${VM_COUNT}/${VM_COUNT}" ]] \
  || fail "expected the rollup to read ${VM_COUNT}/${VM_COUNT} (total/succeeded), got $SUMMARY"
log "status.summary agrees with the children: $SUMMARY"

HOST_ID="$(child_field "$VM_NAME" awxHostID)"
[[ -n "$HOST_ID" ]] || fail "no awxHostID recorded for $VM_NAME"

# Read the host back out of the real AWX rather than trusting status: the
# point of the check is that what the controller says it wrote is what
# AWX actually holds.
HOST_JSON="$(awx "/hosts/${HOST_ID}/")" || fail "AWX has no host $HOST_ID, but status says it does"
HOST_IP="$(echo "$HOST_JSON" | python3 -c "
import json,sys
h=json.load(sys.stdin)
v=h.get('variables') or '{}'
try:
    print(json.loads(v).get('ansible_host',''))
except Exception:
    print('')
")"
[[ "$HOST_IP" == "$VM_IP" ]] \
  || fail "AWX host $HOST_ID has ansible_host='$HOST_IP', VM reports '$VM_IP'"
log "AWX host $HOST_ID exists with ansible_host=$HOST_IP, matching the live VM"

# --- phase 3: an idle binding must cost nothing -----------------------
# The reason this release exists. status.vms[].lastUpdated used to be
# stamped every pass, so the object changed on every reconcile whether or
# not anything happened, and every binding wrote to etcd once per resync
# forever. Verified here rather than reasoned about.

log "phase 3: checking an idle binding goes quiet (${RESYNC}s resync)"
RV_BEFORE="$(kubectl get ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" -o jsonpath='{.metadata.resourceVersion}')"
CHECKS_BEFORE="$(child_field "$VM_NAME" lastHostCheck)"
HOST_MODIFIED_BEFORE="$(echo "$HOST_JSON" | jqp "d.get('modified','')")"

IDLE=$(( RESYNC * 3 + 10 ))
log "waiting ${IDLE}s (3+ resync passes) with nothing changing"
sleep "$IDLE"

RV_AFTER="$(kubectl get ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" -o jsonpath='{.metadata.resourceVersion}')"
if [[ "$RV_BEFORE" != "$RV_AFTER" ]]; then
  kubectl get ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" -o yaml > "$WORK_DIR/idle-binding.yaml"
  fail "an idle binding still wrote: resourceVersion $RV_BEFORE -> $RV_AFTER after ${IDLE}s. See $WORK_DIR/idle-binding.yaml"
fi
log "resourceVersion unchanged across 3+ resyncs: an idle binding writes nothing"

HOST_MODIFIED_AFTER="$(awx "/hosts/${HOST_ID}/" | jqp "d.get('modified','')")"
[[ "$HOST_MODIFIED_BEFORE" == "$HOST_MODIFIED_AFTER" ]] \
  || fail "AWX host $HOST_ID was rewritten while nothing changed: $HOST_MODIFIED_BEFORE -> $HOST_MODIFIED_AFTER"
log "AWX host untouched across the same window: steady state writes nothing to AWX either"

# A child is not quite as quiet as its binding: it stamps
# status.lastHostCheck each time it reconciles its inventory host against
# AWX. What it must not do is stamp it every resync - that would mean the
# host check is still happening on every pass, which is the traffic this
# release exists to remove.
CHILD_CHECKS="$(children -o jsonpath="{.items[*].status.lastHostCheck}" | tr ' ' '\n' | sort -u | wc -l)"
CHECKS_AFTER="$(child_field "$VM_NAME" lastHostCheck)"
if [[ "$HOST_CHECK_PERIOD" -gt "$(( RESYNC * 2 ))" && "$CHECKS_BEFORE" != "$CHECKS_AFTER" ]]; then
  fail "the host check ran during the idle window: lastHostCheck $CHECKS_BEFORE -> $CHECKS_AFTER, with a ${HOST_CHECK_PERIOD}s period"
fi
log "child host check stayed on its ${HOST_CHECK_PERIOD}s period across the idle window (${CHILD_CHECKS} distinct timestamp(s))"

# --- phase 4: teardown removes what it created ------------------------

log "phase 4: checking cleanup"
HOST_CREATED="$(child_field "$VM_NAME" awxHostCreated)"

kubectl delete ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" --timeout=180s >/dev/null \
  || fail "the binding did not delete cleanly - the finalizer never released"
log "binding deleted, finalizer released"

if [[ "$HOST_CREATED" == "true" ]]; then
  if awx "/hosts/${HOST_ID}/" >/dev/null 2>&1; then
    fail "AWX host $HOST_ID was created by the controller but survived cleanup"
  fi
  log "AWX host $HOST_ID removed from the inventory"
else
  awx "/hosts/${HOST_ID}/" >/dev/null 2>&1 \
    || fail "AWX host $HOST_ID pre-existed and was adopted, but cleanup deleted it anyway"
  log "adopted host $HOST_ID left in place, as it must be"
fi

kubectl delete awxconnection "$CONN" -n "$SUPERVISOR_NS" --timeout=60s >/dev/null
kubectl delete secret "$SECRET" -n "$SUPERVISOR_NS" >/dev/null
CREATED=0

log "ALL CHECKS PASSED against $RUNNING_IMAGE"
