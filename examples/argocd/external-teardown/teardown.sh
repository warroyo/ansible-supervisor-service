#!/usr/bin/env bash
# Decommission the guest, then delete the Argo application that owns its VM.
#
# The ordering is the whole point, and so is the failure behaviour: if the
# decommission does not succeed, the application is never deleted. A script
# that pressed on would destroy a VM whose decommission playbook had not run,
# which is the exact failure this sequencing exists to prevent.
#
#   ./teardown.sh <app-name> <namespace>
set -euo pipefail

APP="${1:?usage: teardown.sh <app-name> <namespace>}"
NS="${2:?usage: teardown.sh <app-name> <namespace>}"
TIMEOUT="${TIMEOUT:-1800}"

# A fresh identity every time. Reusing a name would either be rejected (the
# object still exists, its spec is immutable) or, worse, find a leftover
# successful run from an earlier attempt and read it as this attempt's
# success - authorizing the deletion of a VM that was never decommissioned.
RUN="decommission-web-1-$(date -u +%Y%m%d%H%M%S)"

echo "creating $RUN in $NS"
sed -e "s/decommission-web-1-TIMESTAMP/$RUN/" -e "s/namespace: my-namespace/namespace: $NS/" \
  "$(dirname "$0")/ansiblerun.yml" | kubectl apply -f - >/dev/null

echo "waiting for it to finish (up to ${TIMEOUT}s)"
waited=0
while true; do
  state=$(kubectl get ansiblerun "$RUN" -n "$NS" -o jsonpath='{.status.state}' 2>/dev/null || true)
  case "$state" in
    Ready)
      echo "decommission succeeded: $(kubectl get ansiblerun "$RUN" -n "$NS" -o jsonpath='{.status.jobURL}')"
      break
      ;;
    Failed)
      echo "decommission FAILED, not deleting $APP:" >&2
      kubectl get ansiblerun "$RUN" -n "$NS" -o jsonpath='{.status.message}{"\n"}' >&2
      exit 1
      ;;
  esac
  if [[ $waited -ge $TIMEOUT ]]; then
    echo "decommission did not finish within ${TIMEOUT}s, not deleting $APP" >&2
    echo "state is '${state:-<none>}'; check the run and the AWX job before retrying" >&2
    exit 1
  fi
  sleep 5
  waited=$((waited + 5))
done

echo "deleting application $APP"
argocd app delete "$APP" --yes

echo "done. The AnsibleRun is left in place as the record that it ran:"
echo "  kubectl delete ansiblerun $RUN -n $NS   # when you no longer need it"
