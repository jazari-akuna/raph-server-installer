#!/usr/bin/env bash
# smoke-test.sh — programmatic verification runner for raph-server-installer.
#
# Mirrors docs/verification.md as code. Designed to be run from the
# operator's laptop (not on the VPS): laptop-side checks hit public DNS
# and HTTPS endpoints, host-side probes are batched into a single SSH
# call to keep total runtime ~5s instead of ~30s.
#
# Output: one line per check, prefixed PASS / FAIL / SKIP. Exit code is
# the count of FAILs, so cron / CI can branch on it.
#
# Usage:
#   VPS_HOST=foo.example VPS_USER=<admin> DOMAIN=foo.example ./scripts/smoke-test.sh
#
# Environment (all required unless noted):
#   VPS_HOST           SSH-reachable hostname or IP of the VPS.
#   VPS_USER           SSH login username on the VPS.
#   DOMAIN             Apex domain the installer is hosting on (e.g. example.com).
#                      Used to derive subdomain hosts (auth, gw, cdn, cloud, ...).
#   EXPECTED_IP        Public IPv4 the apex/wildcard records should resolve to.
#                      If unset, the script skips DNS-IP equality checks.
#   PEER_SUBNET        AWG peer subnet for NAT MASQUERADE rule check.
#                      Default: 10.99.0.0/24
#   CHNROUTES_PATH     Filesystem path to the chnroutes cache file on the VPS.
#                      Default: /var/cache/raph/chnroutes.txt
#   RESTIC_REPOSITORY  If set, Section 6 actually runs. If unset, skipped.
#
# Conventions:
#   - set -uo pipefail (NOT -e): one failing check must not short-circuit
#     the rest. Each check is wrapped in a function that captures errors.
#   - ssh uses BatchMode=yes ConnectTimeout=5 — no interactive prompts.
#   - Sensitive material (OVH creds, TOTP secrets, NPM admin password) is
#     NEVER read or echoed. This script only probes externally-observable
#     state and host status via sudo NOPASSWD commands already in place.

set -uo pipefail

# ---------------------------------------------------------------------------
# config
# ---------------------------------------------------------------------------

VPS_HOST="${VPS_HOST:-}"
VPS_USER="${VPS_USER:-}"
APEX="${DOMAIN:-}"
EXPECTED_IP="${EXPECTED_IP:-}"
PEER_SUBNET="${PEER_SUBNET:-10.99.0.0/24}"
CHNROUTES_PATH="${CHNROUTES_PATH:-/var/cache/raph/chnroutes.txt}"

if [[ -z "$VPS_HOST" || -z "$VPS_USER" || -z "$APEX" ]]; then
    echo "smoke-test.sh: VPS_HOST, VPS_USER, and DOMAIN are required env vars." >&2
    exit 2
fi

WILDCARD_HOST="anything.${APEX}"
GW_HOST="gw.${APEX}"
CDN_HOST="cdn.${APEX}"
CLOUD_HOST="cloud.${APEX}"
CONSOLE_HOST="console.${APEX}"
ENROL_HOST="enrol.${APEX}"
AUTH_HOST="auth.${APEX}"
EXPECTED_CN="*.${APEX}"

# ssh options reused everywhere
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new)

# ---------------------------------------------------------------------------
# tally + reporting helpers
# ---------------------------------------------------------------------------

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

pass() {
    printf '  PASS: %s\n' "$1"
    PASS_COUNT=$((PASS_COUNT + 1))
}

fail() {
    printf '  FAIL: %s — %s\n' "$1" "$2"
    FAIL_COUNT=$((FAIL_COUNT + 1))
}

skip() {
    printf '  SKIP: %s%s\n' "$1" "${2:+ ($2)}"
    SKIP_COUNT=$((SKIP_COUNT + 1))
}

section() {
    printf '\n[%s] %s\n' "$1" "$2"
}

# ---------------------------------------------------------------------------
# Section 1 — DNS (laptop-side only)
# ---------------------------------------------------------------------------

dns_check() {
    local name="$1" host="$2"
    local got
    got="$(dig @1.1.1.1 +short +time=3 +tries=2 "$host" 2>/dev/null | grep -E '^[0-9]+\.' | head -1)"
    if [[ -z "$EXPECTED_IP" ]]; then
        # No expected IP supplied — assert only that *something* resolves.
        if [[ -n "$got" ]]; then
            pass "$name resolves to $got"
        else
            fail "$name" "no A record returned"
        fi
        return
    fi
    if [[ "$got" == "$EXPECTED_IP" ]]; then
        pass "$name resolves to $EXPECTED_IP"
    else
        fail "$name" "expected $EXPECTED_IP, got '${got:-<empty>}'"
    fi
}

run_dns_section() {
    section "1/7" "DNS"
    dns_check "apex"     "$APEX"
    dns_check "wildcard" "$WILDCARD_HOST"
    dns_check "gw"       "$GW_HOST"
    dns_check "cdn"      "$CDN_HOST"
}

# ---------------------------------------------------------------------------
# Section 2 — public HTTPS (laptop-side)
# ---------------------------------------------------------------------------

# Verify host returns HTTP 302 with Location pointing at the auth portal.
auth_redirect_check() {
    local host="$1"
    local headers
    headers="$(curl -sI -m 10 "https://${host}" 2>/dev/null || true)"
    if [[ -z "$headers" ]]; then
        fail "$host auth-portal redirect" "no response from https://${host}"
        return
    fi
    # First status line: HTTP/<v> 302
    local status
    status="$(printf '%s\n' "$headers" | awk 'NR==1 {print $2}')"
    local location
    location="$(printf '%s\n' "$headers" | awk 'tolower($1)=="location:" {print $2}' | tr -d '\r')"
    if [[ "$status" == "302" ]] && [[ "$location" == https://${AUTH_HOST}/* ]]; then
        pass "$host -> auth portal redirect (302)"
    else
        fail "$host auth-portal redirect" "status='${status:-?}' location='${location:-?}'"
    fi
}

auth_health_check() {
    local code
    code="$(curl -sI -m 10 -o /dev/null -w '%{http_code}' "https://${AUTH_HOST}/api/health" 2>/dev/null || true)"
    if [[ "$code" == "200" ]]; then
        pass "auth /api/health (200)"
    else
        fail "auth /api/health" "expected 200, got '${code:-<none>}'"
    fi
}

cert_validity_check() {
    local cert
    # checkend 2592000 = 30 days
    cert="$(openssl s_client -connect "${CLOUD_HOST}:443" -servername "${CLOUD_HOST}" \
        </dev/null 2>/dev/null \
        | openssl x509 -noout -checkend 2592000 2>/dev/null; echo "rc=$?")"
    local rc="${cert##*rc=}"
    if [[ "$rc" == "0" ]]; then
        pass "cert valid for >30 days"
    else
        fail "cert validity" "openssl x509 -checkend 2592000 returned rc=$rc"
    fi
}

cert_cn_check() {
    local subject
    subject="$(openssl s_client -connect "${CLOUD_HOST}:443" -servername "${CLOUD_HOST}" \
        </dev/null 2>/dev/null \
        | openssl x509 -noout -subject 2>/dev/null || true)"
    # subject example: subject=CN = *.<your-domain>
    if [[ "$subject" == *"$EXPECTED_CN"* ]]; then
        pass "cert CN is ${EXPECTED_CN}"
    else
        fail "cert CN" "expected '${EXPECTED_CN}' in subject, got '${subject:-<none>}'"
    fi
}

run_https_section() {
    section "2/7" "HTTPS"
    auth_redirect_check "$CLOUD_HOST"
    auth_redirect_check "$ENROL_HOST"
    auth_redirect_check "$CONSOLE_HOST"
    auth_health_check
    cert_validity_check
    cert_cn_check
}

# ---------------------------------------------------------------------------
# Sections 3-5, 7 — host-side probes (single batched SSH)
# ---------------------------------------------------------------------------
#
# We run one SSH session that emits delimited blocks for each probe; the
# laptop-side parser extracts each block by name and asserts on it.
#
# Block format:
#   <<<BEGIN:probe-name>>>
#   ...output...
#   <<<END:probe-name>>>
#
# Probes that fail to run (e.g. binary missing) still produce their
# delimiters so the parser never blocks on a missing block.

HOST_REPORT=""

# Strip the wrapping delimiters and return a probe block's body.
extract_block() {
    local name="$1"
    awk -v n="$name" '
        $0 == "<<<BEGIN:" n ">>>" { capture = 1; next }
        $0 == "<<<END:"   n ">>>" { capture = 0; next }
        capture { print }
    ' <<<"$HOST_REPORT"
}

# Build the remote probe script as a single string in a HEREDOC, then pipe
# it over ssh. Doing the heredoc here (instead of inline inside the `if`
# condition) sidesteps a parser corner case in older bash where a heredoc
# inside `$(...)` inside an `if !` produces a warning.
build_remote_script() {
    cat <<'REMOTE'
set -u

# Wrap a probe: print BEGIN, run a command, print END. Probe failures are
# captured in the block (never propagate) so one bad probe doesn't kill
# the others.
probe() {
    local name="$1"; shift
    printf '<<<BEGIN:%s>>>\n' "$name"
    "$@" 2>&1 || true
    printf '<<<END:%s>>>\n' "$name"
}

# Section 3 — gateway
probe awg_show         sudo awg show gw0
probe ss_udp_gw0       bash -c 'sudo ss -uln 2>/dev/null | grep -cE "[[:space:]](0\.0\.0\.0|\\*|\\[::\\]):443([[:space:]]|\$)" || true'
probe iptables_nat     bash -c 'sudo iptables -t nat -S POSTROUTING 2>/dev/null | grep -c "10.99.0.0/24.*MASQUERADE" || true'
probe gw0_conf_peers   bash -c 'sudo grep -c "^\[Peer\]" /etc/amnezia/amneziawg/gw0.conf 2>/dev/null || echo 0'
probe awg_dump_peers   bash -c 'sudo awg show gw0 dump 2>/dev/null | tail -n +2 | wc -l'

# Section 4 — Docker stacks (one ps per stack)
for stack in ingress console cloud authelia enrol qedge; do
    probe "compose_${stack}" sudo docker compose -f "/opt/stacks/${stack}/docker-compose.yml" ps --format json
done

# Container healthcheck status (only the running ones we care about).
# `docker inspect` fails the template eval when .State.Health is absent
# (no healthcheck defined). Detect this with a leading existence check
# and emit "<no-healthcheck>" so the laptop-side parser can skip it.
probe container_health bash -c '
for c in ingress console cloud authelia enrol; do
    if ! sudo docker inspect "$c" >/dev/null 2>&1; then
        printf "%s\t%s\n" "$c" "<no-container>"
        continue
    fi
    has=$(sudo docker inspect -f "{{if .State.Health}}yes{{else}}no{{end}}" "$c" 2>/dev/null)
    if [[ "$has" != "yes" ]]; then
        printf "%s\t%s\n" "$c" "<no-healthcheck>"
        continue
    fi
    state=$(sudo docker inspect -f "{{.State.Health.Status}}" "$c" 2>/dev/null)
    printf "%s\t%s\n" "$c" "${state:-<unknown>}"
done
'

# Section 5 — host
probe free_used_mb     bash -c 'free -m | awk "/^Mem:/ {print \$3}"'
probe df_root_pct      bash -c 'df -h / | awk "NR==2 {print \$5}" | tr -d %'
probe swap_used_kb     bash -c 'free -k | awk "/^Swap:/ {print \$3}"'
probe running_kernel   uname -r
probe dkms_status      bash -c 'sudo dkms status amneziawg 2>/dev/null || true'
probe systemctl_active bash -c '
for u in awg-quick@gw0 tailscaled fail2ban gw0-nat.service; do
    state="$(sudo systemctl is-active "$u" 2>&1 || true)"
    printf "%s\t%s\n" "$u" "$state"
done
# store-mount@.service is a template — list any active instances.
inst="$(sudo systemctl list-units --type=service --state=active --no-legend "store-mount@*.service" 2>/dev/null | awk "{print \$1}" | tr "\n" "," | sed "s/,\$//")"
printf "store-mount-instances\t%s\n" "${inst:-<none-active>}"
'
probe fail2ban_banned  bash -c 'sudo fail2ban-client status sshd 2>/dev/null | awk -F: "/Total banned/ {gsub(/ /,\"\",\$2); print \$2}" || echo 0'

# Section 7 — chnroutes2 cache freshness. Path is injected by laptop side.
probe chnroutes_cache  bash -c "sudo find ${__CHNROUTES_PATH__} -mtime -35 2>/dev/null || true"
REMOTE
}

run_host_probes() {
    local script
    script="$(build_remote_script)"
    # Substitute laptop-side env into the remote script (single token).
    script="${script//__CHNROUTES_PATH__/${CHNROUTES_PATH}}"
    HOST_REPORT="$(ssh "${SSH_OPTS[@]}" "${VPS_USER}@${VPS_HOST}" 'bash -s' \
        <<<"$script" 2>/dev/null)" || return 1
    return 0
}

# --- gateway parsers ---------------------------------------------------------

check_awg_show() {
    local out first
    out="$(extract_block awg_show)"
    first="$(printf '%s\n' "$out" | head -1)"
    if [[ "$first" == "interface: gw0" ]]; then
        pass "awg show gw0 (interface up)"
    else
        fail "awg show gw0" "expected 'interface: gw0', got '${first:-<empty>}'"
    fi
}

check_udp_listen() {
    local out
    out="$(extract_block ss_udp_gw0 | tail -1 | tr -d '[:space:]')"
    if [[ "$out" =~ ^[0-9]+$ ]] && (( out >= 1 )); then
        pass "udp/443 gw0 listener (count=$out)"
    else
        fail "udp/443 gw0 listener" "expected >=1 listener, got '${out:-<empty>}'"
    fi
}

check_nat_rule() {
    local out
    out="$(extract_block iptables_nat | tail -1 | tr -d '[:space:]')"
    if [[ "$out" == "1" ]]; then
        pass "NAT MASQUERADE rule for ${PEER_SUBNET}"
    else
        fail "NAT MASQUERADE rule" "expected count=1, got '${out:-<empty>}'"
    fi
}

check_peer_consistency() {
    local conf_count dump_count
    conf_count="$(extract_block gw0_conf_peers | tail -1 | tr -d '[:space:]')"
    dump_count="$(extract_block awg_dump_peers | tail -1 | tr -d '[:space:]')"
    if [[ "$conf_count" =~ ^[0-9]+$ ]] && [[ "$dump_count" =~ ^[0-9]+$ ]] \
        && [[ "$conf_count" == "$dump_count" ]]; then
        pass "peer count consistent (conf=$conf_count, runtime=$dump_count)"
    else
        fail "peer count drift" "conf=${conf_count:-?} runtime=${dump_count:-?}"
    fi
}

run_gateway_section() {
    section "3/7" "Gateway"
    check_awg_show
    check_udp_listen
    check_nat_rule
    check_peer_consistency
}

# --- Docker stack parsers ----------------------------------------------------

# Parse `docker compose ps --format json` output. Compose v2 emits one JSON
# object per line (NDJSON) — newer versions emit a single JSON array.
# Be permissive: search the raw output for "State":"running" occurrences.
compose_state_running() {
    local block="$1"
    if printf '%s' "$block" | grep -q '"State":"running"'; then
        return 0
    fi
    return 1
}

compose_has_any_container() {
    local block="$1"
    # Empty result for a down stack is `[]` or no lines.
    if [[ -z "$(printf '%s' "$block" | tr -d '[:space:]')" ]] \
        || [[ "$(printf '%s' "$block" | tr -d '[:space:]')" == "[]" ]]; then
        return 1
    fi
    return 0
}

check_stack_running() {
    local stack="$1"
    local block
    block="$(extract_block "compose_${stack}")"
    if compose_state_running "$block"; then
        pass "${stack} running"
    else
        fail "${stack}" "no running container in compose ps"
    fi
}

check_stack_stopped() {
    local stack="$1"
    local block
    block="$(extract_block "compose_${stack}")"
    if compose_state_running "$block"; then
        fail "${stack} (expected stopped)" "container is running"
    else
        pass "${stack} stopped (by design)"
    fi
}

check_container_healthchecks() {
    local block name state
    block="$(extract_block container_health)"
    while IFS=$'\t' read -r name state; do
        [[ -z "$name" ]] && continue
        case "$state" in
            healthy)
                pass "${name} healthcheck=healthy"
                ;;
            "<no-healthcheck>"|"<no-container>")
                # No healthcheck defined, or the container isn't there yet.
                # qedge intentionally has no container running — silent skip.
                if [[ "$state" == "<no-container>" ]]; then
                    fail "${name} healthcheck" "container not found"
                else
                    skip "${name} healthcheck" "not defined"
                fi
                ;;
            starting|unhealthy|*)
                fail "${name} healthcheck" "state=${state}"
                ;;
        esac
    done <<<"$block"
}

run_docker_section() {
    section "4/7" "Docker stacks"
    for stack in ingress console cloud authelia enrol; do
        check_stack_running "$stack"
    done
    check_stack_stopped "qedge"
    check_container_healthchecks
}

# --- host parsers ------------------------------------------------------------

check_ram() {
    local used
    used="$(extract_block free_used_mb | tail -1 | tr -d '[:space:]')"
    if [[ "$used" =~ ^[0-9]+$ ]]; then
        if (( used <= 3000 )); then
            pass "RAM used: ${used} MB (cap 3000)"
        else
            fail "RAM" "${used} MB used (cap 3000)"
        fi
    else
        fail "RAM" "could not parse: '${used:-<empty>}'"
    fi
}

check_disk() {
    local pct
    pct="$(extract_block df_root_pct | tail -1 | tr -d '[:space:]')"
    if [[ "$pct" =~ ^[0-9]+$ ]]; then
        if (( pct <= 80 )); then
            pass "disk /: ${pct}% used (cap 80%)"
        else
            fail "disk /" "${pct}% used (cap 80%)"
        fi
    else
        fail "disk /" "could not parse: '${pct:-<empty>}'"
    fi
}

check_swap() {
    local kb mb
    kb="$(extract_block swap_used_kb | tail -1 | tr -d '[:space:]')"
    if [[ "$kb" =~ ^[0-9]+$ ]]; then
        mb=$((kb / 1024))
        if (( mb < 1024 )); then
            pass "swap in use: ${mb} MB (<1 GB)"
        else
            fail "swap" "${mb} MB in use (>=1 GB suggests RAM pressure)"
        fi
    else
        fail "swap" "could not parse: '${kb:-<empty>}'"
    fi
}

check_dkms() {
    local kernel status
    kernel="$(extract_block running_kernel | tail -1 | tr -d '[:space:]')"
    status="$(extract_block dkms_status)"
    if [[ -z "$kernel" ]]; then
        fail "DKMS amneziawg" "could not determine running kernel"
        return
    fi
    # Match either "amneziawg, X.Y.Z, KERNEL, ARCH: installed" (older format)
    # or "amneziawg/X.Y.Z, KERNEL, ARCH: installed" (newer format).
    local count
    count="$(printf '%s\n' "$status" | grep -c ", ${kernel}, .*: installed" || true)"
    if [[ "$count" =~ ^[0-9]+$ ]] && (( count >= 1 )); then
        pass "DKMS amneziawg installed for ${kernel}"
    else
        fail "DKMS amneziawg" "no 'installed' entry for kernel ${kernel}"
    fi
}

check_systemd_units() {
    local block name state
    block="$(extract_block systemctl_active)"
    while IFS=$'\t' read -r name state; do
        [[ -z "$name" ]] && continue
        case "$name" in
            store-mount-instances)
                # Informational — store volumes can legitimately be locked.
                if [[ "$state" == "<none-active>" ]]; then
                    skip "store-mount instances" "no active instances (volumes locked)"
                else
                    pass "store-mount instances active: ${state}"
                fi
                ;;
            *)
                if [[ "$state" == "active" ]]; then
                    pass "${name} active"
                else
                    fail "${name}" "is-active=${state}"
                fi
                ;;
        esac
    done <<<"$block"
}

check_fail2ban() {
    local n
    n="$(extract_block fail2ban_banned | tail -1 | tr -d '[:space:]')"
    if [[ "$n" =~ ^[0-9]+$ ]]; then
        if (( n <= 100 )); then
            pass "fail2ban total banned: ${n} (<=100)"
        else
            fail "fail2ban" "total banned ${n} >100 (possible attack)"
        fi
    else
        # Empty/non-numeric — fail2ban may not be installed; soft-fail
        # with the raw value so the operator can disambiguate.
        fail "fail2ban" "could not parse total banned: '${n:-<empty>}'"
    fi
}

run_host_section() {
    section "5/7" "Host"
    check_ram
    check_disk
    check_swap
    check_dkms
    check_systemd_units
    check_fail2ban
}

# ---------------------------------------------------------------------------
# Section 6 — backups (laptop-side)
# ---------------------------------------------------------------------------

run_backups_section() {
    section "6/7" "Backups"
    if [[ -z "${RESTIC_REPOSITORY:-}" ]]; then
        skip "restic snapshots" "RESTIC_REPOSITORY unset"
        return
    fi
    if ! command -v restic >/dev/null 2>&1; then
        skip "restic snapshots" "restic binary not on PATH"
        return
    fi
    local json
    json="$(restic snapshots --json 2>/dev/null || true)"
    if [[ -z "$json" ]] || [[ "$json" == "[]" ]]; then
        fail "restic snapshots" "no snapshots in repository"
        return
    fi
    # Find the newest snapshot timestamp; compare to (now - 7d).
    local newest_ts now_ts cutoff_ts
    if command -v jq >/dev/null 2>&1; then
        newest_ts="$(printf '%s' "$json" | jq -r 'map(.time) | sort | last' 2>/dev/null || true)"
    else
        # Fall back to grep. ISO-8601 sorts lexically.
        newest_ts="$(printf '%s' "$json" | grep -oE '"time":"[^"]+"' | sed 's/"time":"//;s/"//' | sort | tail -1)"
    fi
    if [[ -z "$newest_ts" ]]; then
        fail "restic snapshots" "could not parse snapshot timestamps"
        return
    fi
    if ! newest_ts="$(date -d "$newest_ts" +%s 2>/dev/null)"; then
        fail "restic snapshots" "could not convert timestamp '${newest_ts}'"
        return
    fi
    now_ts="$(date +%s)"
    cutoff_ts=$((now_ts - 7 * 86400))
    if (( newest_ts >= cutoff_ts )); then
        pass "newest restic snapshot within 7 days"
    else
        local age_days=$(( (now_ts - newest_ts) / 86400 ))
        fail "restic snapshots" "newest snapshot is ${age_days} days old (>7d)"
    fi
}

# ---------------------------------------------------------------------------
# Section 7 — chnroutes2 cache
# ---------------------------------------------------------------------------

run_chnroutes_section() {
    section "7/7" "chnroutes2"
    local out
    out="$(extract_block chnroutes_cache | tr -d '[:space:]')"
    if [[ "$out" == "${CHNROUTES_PATH}" ]]; then
        pass "chnroutes.txt fresh (<35 days)"
    else
        fail "chnroutes.txt" "missing or stale (>35 days) — refresh via update-route-tables.sh"
    fi
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

main() {
    printf '=== raph-server-installer smoke test ===\n'
    printf 'target: %s@%s\n' "$VPS_USER" "$VPS_HOST"

    run_dns_section
    run_https_section

    section "host probes" "(batched ssh)"
    if run_host_probes; then
        printf '  ok: ssh probe collected\n'
        run_gateway_section
        run_docker_section
        run_host_section
        run_chnroutes_section
    else
        # Mark every host-side check as failed (count: 4 gateway + 6 stacks
        # + 5 healthchecks + 6 host + 1 chnroutes = ~22). We don't try to
        # be exact — one big FAIL is informative enough.
        fail "ssh probe" "ssh ${VPS_USER}@${VPS_HOST} failed (BatchMode, ConnectTimeout=5)"
        fail "Section 3 gateway" "skipped — ssh probe failed"
        fail "Section 4 docker stacks" "skipped — ssh probe failed"
        fail "Section 5 host" "skipped — ssh probe failed"
        fail "Section 7 chnroutes2" "skipped — ssh probe failed"
    fi

    run_backups_section

    printf '\nSummary: %d PASS, %d FAIL, %d SKIP\n' "$PASS_COUNT" "$FAIL_COUNT" "$SKIP_COUNT"
    printf 'Exit code: %d\n' "$FAIL_COUNT"
    exit "$FAIL_COUNT"
}

main "$@"
