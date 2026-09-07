#!/usr/bin/env bash
# Load the harness's settings from a .env file at the repo root.
#
# The live gate takes a dozen values - a Supervisor, an AWX, a template,
# a key - and none of them are derivable, so before this they had to be
# retyped or kept in shell history on every run. .env keeps them in one
# gitignored file. See .env.example for the full set.
#
# Sourced by the harness scripts:
#
#   source "$HARNESS_DIR/lib/dotenv.sh"
#   load_dotenv
#
# and run directly by the Makefile to read one value out:
#
#   test/lib/dotenv.sh DEV_VERSION
#
# The file is bash, sourced as bash, so quoting works the way it does
# everywhere else - `AWX_TEMPLATE="Configure Webserver"` is one value,
# and `#` starts a comment.
#
# Anything already in the environment wins over the file. That ordering
# is what keeps `AWX_TEMPLATE=... make verify-supervisor` and
# `DEV_VERSION=1.0.1-rc2 make ...` working as one-off overrides without
# editing .env, and it is why this cannot just be `set -a; source`.

# load_dotenv [file] - default: .env beside this repo's Makefile.
load_dotenv() {
  local file="${1:-${DOTENV_FILE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.env}}"
  [[ -r "$file" ]] || return 0

  # The keys the file assigns, so we know which ones to protect.
  local line key
  local -a keys=()
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)= ]]; then
      keys+=("${BASH_REMATCH[2]}")
    fi
  done < "$file"

  local -A was_set=() previous=()
  for key in ${keys[@]+"${keys[@]}"}; do
    if [[ -n "${!key+x}" ]]; then
      was_set["$key"]=1
      previous["$key"]="${!key}"
    fi
  done

  set -a
  # shellcheck disable=SC1090
  source "$file"
  set +a

  # Put back anything that was already set, keeping the export the
  # sourcing just gave it.
  for key in ${keys[@]+"${keys[@]}"}; do
    [[ -n "${was_set[$key]:-}" ]] && printf -v "$key" '%s' "${previous[$key]}"
  done
  return 0
}

# Run directly: print one value, empty if the file does not set it. Used
# by the Makefile, which needs DEV_VERSION before any script runs.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  [[ $# -eq 1 ]] || { echo "usage: ${0##*/} VAR_NAME" >&2; exit 64; }
  load_dotenv
  printf '%s' "${!1:-}"
fi
