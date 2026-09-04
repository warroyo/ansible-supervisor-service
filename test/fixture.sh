#!/usr/bin/env bash
# Create or destroy the test VM that verify-supervisor.sh runs against.
#
# Split out so the fixture is part of the harness rather than something
# built by hand before each release. verify-supervisor.sh calls this
# itself unless VM_LABEL names a VM you already have.
#
#   test/fixture.sh up     # create the VM, wait for it to report an IP
#   test/fixture.sh down   # delete the VM and its bootstrap Secret
#
# Required:
#   SUPERVISOR_NS       namespace to create the VM in
#
# The SSH key AWX authenticates with:
#   SSH_PUBLIC_KEY_FILE   public key to authorize for the `ansible` user.
#                         AWX must already hold the matching private key in
#                         the Machine credential attached to the template.
#                         This is the non-destructive path and the default.
#
#   MANAGE_AWX_CREDENTIAL=1 instead generates a fresh keypair per run and
#                         PATCHes the private half into the AWX credential
#                         named by AWX_CREDENTIAL (default: the one attached
#                         to AWX_TEMPLATE). DESTRUCTIVE: the credential's
#                         previous private key is overwritten and cannot be
#                         read back first, so anything else authenticating
#                         with it breaks. Only for an AWX you own.
#
# Optional, all auto-discovered when the namespace offers exactly one
# sensible answer:
#   VM_IMAGE, VM_CLASS, VM_STORAGE_CLASS, FIXTURE_NAME, FIXTURE_LABEL
set -euo pipefail

: "${SUPERVISOR_NS:?set SUPERVISOR_NS}"

FIXTURE_NAME="${FIXTURE_NAME:-ansible-verify-fixture}"
FIXTURE_LABEL="${FIXTURE_LABEL:-app=${FIXTURE_NAME}}"
STATE_DIR="${FIXTURE_STATE_DIR:-${TMPDIR:-/tmp}/ansible-supervisor-fixture}"

log()  { echo "[fixture] $*"; }
fail() { echo "[fixture] FAILED: $*" >&2; exit 1; }

kube() { kubectl -n "$SUPERVISOR_NS" "$@"; }

vm_api_version() {
  # The Supervisor serves several VirtualMachine versions side by side and
  # retires the oldest over time. Ask for the preferred one rather than
  # pinning: spec.bootstrap only exists from v1alpha2 on, and a pinned
  # version becomes "no such field" the day it stops being served.
  kubectl get --raw /apis/vmoperator.vmware.com 2>/dev/null \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['preferredVersion']['version'])"
}

discover() {
  if [[ -z "${VM_IMAGE:-}" ]]; then
    local images
    images="$(kube get virtualmachineimage -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
    # shellcheck disable=SC2086
    set -- $images
    [[ $# -eq 1 ]] || fail "found $# VirtualMachineImages in $SUPERVISOR_NS; set VM_IMAGE to the one to use"
    VM_IMAGE="$1"
  fi
  if [[ -z "${VM_CLASS:-}" ]]; then
    # Smallest sensible default, and the one every namespace has.
    kube get virtualmachineclass best-effort-small >/dev/null 2>&1 \
      || fail "best-effort-small is not available in $SUPERVISOR_NS; set VM_CLASS"
    VM_CLASS="best-effort-small"
  fi
  if [[ -z "${VM_STORAGE_CLASS:-}" ]]; then
    # Copy whatever the namespace's existing VMs use rather than guessing:
    # a storage class the namespace has no quota for is rejected.
    VM_STORAGE_CLASS="$(kube get virtualmachine -o jsonpath='{.items[0].spec.storageClass}' 2>/dev/null || true)"
    [[ -n "$VM_STORAGE_CLASS" ]] || fail "could not infer a storage class from existing VMs; set VM_STORAGE_CLASS"
  fi
}

# --- the SSH key AWX will authenticate with ---------------------------

resolve_public_key() {
  mkdir -p "$STATE_DIR"; chmod 700 "$STATE_DIR"

  if [[ "${MANAGE_AWX_CREDENTIAL:-0}" != "1" ]]; then
    : "${SSH_PUBLIC_KEY_FILE:?set SSH_PUBLIC_KEY_FILE to the public key whose private half is in AWX, or set MANAGE_AWX_CREDENTIAL=1 to have this script rotate the credential (destructive)}"
    [[ -r "$SSH_PUBLIC_KEY_FILE" ]] || fail "cannot read $SSH_PUBLIC_KEY_FILE"
    PUBKEY="$(cat "$SSH_PUBLIC_KEY_FILE")"
    log "authorizing the key from $SSH_PUBLIC_KEY_FILE (AWX credential untouched)"
    return
  fi

  : "${AWX_URL:?MANAGE_AWX_CREDENTIAL=1 needs AWX_URL}"
  : "${AWX_TOKEN:?MANAGE_AWX_CREDENTIAL=1 needs AWX_TOKEN}"
  : "${AWX_TEMPLATE:?MANAGE_AWX_CREDENTIAL=1 needs AWX_TEMPLATE to find the credential to rotate}"

  local base=""
  for c in /api/v2 /api/controller/v2; do
    curl -sf -H "Authorization: Bearer ${AWX_TOKEN}" "${AWX_URL}${c}/me/" >/dev/null 2>&1 && { base="$c"; break; }
  done
  [[ -n "$base" ]] || fail "cannot authenticate to $AWX_URL"

  local tid
  tid="$(curl -sf -H "Authorization: Bearer ${AWX_TOKEN}" \
    "${AWX_URL}${base}/job_templates/?name=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$AWX_TEMPLATE")" \
    | python3 -c "
import json,sys
m=[t for t in json.load(sys.stdin).get('results',[]) if t.get('name')==sys.argv[1]]
sys.exit(1) if len(m)!=1 else print(m[0]['id'])
" "$AWX_TEMPLATE")" || fail "template '$AWX_TEMPLATE' not found or ambiguous"

  local cred
  cred="${AWX_CREDENTIAL_ID:-$(curl -sf -H "Authorization: Bearer ${AWX_TOKEN}" \
    "${AWX_URL}${base}/job_templates/${tid}/credentials/" \
    | python3 -c "
import json,sys
m=[c for c in json.load(sys.stdin).get('results',[]) if c.get('kind')=='ssh']
sys.exit(1) if len(m)!=1 else print(m[0]['id'])
")}" || fail "could not find exactly one Machine credential on template '$AWX_TEMPLATE'; set AWX_CREDENTIAL_ID"

  log "MANAGE_AWX_CREDENTIAL=1: rotating the private key on AWX credential $cred"
  log "  the previous private key cannot be read back and will be lost"

  rm -f "$STATE_DIR/id" "$STATE_DIR/id.pub"
  ssh-keygen -t ed25519 -N '' -C "ansible@${FIXTURE_NAME}" -f "$STATE_DIR/id" >/dev/null
  chmod 600 "$STATE_DIR/id"

  local user
  user="$(curl -sf -H "Authorization: Bearer ${AWX_TOKEN}" "${AWX_URL}${base}/credentials/${cred}/" \
    | python3 -c "import json,sys; print(json.load(sys.stdin).get('inputs',{}).get('username') or 'ansible')")"

  python3 -c "
import json,sys
k=open(sys.argv[2]).read()
if not k.endswith('\n'): k+='\n'
print(json.dumps({'inputs':{'username':sys.argv[1],'ssh_key_data':k}}))
" "$user" "$STATE_DIR/id" > "$STATE_DIR/cred.json"

  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X PATCH \
    -H "Authorization: Bearer ${AWX_TOKEN}" -H 'Content-Type: application/json' \
    --data-binary "@$STATE_DIR/cred.json" "${AWX_URL}${base}/credentials/${cred}/")"
  rm -f "$STATE_DIR/cred.json"
  [[ "$code" == "200" ]] || fail "rotating AWX credential $cred returned HTTP $code"

  PUBKEY="$(cat "$STATE_DIR/id.pub")"
  log "credential $cred now holds the key at $STATE_DIR/id (user: $user)"
}

# --- up ---------------------------------------------------------------

fixture_up() {
  discover
  resolve_public_key

  local api; api="$(vm_api_version)"
  log "creating $FIXTURE_NAME in $SUPERVISOR_NS"
  log "  api=$api image=$VM_IMAGE class=$VM_CLASS storage=$VM_STORAGE_CLASS label=$FIXTURE_LABEL"

  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: ${FIXTURE_NAME}-bootstrap
  namespace: ${SUPERVISOR_NS}
type: Opaque
stringData:
  user-data: |
    #cloud-config
    hostname: ${FIXTURE_NAME}
    users:
      - name: ansible
        sudo: ALL=(ALL) NOPASSWD:ALL
        shell: /bin/bash
        ssh_authorized_keys:
          - ${PUBKEY}
    ssh_pwauth: false
---
apiVersion: vmoperator.vmware.com/${api}
kind: VirtualMachine
metadata:
  name: ${FIXTURE_NAME}
  namespace: ${SUPERVISOR_NS}
  labels:
    ${FIXTURE_LABEL%%=*}: "${FIXTURE_LABEL#*=}"
spec:
  className: ${VM_CLASS}
  imageName: ${VM_IMAGE}
  storageClass: ${VM_STORAGE_CLASS}
  powerState: PoweredOn
  bootstrap:
    cloudInit:
      rawCloudConfig:
        name: ${FIXTURE_NAME}-bootstrap
        key: user-data
YAML

  log "waiting for a reported IP"
  local ip=""
  for _ in $(seq 1 90); do
    ip="$(kube get virtualmachine "$FIXTURE_NAME" -o jsonpath='{.status.network.primaryIP4}' 2>/dev/null || true)"
    [[ -n "$ip" ]] && break
    sleep 10
  done
  [[ -n "$ip" ]] || fail "$FIXTURE_NAME reported no IP after 15 minutes"

  # An IP is not a running sshd. The VM answers the controller's readiness
  # check as soon as vm-operator reports the address, but cloud-init is
  # still writing authorized_keys, and AWX would fail to connect. Wait for
  # the guest to settle rather than racing it.
  local settle="${FIXTURE_SETTLE_SECONDS:-90}"
  log "IP is $ip; waiting ${settle}s for cloud-init to finish provisioning the ansible user"
  sleep "$settle"

  log "UP: $FIXTURE_NAME at $ip, selector $FIXTURE_LABEL"
}

# --- down -------------------------------------------------------------

fixture_down() {
  log "deleting $FIXTURE_NAME and its bootstrap Secret from $SUPERVISOR_NS"
  kube delete virtualmachine "$FIXTURE_NAME" --ignore-not-found --timeout=300s >/dev/null 2>&1 || true
  kube delete secret "${FIXTURE_NAME}-bootstrap" --ignore-not-found >/dev/null 2>&1 || true
  log "DOWN"
}

case "${1:-}" in
  up)   fixture_up ;;
  down) fixture_down ;;
  *)    echo "usage: $0 up|down" >&2; exit 1 ;;
esac
