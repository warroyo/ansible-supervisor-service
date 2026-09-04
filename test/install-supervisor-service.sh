#!/usr/bin/env bash
# Register and install (or upgrade) this Carvel supervisor service on a
# Supervisor, through the vCenter REST API.
#
# Broadcom documents Supervisor Services as a vCenter UI flow only - the
# API reference page for namespace_management/supervisor_services renders
# "Object Not Found" - but the endpoints exist and are drivable. This
# matters more than convenience: the Supervisor's own RBAC refuses even a
# full vSphere administrator the ability to create CRDs, ClusterRoles,
# PackageInstalls, namespaces or ServiceAccounts, so `kubectl apply` is
# not a fallback. The vCenter service-install path is the only way the
# CRDs land, which makes this the inner dev loop for the project.
#
# Usage:
#   read -rs "P?vCenter password: " && printf '%s' "$P" > /tmp/vc_pw && unset P
#   export VC_HOST=vc01.example.lab
#   export VC_USER=administrator@vsphere.local
#   export VC_PASSWORD_FILE=/tmp/vc_pw
#   make install-supervisor-service DEV_VERSION=1.0.1-rc1
#
# Optional:
#   VC_CLUSTER        cluster id (e.g. domain-c9). Auto-discovered if there
#                     is exactly one.
#   SERVICE_VALUES    path to the package values YAML to install with.
#                     Defaults to config/values.yml with `namespace`
#                     stripped - the Supervisor fills that in itself.
#   SERVICE_YAML      path to the package file. Defaults to
#                     ./ansible-supervisor.yml, what `make release` writes.
#   KUBECONFIG        if set, the script also waits for the Deployment to
#                     actually roll, which the vCenter status does not tell
#                     you (see "current_version flips early" below).
set -euo pipefail

: "${VC_HOST:?set VC_HOST to the vCenter FQDN}"
: "${VC_USER:?set VC_USER, e.g. administrator@vsphere.local}"
: "${VC_PASSWORD_FILE:?set VC_PASSWORD_FILE to a file holding the vCenter password}"
[[ -r "$VC_PASSWORD_FILE" ]] || { echo "cannot read $VC_PASSWORD_FILE" >&2; exit 1; }

SERVICE_YAML="${SERVICE_YAML:-ansible-supervisor.yml}"
[[ -r "$SERVICE_YAML" ]] || { echo "$SERVICE_YAML not found - run 'make dev-release DEV_VERSION=...' first" >&2; exit 1; }

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

log()  { echo "[install] $*"; }
fail() { echo "[install] FAILED: $*" >&2; exit 1; }

b64() { base64 -w0 < "$1" 2>/dev/null || base64 < "$1" | tr -d '\n'; }

# --- auth -------------------------------------------------------------
# The password goes to curl through a config file on stdin rather than -u
# on the command line, which would put it in the process list for anyone
# running ps while this is up.
vc_login() {
  curl -sSk --config - <<EOF | tr -d '"'
url = "https://${VC_HOST}/api/session"
request = "POST"
user = "${VC_USER}:$(cat "$VC_PASSWORD_FILE")"
EOF
}

TOKEN="$(vc_login)"
[[ -n "$TOKEN" ]] || fail "could not open a vCenter session as $VC_USER"

vc() {  # vc <method> <path> [body-file] -> body on stdout, HTTP code in VC_CODE
  local method="$1" path="$2" body="${3:-}"
  local args=(-sSk -X "$method" -H "vmware-api-session-id: ${TOKEN}"
              -H "Content-Type: application/json"
              -w '\n%{http_code}' "https://${VC_HOST}${path}")
  [[ -n "$body" ]] && args+=(--data-binary "@${body}")
  local out; out="$(curl "${args[@]}")"
  VC_CODE="${out##*$'\n'}"
  printf '%s' "${out%$'\n'*}"
}

jqp() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

# --- which surface does this vCenter serve? ---------------------------
# The widely-circulated PowerShell example uses
# /namespace-management/supervisors/{sup}/supervisor-services, which 404s
# on some vCenters where /clusters/{cluster}/... works. The tier-1
# (register) paths are identical between the two; only this tier-2
# collection differs. Probe rather than assume.
SURFACE=""
for candidate in clusters supervisors; do
  vc GET "/api/vcenter/namespace-management/${candidate}" >/dev/null
  if [[ "$VC_CODE" == "200" ]]; then
    SURFACE="$candidate"
    break
  fi
done
[[ -n "$SURFACE" ]] || fail "neither /namespace-management/clusters nor /supervisors responded 200"
log "this vCenter serves /${SURFACE}"

if [[ -z "${VC_CLUSTER:-}" ]]; then
  CLUSTERS="$(vc GET "/api/vcenter/namespace-management/${SURFACE}")"
  COUNT="$(printf '%s' "$CLUSTERS" | jqp "len(d)")"
  [[ "$COUNT" == "1" ]] || fail "found $COUNT ${SURFACE}; set VC_CLUSTER to the one to install on"
  VC_CLUSTER="$(printf '%s' "$CLUSTERS" | jqp "d[0].get('cluster') or d[0].get('supervisor')")"
fi
log "target ${SURFACE%s}: $VC_CLUSTER"

# --- what are we installing? ------------------------------------------
# refName and version come out of the generated package rather than being
# passed in, so this can never install a version the file does not
# contain.
SVC="$(grep -m1 '^  refName:' "$SERVICE_YAML" | awk '{print $2}')"
PKG_VERSION="$(grep -m1 '^  version:' "$SERVICE_YAML" | awk '{print $2}')"
[[ -n "$SVC" && -n "$PKG_VERSION" ]] || fail "could not read refName/version out of $SERVICE_YAML"
if [[ -n "${DEV_VERSION:-}" && "$DEV_VERSION" != "$PKG_VERSION" ]]; then
  fail "$SERVICE_YAML is version $PKG_VERSION but DEV_VERSION is $DEV_VERSION - re-run 'make dev-release DEV_VERSION=$DEV_VERSION'"
fi
log "service $SVC version $PKG_VERSION"

if [[ -n "${SERVICE_VALUES:-}" ]]; then
  [[ -r "$SERVICE_VALUES" ]] || fail "cannot read SERVICE_VALUES=$SERVICE_VALUES"
  cp "$SERVICE_VALUES" "$WORK_DIR/values.yml"
else
  # config/values.yml minus the ytt schema header and `namespace`, which
  # the Supervisor fills in itself and rejects being told.
  grep -v '^#@\|^---\|^namespace:' config/values.yml | grep -v '^\s*$' > "$WORK_DIR/values.yml"
fi
log "installing with values:"
sed 's/^/         /' "$WORK_DIR/values.yml"

# --- tier 1: the vCenter-wide service catalog -------------------------

REGISTERED="$(vc GET "/api/vcenter/namespace-management/supervisor-services/${SVC}" >/dev/null; echo "$VC_CODE")"

if [[ "$REGISTERED" != "200" ]]; then
  log "registering $SVC (first time)"
  python3 -c "
import json,sys
print(json.dumps({'spec_type':'CARVEL','carvel_spec':{'version_spec':{'content':sys.argv[1]}}}))
" "$(b64 "$SERVICE_YAML")" > "$WORK_DIR/register.json"
  vc POST "/api/vcenter/namespace-management/supervisor-services" "$WORK_DIR/register.json" >/dev/null
  [[ "$VC_CODE" =~ ^2 ]] || fail "registering the service returned HTTP $VC_CODE"
else
  HAVE="$(vc GET "/api/vcenter/namespace-management/supervisor-services/${SVC}/versions" \
    | jqp "'yes' if any(v.get('version')=='${PKG_VERSION}' for v in d) else 'no'")"
  if [[ "$HAVE" == "yes" ]]; then
    log "version $PKG_VERSION is already in the catalog"
  else
    log "adding version $PKG_VERSION to the existing service"
    # Note the body shape differs from registration: no spec_type, and no
    # version_spec wrapper.
    python3 -c "
import json,sys
print(json.dumps({'carvel_spec':{'content':sys.argv[1]}}))
" "$(b64 "$SERVICE_YAML")" > "$WORK_DIR/version.json"
    vc POST "/api/vcenter/namespace-management/supervisor-services/${SVC}/versions" "$WORK_DIR/version.json" >/dev/null
    [[ "$VC_CODE" =~ ^2 ]] || fail "adding version $PKG_VERSION returned HTTP $VC_CODE"
  fi
fi

# Every successful write returns an empty body - no id, no confirmation,
# not even {}. Success is indistinguishable from a silently ignored
# request without reading it back.
vc GET "/api/vcenter/namespace-management/supervisor-services/${SVC}/versions" \
  | jqp "'yes' if any(v.get('version')=='${PKG_VERSION}' for v in d) else 'no'" \
  | grep -q yes || fail "version $PKG_VERSION is not in the catalog after the write"
log "catalog has $PKG_VERSION"

# --- tier 2: install or upgrade on the supervisor ---------------------

TIER2="/api/vcenter/namespace-management/${SURFACE}/${VC_CLUSTER}/supervisor-services"
INSTALLED="$(vc GET "${TIER2}/${SVC}" >/dev/null; echo "$VC_CODE")"

python3 -c "
import json,sys
print(json.dumps({'version':sys.argv[1],'yaml_service_config':sys.argv[2]}))
" "$PKG_VERSION" "$(b64 "$WORK_DIR/values.yml")" > "$WORK_DIR/upgrade.json"

if [[ "$INSTALLED" == "200" ]]; then
  log "upgrading in place to $PKG_VERSION"
  vc PUT "${TIER2}/${SVC}" "$WORK_DIR/upgrade.json" >/dev/null
  [[ "$VC_CODE" =~ ^2 ]] || fail "the upgrade returned HTTP $VC_CODE"
else
  log "installing $PKG_VERSION"
  python3 -c "
import json,sys
print(json.dumps({'supervisor_service':sys.argv[1],'version':sys.argv[2],'yaml_service_config':sys.argv[3]}))
" "$SVC" "$PKG_VERSION" "$(b64 "$WORK_DIR/values.yml")" > "$WORK_DIR/install.json"
  vc POST "$TIER2" "$WORK_DIR/install.json" >/dev/null
  [[ "$VC_CODE" =~ ^2 ]] || fail "the install returned HTTP $VC_CODE"
fi

log "waiting for config_status CONFIGURED"
for _ in $(seq 1 60); do
  BOTH="$(vc GET "${TIER2}/${SVC}" | jqp "d.get('config_status','')+' '+d.get('current_version','')" 2>/dev/null || echo " ")"
  STATUS="${BOTH%% *}"
  CURRENT="${BOTH##* }"
  [[ "$STATUS" == "CONFIGURED" && "$CURRENT" == "$PKG_VERSION" ]] && break
  sleep 5
done
[[ "$STATUS" == "CONFIGURED" ]] || fail "config_status is '$STATUS' after 5 minutes, wanted CONFIGURED"
log "vCenter reports CONFIGURED at $CURRENT"

# --- the workload has NOT necessarily rolled yet ----------------------
# current_version reflects the PackageInstall, not the Deployment behind
# it. Polling for it and then reading pod age has shown a 12-minute-old
# pod still on the previous image. Only the pod itself settles this.
if [[ -n "${KUBECONFIG:-}" ]] && command -v kubectl >/dev/null 2>&1; then
  log "waiting for the controller Deployment to actually roll"
  CTRL_NS="$(kubectl get deployment -A -l app=ansible-supervisor \
    -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)"
  [[ -n "$CTRL_NS" ]] || fail "vCenter says CONFIGURED but no Deployment labelled app=ansible-supervisor exists"
  kubectl rollout status deployment/ansible-supervisor-controller -n "$CTRL_NS" --timeout=300s \
    || fail "the controller Deployment did not roll out"
  log "rolled out in $CTRL_NS, image $(kubectl get deployment ansible-supervisor-controller -n "$CTRL_NS" \
    -o jsonpath='{.spec.template.spec.containers[0].image}')"
else
  log "KUBECONFIG not set: skipping the rollout check."
  log "vCenter reporting CONFIGURED does NOT mean the new pod is running - verify-supervisor asserts the digest."
fi

log "DONE. Next: make verify-supervisor DEV_VERSION=$PKG_VERSION ..."
