#!/usr/bin/env bash
# strict.sh — shared strict-mode + structured-failure helpers.
#
# Sourced by every top-level installer script (bootstrap.sh and
# scripts/*.sh). Provides:
#
#   1. strict_enable        — `set -euo pipefail` + an ERR trap that, on
#                             any non-zero exit anywhere in the script,
#                             prints a single clearly-delimited block:
#                               * script name + failing line + BASH_COMMAND
#                               * exit code
#                               * last 20 lines of $STRICT_LOG_FILE if set
#                               * a "what to check" hint matched to the
#                                 failing step (set via strict_step "...")
#                             so a future failure surfaces as one block
#                             at the bottom of the log, not a cryptic
#                             shell error followed by silent skipping.
#
#   2. strict_step "label"  — record the script's current logical step
#                             so the ERR trap can include it in the hint.
#
#   3. strict_set_log PATH  — declare which log file (if any) the script
#                             tees to; the ERR trap will tail this on
#                             failure.
#
#   4. require_root         — `EUID==0` guard with consistent message.
#
#   5. require_cmd CMD...   — verify each named command is on PATH.
#                             Fails with `Missing prerequisite: <cmd>`
#                             and a remediation hint.
#
#   6. require_env VAR...   — verify each env var is set AND non-empty.
#
#   7. require_file PATH    — verify file exists, is regular, and is
#                             non-zero size. Sentinel files left
#                             behind by a crash (zero-byte) are
#                             explicitly rejected.
#
#   8. require_dir PATH     — verify directory exists.
#
#   9. require_service NAME — verify systemd unit is `active`.
#
#  10. run_subscript PATH [ARGS...]
#                           — invoke a child shell script, capture rc
#                             explicitly, and on failure print a
#                             structured block (which sub-script, last
#                             recorded step inside it via $STRICT_STEP,
#                             rc, log pointer). Aborts with the rc.
#
#  11. install_secret_file MODE PATH
#                           — read stdin, atomic-rename to PATH with
#                             the requested mode and root:root ownership,
#                             then verify the on-disk mode matches.
#
# Conventions:
#   - All output via printf, never echo.
#   - The ERR trap is conservative: it only fires once per script (it
#     unsets itself before the failure block prints) so nested traps
#     don't pile up.
#   - This file is sourced, never executed. It exports nothing into
#     the environment; helpers are plain shell functions.

# Guard against double-sourcing.
if [[ -n "${RAPH_STRICT_LIB_LOADED:-}" ]]; then
  return 0 2>/dev/null || exit 0
fi
RAPH_STRICT_LIB_LOADED=1

# State recorded by strict_step / strict_set_log.
STRICT_STEP="${STRICT_STEP:-<unspecified>}"
STRICT_LOG_FILE="${STRICT_LOG_FILE:-}"
STRICT_SCRIPT_NAME="${STRICT_SCRIPT_NAME:-${0##*/}}"

# Step hint table — keyed by case-insensitive substring of $STRICT_STEP.
# Add lines as new failure modes show up; the trap walks them in order
# and prints the first hint whose key matches.
strict_hint_for_step() {
  local s="${1,,}"
  case "$s" in
    *amneziawg*|*dkms*|*modprobe*|*gw0*)
      printf '%s' "Check: dmesg | tail; ls /var/lib/dkms/amneziawg/*/build/make.log; uname -r vs installed linux-headers."
      ;;
    *authelia*secret*|*generate-authelia-secrets*|*oidc-key*)
      printf '%s' "Check: ls -la /opt/stacks/authelia/secrets/; ensure openssl is on PATH; verify root has write access."
      ;;
    *compose*authelia*)
      printf '%s' "Check: docker logs authelia; verify /opt/stacks/authelia/secrets/* (mode 0600), configuration.yml renders, oidc-key.pem is PKCS#8."
      ;;
    *compose*ingress*|*npm*)
      printf '%s' "Check: docker logs ingress; verify ports 80/443/81 not in use (ss -tlnp); /opt/stacks/ingress/data writable."
      ;;
    *compose*enrol*)
      printf '%s' "Check: docker logs enrol; verify /etc/raph-installer/setup-token exists and is readable; SETUP_TOKEN in /opt/stacks/.env."
      ;;
    *compose*cloud*)
      printf '%s' "Check: docker logs cloud cloud-db cloud-redis cloud-web; verify /srv/store/cloud-{data,config,apps,db} exist with right uid/gid (33:33 / 70:70)."
      ;;
    *compose*console*)
      printf '%s' "Check: docker logs console; portainer needs /var/run/docker.sock readable."
      ;;
    *compose*qedge*)
      printf '%s' "Check: docker logs qedge; verify /opt/stacks/qedge/config/config.yaml rendered and tls/ symlinks valid."
      ;;
    *compose*)
      printf '%s' "Check: docker compose --env-file /opt/stacks/.env -f <stack>/docker-compose.yml config; docker logs <container>."
      ;;
    *render-templates*|*envsubst*)
      printf '%s' "Check: every \${VAR} referenced in *.template is set in /opt/stacks/.env; re-run scripts/render-templates.sh --check."
      ;;
    *bootstrap-host*|*hardening*|*sshd*|*ufw*)
      printf '%s' "Check: visudo -cf for sudoers drop-ins; sshd -t; ufw status."
      ;;
    *install-docker*|*docker*install*)
      printf '%s' "Check: apt-get install docker-ce; cat /etc/docker/daemon.json | jq; systemctl status docker."
      ;;
    *cert*|*letsencrypt*)
      printf '%s' "Check: ls /opt/stacks/ingress/letsencrypt/live/; openssl x509 -in fullchain.pem -noout -ext subjectAltName."
      ;;
    *npm*proxy*|*wire-npm-routes*|*proxy-host*)
      printf '%s' "Check: NPM admin reachable at http://127.0.0.1:81; NPM_EMAIL/NPM_PASS env or NPM_TOKEN supplied."
      ;;
    *)
      printf '%s' "Check the log block above and re-run with bash -x for verbose tracing."
      ;;
  esac
}

# Print the failure block. Called by the ERR trap.
strict_print_failure() {
  local rc=$1 line=$2 cmd=$3
  printf '\n' >&2
  printf '================================================================\n' >&2
  printf 'FATAL in %s at line %s\n'   "$STRICT_SCRIPT_NAME" "$line" >&2
  printf '  step:    %s\n'             "$STRICT_STEP" >&2
  printf '  command: %s\n'             "$cmd" >&2
  printf '  rc:      %s\n'             "$rc" >&2
  if [[ -n "$STRICT_LOG_FILE" && -r "$STRICT_LOG_FILE" ]]; then
    printf '  log:     %s\n' "$STRICT_LOG_FILE" >&2
    printf '  --- last 20 lines of %s ---\n' "$STRICT_LOG_FILE" >&2
    tail -n 20 "$STRICT_LOG_FILE" 2>/dev/null | sed 's/^/    /' >&2 || true
    printf '  --- end log tail ---\n' >&2
  fi
  printf '  hint:    %s\n' "$(strict_hint_for_step "$STRICT_STEP")" >&2
  printf '================================================================\n' >&2
}

# Install ERR trap. Self-disables after firing so nested traps don't
# stack. Note: ERR fires once per failed simple command under `set -e`;
# we exit ourselves so the script terminates with the original rc.
strict_enable() {
  set -euo pipefail
  trap 'rc=$?; trap - ERR; strict_print_failure "$rc" "${BASH_LINENO[0]}" "${BASH_COMMAND}"; exit "$rc"' ERR
}

strict_step()    { STRICT_STEP="$1"; }
strict_set_log() { STRICT_LOG_FILE="$1"; }

# --- preflight helpers ---------------------------------------------------

require_root() {
  if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    printf 'Missing prerequisite: must run as root (currently uid=%s)\n' "$(id -u)" >&2
    printf 'Remediation: re-run via sudo, or as the root user.\n' >&2
    exit 1
  fi
}

require_cmd() {
  local missing=()
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || missing+=("$c")
  done
  if (( ${#missing[@]} )); then
    printf 'Missing prerequisite: command(s) not on PATH: %s\n' "${missing[*]}" >&2
    printf 'Remediation: apt-get install the providing package(s), or check $PATH.\n' >&2
    exit 1
  fi
}

require_env() {
  local missing=()
  local v
  for v in "$@"; do
    if [[ -z "${!v:-}" ]]; then
      missing+=("$v")
    fi
  done
  if (( ${#missing[@]} )); then
    printf 'Missing prerequisite: env var(s) unset or empty: %s\n' "${missing[*]}" >&2
    printf 'Remediation: export the missing var(s), or source /opt/stacks/.env.\n' >&2
    exit 1
  fi
}

require_file() {
  local p
  for p in "$@"; do
    if [[ ! -f "$p" ]]; then
      printf 'Missing prerequisite: file not found: %s\n' "$p" >&2
      exit 1
    fi
    if [[ ! -s "$p" ]]; then
      printf 'Missing prerequisite: file present but zero bytes (likely a crash leftover): %s\n' "$p" >&2
      printf 'Remediation: rm %s and re-run the producing step.\n' "$p" >&2
      exit 1
    fi
  done
}

require_dir() {
  local p
  for p in "$@"; do
    if [[ ! -d "$p" ]]; then
      printf 'Missing prerequisite: directory not found: %s\n' "$p" >&2
      exit 1
    fi
  done
}

require_service() {
  local svc
  for svc in "$@"; do
    if ! systemctl is-active --quiet "$svc"; then
      printf 'Missing prerequisite: systemd service not active: %s\n' "$svc" >&2
      printf 'Remediation: systemctl status %s; systemctl start %s\n' "$svc" "$svc" >&2
      exit 1
    fi
  done
}

# Reject sentinel files left behind by an interrupted previous run.
require_sentinel() {
  local p="$1"
  if [[ -e "$p" && ! -s "$p" ]]; then
    printf 'Refusing to honour zero-byte sentinel %s — likely from an interrupted run.\n' "$p" >&2
    printf 'Remediation: rm %s and re-run the producing phase.\n' "$p" >&2
    exit 1
  fi
}

# Run a sub-script with explicit rc capture. Prints a structured block
# on failure pointing to its own log if it tees to one, then aborts.
#
# Usage:  run_subscript /path/to/child.sh [args...]
#         (env vars exported by the caller are inherited normally)
run_subscript() {
  local script="$1"; shift
  local rc=0
  if [[ ! -x "$script" && ! -r "$script" ]]; then
    printf 'run_subscript: %s is not executable or readable\n' "$script" >&2
    exit 1
  fi
  # Disable our own ERR trap inside the call so a child failure
  # bubbles up via $? rather than firing the trap on the bash invocation.
  set +e
  bash "$script" "$@"
  rc=$?
  set -e
  if (( rc != 0 )); then
    printf '\n' >&2
    printf '================================================================\n' >&2
    printf 'sub-script FAILED: %s\n' "$script" >&2
    printf '  args:    %s\n' "$*" >&2
    printf '  rc:      %s\n' "$rc" >&2
    printf '  step:    %s (parent: %s)\n' "$STRICT_STEP" "$STRICT_SCRIPT_NAME" >&2
    if [[ -n "$STRICT_LOG_FILE" ]]; then
      printf '  parent log: %s\n' "$STRICT_LOG_FILE" >&2
    fi
    # Best-effort hint about where the child writes its own log.
    case "$script" in
      *bootstrap-phase2*) printf '  child log: /var/log/raph-installer/phase2.log\n' >&2 ;;
      *bootstrap-host*|*install-docker*|*install-gw0*|*create-shared-volume*)
        printf '  child log: stderr only — re-run with bash -x to trace\n' >&2 ;;
      *) ;;
    esac
    printf '================================================================\n' >&2
    exit "$rc"
  fi
}

# Atomic install of secret material from stdin to PATH at MODE, root:root.
# Verifies the on-disk mode after install. Use for jwt-secret etc.
install_secret_file() {
  local mode="$1" path="$2"
  local dir tmp
  dir="$(dirname "$path")"
  install -d -m 0700 -o root -g root "$dir"   # why: secrets parent dir must be 0700
  tmp="$(mktemp "${dir}/.tmp.$(basename "$path").XXXXXX")"
  # stdin -> tmp, then atomic rename onto target.
  cat > "$tmp"
  chmod "$mode" "$tmp"
  chown root:root "$tmp"
  mv -f "$tmp" "$path"
  local got
  got="$(stat -c %a "$path")"
  if [[ "$got" != "${mode#0}" && "$got" != "$mode" ]]; then
    printf 'install_secret_file: %s mode is %s but expected %s\n' "$path" "$got" "$mode" >&2
    exit 1
  fi
}
