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
# Usage: fill in .env at the repo root (see .env.example) and run
# `make verify-supervisor`. Everything below can equally be exported by
# hand, and an exported value overrides .env:
#
#   export KUBECONFIG=/path/to/supervisor.kubeconfig
#   export SUPERVISOR_NS=my-namespace
#   export AWX_URL=https://awx.example.com
#   export AWX_TOKEN=...
#   export AWX_TEMPLATE="Configure Webserver"
#   export AWX_DEPROVISION_TEMPLATE="Deregister Host"
#   export VM_LABEL=app=webserver
#   make verify-supervisor
#
# The template must have Prompt on Launch enabled for Limit (and for
# Variables), and VM_LABEL must match at least one powered-on VM that
# already reports an IP. Both are checked before anything is created.
#
# AWX_DEPROVISION_TEMPLATE is the onDeleted hook's template, and adds
# phase 4. Checking that hook live means deleting the VM it is bound to,
# so it runs only against the harness's own fixture: with VM_LABEL set,
# the VM belongs to whoever set it and the phase is skipped.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Settings come from .env at the repo root when it exists; the
# environment still wins over it. See .env.example.
source "$HARNESS_DIR/lib/dotenv.sh"
load_dotenv
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
# The onDeleted hook's template. Optional: unset, phase 4 is skipped
# rather than failing, because a lab that has not got a teardown template
# can still gate everything else.
AWX_DEPROVISION_TEMPLATE="${AWX_DEPROVISION_TEMPLATE:-}"
DEPROVISION_TEMPLATE_TYPE="${DEPROVISION_TEMPLATE_TYPE:-JobTemplate}"
# Bounds the hook itself. Past it the controller releases the finalizer
# anyway, so the phase waits a little longer than this before calling it
# a failure.
HOOK_TIMEOUT_SECONDS="${HOOK_TIMEOUT_SECONDS:-600}"
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
# Set once phase 4 has destroyed the VM under test, so phase 5 does not
# go looking for what the hook already cleaned up.
FIXTURE_DELETED=0
# The second binding phase 4 runs under cleanupPolicy: Retain, and the
# host it deliberately leaves behind. Both are the harness's to remove:
# Retain means the controller will not.
RETAIN_BINDING=""
RETAIN_HOST_ID=""
# The Retain scenario gets its own fixture VM: single-claim-per-VM means
# two bindings can no longer share $VM_NAME, so it can no longer double
# as the Delete-policy VM and the Retain-policy VM at once.
RETAIN_FIXTURE_NAME=""
RETAIN_FIXTURE_CREATED=0
# Phase 2b's binding: it selects the same VM as $BINDING to prove the
# one-claim-per-VM rule live. It never owns a child, so it needs no host
# cleanup of its own - just deleting the object.
CONFLICT_BINDING=""

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
    # Retain means the controller deliberately leaves this host in the
    # inventory, so the harness is the only thing that will remove it.
    [[ -n "$RETAIN_BINDING" ]] && kubectl delete ansiblebinding "$RETAIN_BINDING" -n "$SUPERVISOR_NS" --timeout=120s >/dev/null 2>&1 || true
    [[ -n "$CONFLICT_BINDING" ]] && kubectl delete ansiblebinding "$CONFLICT_BINDING" -n "$SUPERVISOR_NS" --timeout=60s >/dev/null 2>&1 || true
    if [[ -n "$RETAIN_HOST_ID" && -n "${AWX_BASE:-}" ]]; then
      awx_delete "/hosts/${RETAIN_HOST_ID}/" >/dev/null 2>&1 || true
    fi
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
  if [[ $RETAIN_FIXTURE_CREATED -eq 1 && $KEEP -eq 0 ]]; then
    FIXTURE_NAME="$RETAIN_FIXTURE_NAME" "${HARNESS_DIR}/fixture.sh" down || true
  elif [[ $RETAIN_FIXTURE_CREATED -eq 1 ]]; then
    log "--keep set: leaving the Retain fixture VM $RETAIN_FIXTURE_NAME up. Remove it with FIXTURE_NAME=$RETAIN_FIXTURE_NAME test/fixture.sh down"
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

awx_delete() {  # awx_delete <path> - only ever used on what the harness made
  curl -sf -X DELETE -H "Authorization: Bearer ${AWX_TOKEN}" "${AWX_URL}${AWX_BASE}$1"
}

# The variables an AWX host carries, as JSON on one line. Reading them
# back out of AWX is the point: what the controller says it wrote is not
# evidence that AWX holds it.
host_variables() {  # host_variables <host id>
  awx "/hosts/$1/" | jqp "json.dumps(json.loads(d.get('variables') or '{}'))"
}

jqp() {  # jqp <python expression over `d`> ; reads JSON on stdin
  python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

urlquote() { python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$1"; }

# template_info <name> <JobTemplate|WorkflowTemplate> -> id, ask_limit,
# ask_variables, inventory, one per line. The ?name= filter is a field
# lookup rather than published API, so the name is matched here rather
# than trusting results[0] - the same reason the controller re-checks it.
template_info() {
  local name="$1" type="$2" path="job_templates"
  [[ "$type" == "WorkflowTemplate" ]] && path="workflow_job_templates"
  awx "/${path}/?name=$(urlquote "$name")" | python3 -c "
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
" "$name"
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

TMPL_INFO="$(template_info "$AWX_TEMPLATE" "$TEMPLATE_TYPE")" \
  || fail "template '$AWX_TEMPLATE' not found in AWX, or the name is ambiguous"
TMPL_ID="$(echo "$TMPL_INFO" | sed -n 1p)"
TMPL_ASK_LIMIT="$(echo "$TMPL_INFO" | sed -n 2p)"
TMPL_INVENTORY="$(echo "$TMPL_INFO" | sed -n 4p)"
[[ "$TMPL_ASK_LIMIT" == "True" ]] || fail "template '$AWX_TEMPLATE' does not have Prompt on Launch enabled for Limit - the controller will refuse to launch it"
[[ -n "$TMPL_INVENTORY" ]] || fail "template '$AWX_TEMPLATE' has no inventory, so no host is created and there is nothing to assert"
log "template '$AWX_TEMPLATE' id=$TMPL_ID inventory=$TMPL_INVENTORY, Prompt on Launch for Limit is on"

# --- the onDeleted hook, if there is one to check ---------------------
# The hook fires only for a VM that is really gone, so checking it live
# means deleting the VM under test. That is only ever the harness's own
# fixture: VM_LABEL names a VM someone else manages, and destroying it to
# prove a hook works would make the gate the thing that caused the
# outage.
RUN_HOOK=0
HOOK_TMPL_ID=""
if [[ -z "$AWX_DEPROVISION_TEMPLATE" ]]; then
  log "AWX_DEPROVISION_TEMPLATE is unset: skipping phase 4, the onDeleted hook"
elif [[ $OWN_FIXTURE -eq 0 ]]; then
  log "VM_LABEL names a VM this harness does not own: skipping phase 4, which deletes the VM to fire the hook"
else
  HOOK_INFO="$(template_info "$AWX_DEPROVISION_TEMPLATE" "$DEPROVISION_TEMPLATE_TYPE")" \
    || fail "deprovision template '$AWX_DEPROVISION_TEMPLATE' not found in AWX, or the name is ambiguous"
  HOOK_TMPL_ID="$(echo "$HOOK_INFO" | sed -n 1p)"
  HOOK_INVENTORY="$(echo "$HOOK_INFO" | sed -n 4p)"
  [[ "$(echo "$HOOK_INFO" | sed -n 2p)" == "True" ]] \
    || fail "deprovision template '$AWX_DEPROVISION_TEMPLATE' does not have Prompt on Launch enabled for Limit - the hook would run against the whole inventory, and the controller refuses it"
  # The hook always carries asb_* extra vars, so unlike the provisioning
  # template this one has no useDefaultLimit-style escape: without
  # Variables it cannot launch at all.
  [[ "$(echo "$HOOK_INFO" | sed -n 3p)" == "True" ]] \
    || fail "deprovision template '$AWX_DEPROVISION_TEMPLATE' does not have Prompt on Launch enabled for Variables - the hook passes asb_hook, asb_vm_name, asb_binding and asb_last_known_ip, and the controller refuses a template that would drop them"
  [[ "$HOOK_INVENTORY" == "$TMPL_INVENTORY" ]] \
    || fail "deprovision template '$AWX_DEPROVISION_TEMPLATE' runs against inventory ${HOOK_INVENTORY:-none}, but the host being torn down lives in $TMPL_INVENTORY - the hook's limit would match nothing"
  RUN_HOOK=1
  log "deprovision template '$AWX_DEPROVISION_TEMPLATE' id=$HOOK_TMPL_ID inventory=$HOOK_INVENTORY, prompts for Limit and Variables"
fi

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

# The hook is declared here rather than patched in later: phases 2 and 3
# are unaffected by its presence - it fires on a deleted VM and nothing
# else - and a binding that carries it from the start is the one a user
# would actually write.
ONDELETED=""
if [[ $RUN_HOOK -eq 1 ]]; then
  ONDELETED="$(printf '  onDeleted:\n    template:\n      name: "%s"\n      type: %s\n    timeoutSeconds: %s' \
    "$AWX_DEPROVISION_TEMPLATE" "$DEPROVISION_TEMPLATE_TYPE" "$HOOK_TIMEOUT_SECONDS")"
fi

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
${ONDELETED}
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

# --- phase 2b: a second binding on the same VM must conflict, not double-launch --
# The single-binding-per-VM claim (controller/ansiblebinding.go) is a
# unit-tested arbitration, but only a real API server proves the create
# race actually decides ownership: $BINDING already holds the VM's
# canonical AnsibleBindingVM, so a second binding selecting the same VM
# has to lose the claim rather than get its own child and its own job.

log "phase 2b: a second binding claiming $VM_NAME must conflict"
CONFLICT_BINDING="${PREFIX}-conflict"
kubectl apply -f - >/dev/null <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: ${CONFLICT_BINDING}
  namespace: ${SUPERVISOR_NS}
spec:
  vmSelector:
    ${VM_LABEL%%=*}: "${VM_LABEL#*=}"
  awxConnectionRef: ${CONN}
  template:
    name: "${AWX_TEMPLATE}"
    type: ${TEMPLATE_TYPE}
EOF

wait_for "the conflicting binding to report state Conflict" 60 bash -c \
  "[[ \$(kubectl get ansiblebinding $CONFLICT_BINDING -n $SUPERVISOR_NS -o jsonpath='{.status.state}') == Conflict ]]"

CONFLICT_SUMMARY="$(kubectl get ansiblebinding "$CONFLICT_BINDING" -n "$SUPERVISOR_NS" \
  -o jsonpath='{.status.summary.total}/{.status.summary.conflicted}')"
[[ "$CONFLICT_SUMMARY" == "1/1" ]] \
  || fail "expected the conflicting binding's summary to read 1/1 (total/conflicted), got $CONFLICT_SUMMARY"

CONFLICT_OWNER="$(kubectl get ansiblebinding "$CONFLICT_BINDING" -n "$SUPERVISOR_NS" \
  -o jsonpath='{.status.summary.conflictedVMs[0]}')"
[[ "$CONFLICT_OWNER" == *"$VM_NAME"* && "$CONFLICT_OWNER" == *"$BINDING"* ]] \
  || fail "conflictedVMs should name $VM_NAME owned by $BINDING, got '$CONFLICT_OWNER'"
log "conflicting binding reports state=Conflict, summary=$CONFLICT_SUMMARY, owner: $CONFLICT_OWNER"

CONFLICT_CHILDREN="$(kubectl get ansiblebindingvm -n "$SUPERVISOR_NS" \
  -l "field.vmware.com/binding=${CONFLICT_BINDING}" -o json | jqp "len(d['items'])")"
[[ "$CONFLICT_CHILDREN" == "0" ]] \
  || fail "the conflicting binding should own no AnsibleBindingVM, found $CONFLICT_CHILDREN"
log "conflicting binding created no child - the losing side launched no job"

# The winner has to come through this untouched: a losing challenger must
# not perturb the binding that actually holds the claim.
WINNER_STATE="$(kubectl get ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" -o jsonpath='{.status.state}')"
[[ "$WINNER_STATE" != "Conflict" ]] \
  || fail "the owning binding $BINDING was pushed into Conflict by a later challenger"
WINNER_SUMMARY="$(kubectl get ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" -o jsonpath='{.status.summary.total}/{.status.summary.succeeded}')"
[[ "$WINNER_SUMMARY" == "${VM_COUNT}/${VM_COUNT}" ]] \
  || fail "the owning binding's summary regressed to $WINNER_SUMMARY after the conflict"
log "owning binding $BINDING unaffected: state=$WINNER_STATE summary=$WINNER_SUMMARY"

kubectl delete ansiblebinding "$CONFLICT_BINDING" -n "$SUPERVISOR_NS" --timeout=60s >/dev/null \
  || fail "the conflicting binding did not delete cleanly"
CONFLICT_BINDING=""
log "conflicting binding deleted"

# --- phase 3: an idle binding must cost nothing -----------------------
# The bug 1.0.1 fixed: a per-VM lastUpdated was stamped every pass, so
# the object changed on every reconcile whether or not anything had
# happened, and every binding wrote to etcd once per resync forever.
# Verified here rather than reasoned about.

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

# --- phase 4: deleting a VM runs its onDeleted hook -------------------
# The one path e2e.sh can only fake. Here the VirtualMachine is really
# destroyed by vm-operator, the child is really collected by the garbage
# collector on the way out, and the playbook really runs in AWX - against
# a guest that no longer exists, which is why the controller pins the
# host to the control node before it launches.

HOST_NAME="$(child_field "$VM_NAME" awxHostName)"
HOST_CREATED="$(child_field "$VM_NAME" awxHostCreated)"

if [[ $RUN_HOOK -eq 1 ]]; then
  log "phase 4: checking the onDeleted hook on a real VM delete"
  [[ -n "$HOST_NAME" ]] || fail "no awxHostName recorded for $VM_NAME, so there is nothing for the hook to limit to"

  # A workflow's runs are their own resource, so both the listing and the
  # per-job reads below have to follow the template's type.
  HOOK_JOB_PATH="/jobs"
  HOOK_JOBS="/jobs/?job_template=${HOOK_TMPL_ID}"
  if [[ "$DEPROVISION_TEMPLATE_TYPE" == "WorkflowTemplate" ]]; then
    HOOK_JOB_PATH="/workflow_jobs"
    HOOK_JOBS="/workflow_jobs/?workflow_job_template=${HOOK_TMPL_ID}"
  fi
  hook_latest() { awx "${HOOK_JOBS}&order_by=-id&page_size=1" | jqp "(d['results'][0]['id'] if d.get('results') else 0)"; }

  # Whatever the hook template has run before this gate started is the
  # floor: the assertion is that the delete launches something new, not
  # that the template has ever been used.
  HOOK_BEFORE="$(hook_latest)"
  log "hook template's newest job before the delete is ${HOOK_BEFORE:-none}"

  hook_job_for_limit() {  # hook_job_for_limit <host name> -> the job id, if there is one yet
    awx "${HOOK_JOBS}&order_by=-id&page_size=20" | python3 -c "
import json,sys
want, floor = sys.argv[1], int(sys.argv[2])
for j in json.load(sys.stdin).get('results', []):
    if j.get('id', 0) > floor and (j.get('limit') or '') == want:
        print(j['id'])
        break
" "$1" "${HOOK_BEFORE:-0}"
  }

  # A second binding, under cleanupPolicy: Retain, but on its own fixture
  # VM: single-claim-per-VM (controller/ansiblebinding.go) means $BINDING
  # already owns $VM_NAME, so a second binding can no longer share it.
  # Each policy still gets its own real VM deletion and its own hook
  # launch - they just no longer land in the same delete event, so there
  # is no shared limit to tell them apart by.
  #
  # The pin is the reason cleanupPolicy: Retain matters here.
  # ansible_connection: local is set so a playbook that forgets
  # delegate_to cannot reach an address that may already have been
  # re-leased - but on a host that outlives the hook, leaving it there
  # would silently send the NEXT provisioning run to the AWX control node
  # instead of the machine. Unit tests cover the restore; nothing else has
  # ever watched a real AWX give the host back.
  RETAIN_BINDING="${PREFIX}-retain"
  RETAIN_FIXTURE_NAME="${PREFIX}-retain-vm"
  RETAIN_FIXTURE_LABEL="app=${RETAIN_FIXTURE_NAME}"
  log "bringing up a second fixture VM for the Retain scenario: $RETAIN_FIXTURE_NAME"
  SUPERVISOR_NS="$SUPERVISOR_NS" AWX_URL="$AWX_URL" AWX_TOKEN="$AWX_TOKEN" AWX_TEMPLATE="$AWX_TEMPLATE" \
    FIXTURE_NAME="$RETAIN_FIXTURE_NAME" FIXTURE_LABEL="$RETAIN_FIXTURE_LABEL" \
    "${HARNESS_DIR}/fixture.sh" up || fail "could not bring up the Retain scenario's fixture VM"
  RETAIN_FIXTURE_CREATED=1
  RETAIN_VM_NAME="$RETAIN_FIXTURE_NAME"
  RETAIN_HOST_NAME="$RETAIN_FIXTURE_NAME"

  log "creating $RETAIN_BINDING (cleanupPolicy: Retain) on $RETAIN_VM_NAME"
  kubectl apply -f - >/dev/null <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: ${RETAIN_BINDING}
  namespace: ${SUPERVISOR_NS}
spec:
  vmSelector:
    ${RETAIN_FIXTURE_LABEL%%=*}: "${RETAIN_FIXTURE_LABEL#*=}"
  awxConnectionRef: ${CONN}
  cleanupPolicy: Retain
  template:
    name: "${AWX_TEMPLATE}"
    type: ${TEMPLATE_TYPE}
${ONDELETED}
EOF

  retain_children() { kubectl get ansiblebindingvm -n "$SUPERVISOR_NS" -l "field.vmware.com/binding=${RETAIN_BINDING}" "$@"; }
  retain_succeeded() {
    [[ "$(retain_children -o jsonpath="{.items[?(@.spec.vmName=='$RETAIN_VM_NAME')].status.phase}")" == "Succeeded" ]]
  }
  wait_for "the Retain binding to run against $RETAIN_VM_NAME" 900 retain_succeeded

  RETAIN_HOST_ID="$(retain_children -o jsonpath="{.items[?(@.spec.vmName=='$RETAIN_VM_NAME')].status.awxHostID}")"
  RETAIN_CHILD="$(retain_children -o jsonpath="{.items[?(@.spec.vmName=='$RETAIN_VM_NAME')].metadata.name}")"
  [[ -n "$RETAIN_HOST_ID" ]] || fail "the Retain binding recorded no awxHostID for $RETAIN_VM_NAME"
  # What the host looked like before any hook touched it is the thing the
  # restore has to reproduce, so it is captured rather than assumed.
  RETAIN_VARS_BEFORE="$(host_variables "$RETAIN_HOST_ID")"
  echo "$RETAIN_VARS_BEFORE" | python3 -c "
import json,sys
v=json.load(sys.stdin)
sys.exit(0 if 'ansible_connection' not in v else 1)
" || fail "host $RETAIN_HOST_ID already has an ansible_connection before the hook ran: $RETAIN_VARS_BEFORE"
  log "Retain binding host $RETAIN_HOST_ID ($RETAIN_HOST_NAME) has no ansible_connection to start with"

  # The child is deleted by the garbage collector when its owning
  # VirtualMachine goes, so nothing here touches the child directly -
  # that is the path a real deletion takes.
  CHILD_NAME="$(children -o jsonpath="{.items[?(@.spec.vmName=='$VM_NAME')].metadata.name}")"
  [[ -n "$CHILD_NAME" ]] || fail "could not find the AnsibleBindingVM for $VM_NAME"
  log "deleting VirtualMachine $VM_NAME (child $CHILD_NAME)"
  kubectl delete virtualmachine "$VM_NAME" -n "$SUPERVISOR_NS" --wait=false >/dev/null

  # "the newest job on this template" is not necessarily this delete's
  # job if the harness or someone else launched the hook template
  # meanwhile, so the limit - the host each hook is scoped to - is what
  # actually identifies it.
  hook_launched() { [[ -n "$(hook_job_for_limit "$HOST_NAME")" ]]; }
  wait_for "the hook to launch a job in AWX" 300 hook_launched
  HOOK_JOB="$(hook_job_for_limit "$HOST_NAME")"
  log "hook launched job $HOOK_JOB on '$AWX_DEPROVISION_TEMPLATE'"

  HOOK_JOB_JSON="$(awx "${HOOK_JOB_PATH}/${HOOK_JOB}/")" || fail "AWX has no job $HOOK_JOB"

  # A hook that lost its limit would deregister every host in the
  # inventory rather than the one VM that went away. Re-read here rather
  # than taken on trust from the lookup above, which searched a listing.
  HOOK_LIMIT="$(echo "$HOOK_JOB_JSON" | jqp "d.get('limit','')")"
  [[ "$HOOK_LIMIT" == "$HOST_NAME" ]] \
    || fail "the hook was not scoped to its own host: limit='$HOOK_LIMIT', expected '$HOST_NAME'"

  # The playbook's whole job is to act on an external record, and these
  # are what tell it which record. They have to survive the launch.
  echo "$HOOK_JOB_JSON" | python3 -c "
import json,sys
job=json.load(sys.stdin)
raw=job.get('extra_vars') or '{}'
try:
    got=json.loads(raw)
except Exception:
    print('extra_vars is not JSON: %r' % raw); sys.exit(1)
want={'asb_hook':'onDeleted','asb_vm_name':sys.argv[1],'asb_binding':sys.argv[2],'asb_last_known_ip':sys.argv[3]}
bad={k:(got.get(k),v) for k,v in want.items() if got.get(k)!=v}
if bad:
    print('extra_vars mismatch (got, want): %s' % bad); sys.exit(1)
" "$VM_NAME" "$BINDING" "$VM_IP" || fail "the hook job did not carry the asb_* extra vars the playbook needs"
  log "hook job $HOOK_JOB carries limit=$HOOK_LIMIT and asb_vm_name/asb_binding/asb_last_known_ip=$VM_IP"

  # The guest is destroyed by now and its address may already be
  # re-leased, so the run has to execute on the control node instead. The
  # host is gone once the hook is terminal, so this is only assertable
  # while the job is still running - on a fast playbook it is reported
  # rather than failed.
  HOOK_STATE="$(echo "$HOOK_JOB_JSON" | jqp "d.get('status','')")"
  HOST_VARS="$(awx "/hosts/${HOST_ID}/" 2>/dev/null | jqp "d.get('variables','')" || true)"
  if [[ -n "$HOST_VARS" ]]; then
    echo "$HOST_VARS" | python3 -c "
import json,sys
v=json.loads(sys.stdin.read().strip() or '{}')
sys.exit(0 if v.get('ansible_connection')=='local' else 1)
" || fail "the host was not pinned to the control node before the hook ran: $HOST_VARS"
    log "host $HOST_ID still in the inventory and pinned to the control node while the hook runs"
  else
    log "hook job was already $HOOK_STATE and its host cleaned up before the pin could be sampled"
  fi

  # Past the hook's own timeout the controller releases the finalizer
  # regardless, so waiting materially longer than that distinguishes a
  # hook that failed from one that was never given a chance.
  HOOK_WAIT=$(( HOOK_TIMEOUT_SECONDS + 120 ))
  if [[ "$HOST_CREATED" == "true" ]]; then
    host_gone() { ! awx "/hosts/${HOST_ID}/" >/dev/null 2>&1; }
    wait_for "the inventory host to be removed once the hook is terminal" "$HOOK_WAIT" host_gone
    log "host $HOST_ID removed after the hook, not before it"
  fi

  child_gone() { [[ -z "$(kubectl get ansiblebindingvm "$CHILD_NAME" -n "$SUPERVISOR_NS" --ignore-not-found -o name)" ]]; }
  wait_for "the child to finish finalizing" "$HOOK_WAIT" child_gone
  log "child $CHILD_NAME finalized and collected"

  # The hook has to have actually succeeded, not merely finished: a
  # failed playbook still releases the finalizer, so the object going
  # away proves nothing on its own.
  HOOK_FINAL="$(awx "${HOOK_JOB_PATH}/${HOOK_JOB}/" | jqp "d.get('status','')")"
  [[ "$HOOK_FINAL" == "successful" ]] \
    || fail "hook job $HOOK_JOB ended '$HOOK_FINAL' - see ${AWX_URL}/#/jobs/playbook/${HOOK_JOB}"

  # The child took its status with it, so the record of what the teardown
  # did has to outlive it.
  wait_for "the outcome to be recorded on the binding" 60 bash -c \
    "kubectl get events -n $SUPERVISOR_NS --field-selector involvedObject.name=$BINDING -o jsonpath='{.items[*].reason}' | grep -q DeprovisionHook"
  log "hook succeeded, host removed, outcome recorded on $BINDING"

  # --- a second delete, on the second VM, under cleanupPolicy: Retain --
  # The host survives, so everything the hook did to it has to be undone
  # rather than deleted along with it.
  log "deleting VirtualMachine $RETAIN_VM_NAME (child $RETAIN_CHILD)"
  kubectl delete virtualmachine "$RETAIN_VM_NAME" -n "$SUPERVISOR_NS" --wait=false >/dev/null

  retain_hook_launched() { [[ -n "$(hook_job_for_limit "$RETAIN_HOST_NAME")" ]]; }
  wait_for "the Retain binding's hook to launch" 300 retain_hook_launched
  RETAIN_JOB="$(hook_job_for_limit "$RETAIN_HOST_NAME")"
  log "the Retain binding's hook launched job $RETAIN_JOB, limited to $RETAIN_HOST_NAME"

  retain_child_gone() { [[ -z "$(kubectl get ansiblebindingvm "$RETAIN_CHILD" -n "$SUPERVISOR_NS" --ignore-not-found -o name)" ]]; }
  wait_for "the Retain binding's child to finish finalizing" "$HOOK_WAIT" retain_child_gone

  RETAIN_FINAL="$(awx "${HOOK_JOB_PATH}/${RETAIN_JOB}/" | jqp "d.get('status','')")"
  [[ "$RETAIN_FINAL" == "successful" ]] \
    || fail "the Retain binding's hook job $RETAIN_JOB ended '$RETAIN_FINAL' - see ${AWX_URL}/#/jobs/playbook/${RETAIN_JOB}"

  awx "/hosts/${RETAIN_HOST_ID}/" >/dev/null 2>&1 \
    || fail "cleanupPolicy: Retain still removed host $RETAIN_HOST_ID after the hook ran against it"

  # Byte for byte what it was before the hook: the pin is off, and
  # nothing else the playbook or the controller did to it stuck either.
  # An ansible_connection left behind here is the bug this phase exists
  # for - the next provisioning run against this host would execute on
  # the AWX control node instead of the machine.
  RETAIN_VARS_AFTER="$(host_variables "$RETAIN_HOST_ID")"
  python3 -c "
import json,sys
before, after = json.loads(sys.argv[1]), json.loads(sys.argv[2])
if 'ansible_connection' in after:
    print('the hook left ansible_connection=%r on the retained host' % after['ansible_connection'])
    sys.exit(1)
if before != after:
    print('retained host variables changed: before=%s after=%s' % (sys.argv[1], sys.argv[2]))
    sys.exit(1)
" "$RETAIN_VARS_BEFORE" "$RETAIN_VARS_AFTER" \
    || fail "the retained host was not handed back as it was found"
  log "retained host $RETAIN_HOST_ID kept, and its variables are exactly what they were before the hook: $RETAIN_VARS_AFTER"

  wait_for "the Retain binding to record its outcome" 60 bash -c \
    "kubectl get events -n $SUPERVISOR_NS --field-selector involvedObject.name=$RETAIN_BINDING -o jsonpath='{.items[*].reason}' | grep -q DeprovisionHook"

  # Deleting the binding itself must not take the host either - the same
  # policy, on the other path that reaches it.
  kubectl delete ansiblebinding "$RETAIN_BINDING" -n "$SUPERVISOR_NS" --timeout=180s >/dev/null \
    || fail "the Retain binding did not delete cleanly - the finalizer never released"
  RETAIN_BINDING=""
  awx "/hosts/${RETAIN_HOST_ID}/" >/dev/null 2>&1 \
    || fail "deleting the Retain binding removed host $RETAIN_HOST_ID, which cleanupPolicy: Retain says to keep"
  log "Retain binding deleted, host $RETAIN_HOST_ID still in the inventory"

  # Removed here rather than left to the exit trap: a clean run clears
  # CREATED once its own objects are gone, so the trap's cleanup block is
  # deliberately skipped on success. The trap still catches this host if
  # the phase fails before this point.
  if [[ $KEEP -eq 0 ]]; then
    awx_delete "/hosts/${RETAIN_HOST_ID}/" >/dev/null 2>&1 \
      || fail "could not remove the retained host $RETAIN_HOST_ID that this phase deliberately left in AWX - delete it by hand"
    log "retained host $RETAIN_HOST_ID removed: it was the harness's to clean up, not the controller's"
    RETAIN_HOST_ID=""
  else
    log "--keep set: leaving retained host $RETAIN_HOST_ID in the inventory"
  fi

  # The VM and its host are gone, so phase 5 has nothing left to assert
  # about them.
  FIXTURE_DELETED=1
fi

# --- phase 5: teardown removes what it created ------------------------

log "phase 5: checking cleanup"

kubectl delete ansiblebinding "$BINDING" -n "$SUPERVISOR_NS" --timeout=180s >/dev/null \
  || fail "the binding did not delete cleanly - the finalizer never released"
log "binding deleted, finalizer released"

if [[ $FIXTURE_DELETED -eq 1 ]]; then
  # Phase 4 already deleted the VM, and with it the host and the child.
  # Asserting the host here would be asserting the hook's cleanup twice.
  log "the VM was deleted in phase 4: its host was already checked there"
elif [[ "$HOST_CREATED" == "true" ]]; then
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
