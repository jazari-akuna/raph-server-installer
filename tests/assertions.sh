#!/usr/bin/env bash
# assertions.sh — assertion library for the raph-server-installer test
# harness. Sourced by scenarios.sh and run-tests.sh.
#
# Each assert_* function:
#   - Returns 0 on pass, 1 on fail.
#   - Prints `PASS: <description>` or `FAIL: <description> — <reason>`.
#   - Increments ASSERT_PASS / ASSERT_FAIL.
#
# A scenario function bundles a set of assert_* calls under a `begin_scenario
# <name>` / `end_scenario` pair so summary reporting can be per-scenario.
#
# At end-of-run, summarise() prints `<n> PASSED, <m> FAILED` and returns the
# FAIL count so the caller can `exit $?`.

ASSERT_PASS=0
ASSERT_FAIL=0
SCENARIO_NAME=""
SCENARIO_PASS=0
SCENARIO_FAIL=0
TESTS_VERBOSE="${TESTS_VERBOSE:-0}"

# ---- internal helpers ----------------------------------------------------

_pass() {
  ASSERT_PASS=$((ASSERT_PASS+1))
  SCENARIO_PASS=$((SCENARIO_PASS+1))
  # Use a literal-arg form of printf so messages starting with `--` (e.g.
  # callers passing dashed flags through $*) don't get parsed by printf.
  printf '%s\n' "PASS: $*"
}

_fail() {
  ASSERT_FAIL=$((ASSERT_FAIL+1))
  SCENARIO_FAIL=$((SCENARIO_FAIL+1))
  printf '%s\n' "FAIL: $*"
}

begin_scenario() {
  SCENARIO_NAME="$1"
  SCENARIO_PASS=0
  SCENARIO_FAIL=0
  printf '\n--- scenario: %s ---\n' "$SCENARIO_NAME"
}

end_scenario() {
  printf '--- end %s: %d passed, %d failed ---\n' \
    "$SCENARIO_NAME" "$SCENARIO_PASS" "$SCENARIO_FAIL"
}

summarise() {
  printf '\n========================================\n'
  printf '%d PASSED, %d FAILED\n' "$ASSERT_PASS" "$ASSERT_FAIL"
  printf '========================================\n'
  return "$ASSERT_FAIL"
}

# ---- file existence / mode / owner --------------------------------------

assert_file_exists() {
  local path="$1" desc="${2:-file exists: $1}"
  if [[ -e "$path" ]]; then
    _pass "$desc"
  else
    _fail "$desc — path missing: $path"
  fi
}

assert_file_absent() {
  local path="$1" desc="${2:-file absent: $1}"
  if [[ ! -e "$path" ]]; then
    _pass "$desc"
  else
    _fail "$desc — path unexpectedly present: $path"
  fi
}

assert_file_mode() {
  local path="$1" want="$2" desc="${3:-mode $2: $1}"
  if [[ ! -e "$path" ]]; then
    _fail "$desc — path missing: $path"
    return 1
  fi
  local got
  got="$(stat -c '%a' "$path")"
  # Accept caller-supplied mode with or without leading zero (0600 == 600).
  local want_n="${want#0}"
  local got_n="${got#0}"
  if [[ "$got_n" == "$want_n" ]]; then
    _pass "$desc"
  else
    _fail "$desc — got mode $got, wanted $want"
  fi
}

assert_file_owner() {
  local path="$1" want="$2" desc="${3:-owner $2: $1}"
  if [[ ! -e "$path" ]]; then
    _fail "$desc — path missing: $path"
    return 1
  fi
  local got
  got="$(stat -c '%U' "$path")"
  if [[ "$got" == "$want" ]]; then
    _pass "$desc"
  else
    _fail "$desc — owner is $got, wanted $want"
  fi
}

# ---- systemd unit assertions --------------------------------------------

assert_systemd_unit_present() {
  local unit="$1" desc="${2:-unit present: $1}"
  local path="/etc/systemd/system/$unit"
  if [[ -f "$path" ]]; then
    _pass "$desc"
  else
    _fail "$desc — $path missing"
  fi
}

assert_systemd_unit_enabled() {
  local unit="$1" desc="${2:-unit enabled: $1}"
  if systemctl is-enabled "$unit" 2>/dev/null | grep -qx enabled; then
    _pass "$desc"
  else
    _fail "$desc — systemctl is-enabled $unit returned $(systemctl is-enabled "$unit" 2>&1 | head -1)"
  fi
}

# ---- env file / template assertions -------------------------------------

# assert_env_in_file VAR EXPECTED FILE [DESC]
# Greps for `VAR=EXPECTED` in FILE. Tolerates the value being unquoted,
# single-quoted, or double-quoted (compose .env spec accepts all three;
# bootstrap.sh writes single-quoted values to make the file safe to
# source under `set -u`).
assert_env_in_file() {
  local key="$1" want="$2" file="$3"
  local desc="${4:-env $key=$want in $(basename "$file")}"
  if [[ ! -f "$file" ]]; then
    _fail "$desc — file missing: $file"
    return 1
  fi
  # Match bare value, single-quoted value, or double-quoted value.
  if grep -qE "^${key}=(${want}|'${want}'|\"${want}\")\$" "$file"; then
    _pass "$desc"
  else
    local actual
    actual="$(grep -E "^${key}=" "$file" | head -1 || true)"
    _fail "$desc — actual line: ${actual:-<absent>}"
  fi
}

# assert_template_rendered FILE [DESC]
# Verifies that no `${VAR}` placeholder remains in FILE (anything matching
# the standard envsubst form). Bare `$word` (nginx vars) is allowed.
assert_template_rendered() {
  local file="$1" desc="${2:-no \${VAR} placeholders left in $(basename "$1")}"
  if [[ ! -f "$file" ]]; then
    _fail "$desc — file missing: $file"
    return 1
  fi
  if grep -qE '\$\{[A-Z_][A-Z0-9_]*\}' "$file"; then
    local sample
    sample="$(grep -nE '\$\{[A-Z_][A-Z0-9_]*\}' "$file" | head -3)"
    _fail "$desc — leftover placeholders:
$sample"
  else
    _pass "$desc"
  fi
}

# ---- HTTP / token assertions --------------------------------------------

assert_http_status() {
  local url="$1" want="$2" desc="${3:-HTTP $2 from $1}"
  local got
  got="$(curl -fsS -o /dev/null -w '%{http_code}' --max-time 5 "$url" 2>/dev/null \
    || curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$url" 2>/dev/null \
    || echo '000')"
  if [[ "$got" == "$want" ]]; then
    _pass "$desc"
  else
    _fail "$desc — got HTTP $got"
  fi
}

# assert_setup_token_format [PATH]
# Default path /etc/raph-installer/setup-token. Verifies 32 chars, [A-Za-z0-9].
assert_setup_token_format() {
  local path="${1:-/etc/raph-installer/setup-token}"
  local desc="setup token: 32 alphanumeric chars at $path"
  if [[ ! -f "$path" ]]; then
    _fail "$desc — token file missing"
    return 1
  fi
  local tok
  tok="$(tr -d '\n' < "$path")"
  if [[ ${#tok} -ne 32 ]]; then
    _fail "$desc — length is ${#tok}, wanted 32"
    return 1
  fi
  if [[ ! "$tok" =~ ^[A-Za-z0-9]{32}$ ]]; then
    _fail "$desc — non-alphanumeric chars present"
    return 1
  fi
  _pass "$desc"
}

# ---- log assertions -----------------------------------------------------

assert_log_contains() {
  local file="$1" pat="$2" desc="${3:-log contains: $2}"
  if [[ ! -f "$file" ]]; then
    _fail "$desc — log missing: $file"
    return 1
  fi
  if grep -qF -- "$pat" "$file"; then
    _pass "$desc"
  else
    _fail "$desc — pattern not found in $file"
  fi
}

assert_log_not_contains() {
  local file="$1" pat="$2" desc="${3:-log does NOT contain: $2}"
  if [[ ! -f "$file" ]]; then
    _fail "$desc — log missing: $file"
    return 1
  fi
  if grep -qF -- "$pat" "$file"; then
    local hit
    hit="$(grep -F -- "$pat" "$file" | head -3)"
    _fail "$desc — pattern present:
$hit"
  else
    _pass "$desc"
  fi
}

# assert_log_matches FILE PATTERN [DESC] — regex form (egrep).
assert_log_matches() {
  local file="$1" pat="$2" desc="${3:-log matches: $2}"
  if [[ ! -f "$file" ]]; then
    _fail "$desc — log missing: $file"
    return 1
  fi
  if grep -qE -- "$pat" "$file"; then
    _pass "$desc"
  else
    _fail "$desc — regex not found in $file"
  fi
}

# Detect whether a script's expected stub commands were invoked. Reads
# /tmp/test-stubs.log (configurable via TEST_STUB_LOG).
assert_stub_invoked() {
  local pat="$1" desc="${2:-stub invoked: $1}"
  local log="${TEST_STUB_LOG:-/tmp/test-stubs.log}"
  assert_log_contains "$log" "$pat" "$desc"
}

assert_stub_not_invoked() {
  local pat="$1" desc="${2:-stub NOT invoked: $1}"
  local log="${TEST_STUB_LOG:-/tmp/test-stubs.log}"
  assert_log_not_contains "$log" "$pat" "$desc"
}
