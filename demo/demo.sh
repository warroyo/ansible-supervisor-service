#!/usr/bin/env bash
# A guided, pausing walkthrough of the service for showing it to people.
#
#   demo/demo.sh          # or `make demo`: run the walkthrough
#   demo/demo.sh clean    # or `make demo-clean`: remove everything it made
#
# It prints every manifest and command before running it, then waits for
# Enter, so it can be narrated live. DEMO_PAUSE=0 runs it straight through.
#
# What it shows, in order:
#   1. connect the namespace to AWX         (Secret + AWXConnection)
#   2. create a VM                          (VM Service VM with a label)
#   3. bind it to a template                (AnsibleBinding -> AWX job)
#   4. scale out                            (a second VM joins on its own)
#   5. a one-off run                        (AnsibleRun borrowing the host)
#   6. delete a VM                          (the onDeleted hook fires)
#
# Everything is left running at the end so it can be poked at in AWX and
# kubectl afterwards. `clean` takes it down.
#
# Settings come from .env, same as the release harness (see .env.example):
#   SUPERVISOR_NS, AWX_URL, AWX_TOKEN, AWX_TEMPLATE, SSH_PUBLIC_KEY_FILE
#   AWX_DEPROVISION_TEMPLATE   optional; step 6 is skipped without it
#   VM_IMAGE, VM_CLASS, VM_STORAGE_CLASS   as for test/fixture.sh
#
# The demo's own objects are all prefixed so it never touches anything
# else in the namespace: DEMO_PREFIX (default asb-demo), and the VMs
# carry the label app=$DEMO_PREFIX.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/test/lib/dotenv.sh"
load_dotenv

: "${SUPERVISOR_NS:?set SUPERVISOR_NS}"

PREFIX="${DEMO_PREFIX:-asb-demo}"
LABEL="app=${PREFIX}"
CONN="${PREFIX}-awx"
TOKEN_SECRET="${PREFIX}-awx-token"
BINDING="${PREFIX}"
RUN="${PREFIX}-adhoc"
VM1="${PREFIX}-vm-1"
VM2="${PREFIX}-vm-2"
PAUSE="${DEMO_PAUSE:-1}"

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '  %s\n' "$*"; }
fail() { echo "[demo] FAILED: $*" >&2; exit 1; }

pause() {
  [[ "$PAUSE" == "1" ]] || return 0
  read -rp $'\n  [Enter to continue] ' _ </dev/tty
}

# show <command...> - print a command the audience should see, then run it.
show() {
  printf '\n  \033[36m$ %s\033[0m\n' "$*"
  "$@"
}

# apply_shown - print a manifest from stdin, then apply it.
apply_shown() {
  local doc; doc="$(cat)"
  printf '\n\033[36m%s\033[0m\n' "$doc" | sed 's/^/  /'
  printf '%s\n' "$doc" | kubectl apply -n "$SUPERVISOR_NS" -f -
}

kube() { kubectl -n "$SUPERVISOR_NS" "$@"; }

# wait_for <description> <timeout seconds> <command...>
wait_for() {
  local what="$1" timeout="$2"; shift 2
  local waited=0
  printf '  waiting for %s ' "$what"
  until "$@" >/dev/null 2>&1; do
    (( waited >= timeout )) && { echo; fail "$what did not happen within ${timeout}s"; }
    printf '.'; sleep 5; waited=$((waited + 5))
  done
  echo " done"
}

vm_up() {
  FIXTURE_NAME="$1" FIXTURE_LABEL="$LABEL" "$ROOT/test/fixture.sh" up
}

children_succeeded() {
  local want="$1"
  [[ "$(kube get ansiblebindingvm -l "field.vmware.com/binding=${BINDING}" \
    -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c '^Succeeded$')" -ge "$want" ]]
}

run_terminal() {
  kube get ansiblerun "$RUN" -o jsonpath='{.status.state}' | grep -qE '^(Ready|Failed)$'
}

hook_event() {
  kube get events --field-selector "involvedObject.name=${BINDING}" \
    -o jsonpath='{.items[*].reason}' | grep -qE 'DeprovisionHook'
}

# --- run --------------------------------------------------------------

demo_run() {
  : "${AWX_URL:?set AWX_URL}"
  : "${AWX_TOKEN:?set AWX_TOKEN}"
  : "${AWX_TEMPLATE:?set AWX_TEMPLATE}"

  say "0. The service is installed on this Supervisor"
  kubectl get --raw /version >/dev/null 2>&1 \
    || fail "cannot reach the Supervisor API for context $(kubectl config current-context) - log in again, or check the network"
  printf '\n  \033[36m$ kubectl get crd | grep field.vmware.com\033[0m\n'
  kubectl get crd | grep field.vmware.com || fail "the service's CRDs are not installed - see QUICKSTART.md step 1"
  note "AWX: $AWX_URL   template: $AWX_TEMPLATE   namespace: $SUPERVISOR_NS"
  pause

  say "1. Connect the namespace to AWX"
  note "The token lives in a Secret; the AWXConnection points at it."
  # From a file descriptor, so the token is never on a command line.
  kube create secret generic "$TOKEN_SECRET" --from-file=token=<(printf '%s' "$AWX_TOKEN") \
    --dry-run=client -o yaml | kube apply -f - >/dev/null
  note "(Secret $TOKEN_SECRET created from AWX_TOKEN)"
  apply_shown <<EOF
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: ${CONN}
spec:
  url: "${AWX_URL}"
  secretRef: ${TOKEN_SECRET}
EOF
  wait_for "the AWXConnection to go Ready" 60 \
    bash -c "kubectl -n '$SUPERVISOR_NS' get awxconnection '$CONN' -o jsonpath='{.status.ready}' | grep -q true"
  show kube get awxconnection "$CONN"
  pause

  say "2. Create a VM"
  note "An ordinary VM Service VM. The only thing this service cares about is the label: $LABEL"
  note "cloud-init authorizes the key whose private half AWX holds. This takes a few minutes."
  vm_up "$VM1"
  show kube get vm -l "$LABEL" -o wide
  pause

  say "3. Bind it to an AWX template"
  apply_shown <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleBinding
metadata:
  name: ${BINDING}
spec:
  vmSelector:
    app: ${PREFIX}
  awxConnectionRef: ${CONN}
  template:
    name: "${AWX_TEMPLATE}"
    type: JobTemplate
  extraVars:
    environment: demo
$( [[ -n "${AWX_DEPROVISION_TEMPLATE:-}" ]] && printf '  onDeleted:\n    template:\n      name: "%s"\n      type: JobTemplate\n' "$AWX_DEPROVISION_TEMPLATE" )
EOF
  note "The controller adds $VM1 to the template's inventory and launches the job, limited to that one host."
  wait_for "the AWX job to finish" 600 children_succeeded 1
  show kube get ansiblebinding "$BINDING"
  show kube get ansiblebindingvm -l "field.vmware.com/binding=${BINDING}" \
    -o custom-columns=VM:.spec.vmName,PHASE:.status.phase,JOB:.status.lastJobURL
  note "Open the job URL to show it in AWX, and the host in the template's inventory."
  pause

  say "4. Scale out: a second VM with the same label"
  note "No change to the binding. It picks the new VM up on its own."
  vm_up "$VM2"
  wait_for "the second VM's job to finish" 600 children_succeeded 2
  show kube get ansiblebinding "$BINDING"
  show kube get ansiblebindingvm -l "field.vmware.com/binding=${BINDING}" \
    -o custom-columns=VM:.spec.vmName,PHASE:.status.phase,JOB:.status.lastJobURL
  pause

  say "5. A one-off run against one VM"
  note "An AnsibleRun is one job, launched once. It borrows the binding's host rather than making its own."
  apply_shown <<EOF
apiVersion: field.vmware.com/v1
kind: AnsibleRun
metadata:
  name: ${RUN}
spec:
  awxConnectionRef: ${CONN}
  template:
    name: "${AWX_TEMPLATE}"
    type: JobTemplate
  vmRef:
    name: ${VM1}
  extraVars:
    reason: day2-demo
  ttlSecondsAfterFinished: 3600
EOF
  wait_for "the run to finish" 600 run_terminal
  show kube get ansiblerun "$RUN" -o custom-columns=STATE:.status.state,JOB:.status.jobURL
  pause

  if [[ -z "${AWX_DEPROVISION_TEMPLATE:-}" ]]; then
    say "6. (skipped) Delete a VM - set AWX_DEPROVISION_TEMPLATE to show the onDeleted hook"
  else
    say "6. Delete a VM"
    note "The binding's onDeleted hook runs '$AWX_DEPROVISION_TEMPLATE' for it, then its host leaves the inventory."
    show kube delete vm "$VM2" --wait=false
    wait_for "the hook job to finish" 600 hook_event
    show kube get events --field-selector "involvedObject.name=${BINDING}" \
      -o custom-columns=REASON:.reason,MESSAGE:.message
    kube delete secret "${VM2}-bootstrap" --ignore-not-found >/dev/null
    show kube get ansiblebinding "$BINDING"
  fi

  say "Done. Everything is left running; 'make demo-clean' removes it."
}

# --- clean ------------------------------------------------------------

demo_clean() {
  say "Removing the demo from $SUPERVISOR_NS"
  kube delete ansiblerun "$RUN" --ignore-not-found --timeout=300s
  # Before the VMs and the connection: the binding's finalizer needs both
  # to take its hosts back out of AWX.
  kube delete ansiblebinding "$BINDING" --ignore-not-found --timeout=300s
  for vm in "$VM1" "$VM2"; do
    FIXTURE_NAME="$vm" "$ROOT/test/fixture.sh" down
  done
  kube delete awxconnection "$CONN" --ignore-not-found
  kube delete secret "$TOKEN_SECRET" --ignore-not-found
  say "Clean."
}

case "${1:-run}" in
  run)   demo_run ;;
  clean) demo_clean ;;
  *)     echo "usage: $0 [run|clean]" >&2; exit 1 ;;
esac
