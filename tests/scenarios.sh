#!/usr/bin/env bash
# scenarios.sh — concrete test scenarios for the raph-server-installer
# harness. Sourced by entrypoint.sh; each test_* function is a self-contained
# scenario invoked from run_all().
#
# Conventions:
#   - Tests assume TEST_MODE=1 (set in the Dockerfile).
#   - Tests use DOMAIN=vps.example.com, ADMIN_EMAIL=alice@example.com.
#   - Each test resets the on-disk state it cares about (sentinels, env file)
#     before running, but does NOT teardown shared infra (that's the harness's
#     job between containers).
#   - Output of each phase 1/2 invocation is captured under
#     /tmp/test-runs/<scenario>/.

# Sourced by entrypoint.sh which has already sourced assertions.sh.

REPO_SRC="${TEST_REPO_SRC:-/opt/raph-server-installer-src}"
ENV_FILE="/opt/stacks/.env"
PHASE1_DONE="/srv/store/.bootstrap-phase1-complete"
PHASE2_DONE="/srv/store/.bootstrap-phase2-complete"
REBOOT_SENTINEL="/srv/store/.test-reboot-requested"
REBOOT_LEAK_SENTINEL="/srv/store/.test-reboot-requested-via-stub"
TOKEN_FILE="/etc/raph-installer/setup-token"
COMPOSE_LOG="${TEST_COMPOSE_LOG:-/tmp/test-compose.log}"
STUB_LOG="${TEST_STUB_LOG:-/tmp/test-stubs.log}"
RUN_DIR_BASE="/tmp/test-runs"

DOMAIN="vps.example.com"
ADMIN_EMAIL="alice@example.com"
ADMIN_USERS="alice"

# ---- helpers ------------------------------------------------------------

# reset_state — wipe everything the bootstrap writes so the next scenario
# starts clean. Touches /opt/raph-server-installer-src/stacks/.env (the
# bind-mount; safe because .env is gitignored) so the symlinked
# /opt/stacks/.env actually disappears between runs.
reset_state() {
  rm -rf /srv/store /etc/raph-installer /var/log/raph-installer
  # /opt/stacks is a symlink into the bind mount; rm -rf on a symlink only
  # removes the symlink itself. Belt-and-braces remove the underlying .env
  # via the bind mount path so the next bootstrap regenerates it.
  rm -f "$REPO_SRC/stacks/.env"
  rm -rf /opt/stacks /opt/raph-server-installer
  rm -rf /var/lib/test-systemctl
  rm -f /tmp/test-stubs.log /tmp/test-compose.log
  # Ensure the host's /etc files we write are clean.
  rm -f /etc/server-domain /etc/server-admin-email
  # Recreate dirs the Dockerfile pre-created.
  mkdir -p /srv/store /var/log/raph-installer /etc/raph-installer
  : > "$STUB_LOG"
  : > "$COMPOSE_LOG"
}

# run_phase1 SCENARIO [EXTRA_ENV...]
# Invokes bootstrap.sh under TEST_MODE with the harness DOMAIN/ADMIN_EMAIL.
# Captures stdout+stderr to $RUN_DIR/phase1.log. Returns the script's exit code.
run_phase1() {
  local scenario="$1"; shift
  local run_dir="$RUN_DIR_BASE/$scenario"
  mkdir -p "$run_dir"
  local rc=0
  if [[ "$TESTS_VERBOSE" == "1" ]]; then
    env TEST_MODE=1 \
        TEST_REPO_SRC="$REPO_SRC" \
        TEST_STUB_LOG="$STUB_LOG" \
        TEST_COMPOSE_LOG="$COMPOSE_LOG" \
        DOMAIN="$DOMAIN" \
        ADMIN_EMAIL="$ADMIN_EMAIL" \
        ADMIN_USERS="$ADMIN_USERS" \
        "$@" \
        bash "$REPO_SRC/bootstrap.sh" 2>&1 | tee "$run_dir/phase1.log"
    rc=${PIPESTATUS[0]}
  else
    env TEST_MODE=1 \
        TEST_REPO_SRC="$REPO_SRC" \
        TEST_STUB_LOG="$STUB_LOG" \
        TEST_COMPOSE_LOG="$COMPOSE_LOG" \
        DOMAIN="$DOMAIN" \
        ADMIN_EMAIL="$ADMIN_EMAIL" \
        ADMIN_USERS="$ADMIN_USERS" \
        "$@" \
        bash "$REPO_SRC/bootstrap.sh" >"$run_dir/phase1.log" 2>&1 \
      || rc=$?
  fi
  return "$rc"
}

# run_phase2 SCENARIO [EXTRA_ENV...]
run_phase2() {
  local scenario="$1"; shift
  local run_dir="$RUN_DIR_BASE/$scenario"
  mkdir -p "$run_dir"
  local rc=0
  if [[ "$TESTS_VERBOSE" == "1" ]]; then
    env TEST_MODE=1 \
        TEST_STUB_LOG="$STUB_LOG" \
        TEST_COMPOSE_LOG="$COMPOSE_LOG" \
        REPO_DIR=/opt/raph-server-installer \
        STACKS_DIR=/opt/stacks \
        ENV_FILE=/opt/stacks/.env \
        "$@" \
        bash /opt/raph-server-installer/scripts/bootstrap-phase2.sh 2>&1 | tee "$run_dir/phase2.log"
    rc=${PIPESTATUS[0]}
  else
    env TEST_MODE=1 \
        TEST_STUB_LOG="$STUB_LOG" \
        TEST_COMPOSE_LOG="$COMPOSE_LOG" \
        REPO_DIR=/opt/raph-server-installer \
        STACKS_DIR=/opt/stacks \
        ENV_FILE=/opt/stacks/.env \
        "$@" \
        bash /opt/raph-server-installer/scripts/bootstrap-phase2.sh >"$run_dir/phase2.log" 2>&1 \
      || rc=$?
  fi
  return "$rc"
}

# ---- scenarios ----------------------------------------------------------

test_phase1_happy_path() {
  begin_scenario "phase1_happy_path"
  reset_state

  if ! run_phase1 "phase1_happy_path"; then
    _fail "bootstrap.sh exited non-zero in happy path"
    end_scenario
    return
  fi
  _pass "bootstrap.sh exited 0 in happy path"

  # Sentinels + persistent state.
  assert_file_exists "$PHASE1_DONE" "phase1 sentinel created"
  assert_file_exists "/etc/server-domain" "/etc/server-domain written"
  assert_file_exists "/etc/server-admin-email" "/etc/server-admin-email written"
  assert_file_exists "$ENV_FILE" "/opt/stacks/.env generated"
  assert_file_exists "/etc/systemd/system/bootstrap-continue.service" \
    "bootstrap-continue.service installed"

  # Reboot was simulated, NOT real.
  assert_file_exists "$REBOOT_SENTINEL" "TEST_MODE reboot sentinel created"
  assert_file_absent "$REBOOT_LEAK_SENTINEL" \
    "real systemctl reboot not invoked"

  # Token format.
  assert_setup_token_format

  # Env file content.
  assert_env_in_file "DOMAIN" "$DOMAIN" "$ENV_FILE"
  assert_env_in_file "ADMIN_USERS" "$ADMIN_USERS" "$ENV_FILE"
  assert_env_in_file "ENROL_DOMAIN" "$DOMAIN" "$ENV_FILE"

  # Templates were rendered.
  assert_file_exists "/opt/raph-server-installer/stacks/authelia/configuration.yml" \
    "Authelia configuration.yml rendered"
  assert_template_rendered "/opt/raph-server-installer/stacks/authelia/configuration.yml"

  # The TEST_MODE banner should be in the phase1 log.
  assert_log_contains "$RUN_DIR_BASE/phase1_happy_path/phase1.log" \
    "TEST_MODE=1" "phase1 log advertises TEST_MODE"

  end_scenario
}

test_phase1_missing_domain() {
  begin_scenario "phase1_missing_domain"
  reset_state

  # We can't use run_phase1 here because it positional-assigns DOMAIN before
  # passing --unset; `env` honours the assignment first. Build a bespoke
  # invocation that strips DOMAIN from the inherited env entirely.
  local scenario="phase1_missing_domain"
  local run_dir="$RUN_DIR_BASE/$scenario"
  mkdir -p "$run_dir"
  local rc=0
  env -u DOMAIN \
      TEST_MODE=1 \
      TEST_REPO_SRC="$REPO_SRC" \
      TEST_STUB_LOG="$STUB_LOG" \
      TEST_COMPOSE_LOG="$COMPOSE_LOG" \
      ADMIN_EMAIL="$ADMIN_EMAIL" \
      ADMIN_USERS="$ADMIN_USERS" \
      bash "$REPO_SRC/bootstrap.sh" >"$run_dir/phase1.log" 2>&1 \
    || rc=$?

  if [[ $rc -ne 0 ]]; then
    _pass "bootstrap.sh exited non-zero ($rc) with DOMAIN unset"
  else
    _fail "bootstrap.sh exited 0 with DOMAIN unset (should hard-fail)"
  fi

  # Crucially: NO sentinels touched.
  assert_file_absent "$PHASE1_DONE" \
    "phase1 sentinel NOT created when DOMAIN missing"
  assert_file_absent "$REBOOT_SENTINEL" \
    "reboot NOT requested when DOMAIN missing"

  # Error message in log.
  assert_log_contains "$run_dir/phase1.log" \
    "DOMAIN env var is required" "clear error printed"

  end_scenario
}

test_phase1_idempotent() {
  begin_scenario "phase1_idempotent"
  reset_state

  if ! run_phase1 "phase1_idempotent"; then
    _fail "first bootstrap.sh run failed"
    end_scenario
    return
  fi
  assert_file_exists "$PHASE1_DONE" "first run created sentinel"

  # Capture the env-file mtime so we can detect a regen on re-run.
  local mtime_before
  mtime_before="$(stat -c '%Y' "$ENV_FILE")"
  # Wait one second so any unintended regen would have a fresher mtime.
  sleep 1

  # Re-arm the reboot sentinel by removing it; the second run should re-create
  # it (the script touches it again on re-arm) but should NOT redo the heavy
  # apt/install steps.
  rm -f "$REBOOT_SENTINEL"
  : > "$STUB_LOG"  # so we can detect new stub invocations

  if ! run_phase1 "phase1_idempotent_2"; then
    _fail "second bootstrap.sh run failed"
    end_scenario
    return
  fi
  _pass "second run exited 0"

  # Re-arm path should print the "skipping heavy steps" line.
  assert_log_contains "$RUN_DIR_BASE/phase1_idempotent_2/phase1.log" \
    "Phase 1 sentinel present" "second run took the idempotent short-circuit"

  # The idempotent re-arm path should NOT have re-rendered the env file.
  local mtime_after
  mtime_after="$(stat -c '%Y' "$ENV_FILE")"
  if [[ "$mtime_before" == "$mtime_after" ]]; then
    _pass "env file not regenerated on second run"
  else
    _fail "env file mtime changed (before=$mtime_before after=$mtime_after) on second run"
  fi

  # And the reboot was simulated again.
  assert_file_exists "$REBOOT_SENTINEL" "second run also touched reboot sentinel"

  end_scenario
}

test_render_check_passes() {
  begin_scenario "render_check_passes"
  # No state reset needed — render --check uses /tmp and ignores caller env.
  local rc=0
  if "$REPO_SRC/scripts/render-templates.sh" --check >/tmp/render-check.log 2>&1; then
    _pass "render-templates.sh --check exited 0"
  else
    rc=$?
    _fail "render-templates.sh --check exited $rc (see /tmp/render-check.log)"
    [[ "$TESTS_VERBOSE" == "1" ]] && cat /tmp/render-check.log
  fi
  end_scenario
}

test_render_preserves_nginx_vars() {
  begin_scenario "render_preserves_nginx_vars"
  local snippet="$REPO_SRC/stacks/authelia/snippets/authelia-authrequest.conf.template"
  local out
  out="$(mktemp)"
  if [[ ! -f "$snippet" ]]; then
    _fail "auth snippet template missing: $snippet"
    end_scenario
    return
  fi
  # shellcheck disable=SC2016
  DOMAIN="$DOMAIN" envsubst '${DOMAIN}' < "$snippet" > "$out"

  # Wave 1B QC constraint: $scheme must remain literal in rendered output.
  if grep -q '\$scheme' "$out"; then
    _pass 'rendered snippet preserves $scheme literal'
  else
    _fail 'rendered snippet lost $scheme literal'
  fi
  if grep -q '\$upstream_http_remote_user' "$out"; then
    _pass 'rendered snippet preserves $upstream_http_remote_user literal'
  else
    _fail 'rendered snippet lost $upstream_http_remote_user literal'
  fi
  # And DOMAIN should be substituted.
  if grep -q "$DOMAIN" "$out"; then
    _pass "DOMAIN ($DOMAIN) substituted into snippet"
  else
    _fail "DOMAIN not substituted into snippet"
  fi
  rm -f "$out"
  end_scenario
}

test_phase2_skip_gw0_default() {
  begin_scenario "phase2_skip_gw0_default"
  # We need a phase1 to have run so phase2's preflight (env file, sentinel)
  # passes. Reuse the happy-path setup if it ran.
  if [[ ! -f "$PHASE1_DONE" || ! -f "$ENV_FILE" ]]; then
    reset_state
    if ! run_phase1 "phase2_skip_gw0_default"; then
      _fail "phase1 prerequisite failed"
      end_scenario
      return
    fi
  fi

  # Run install-gw0.sh directly with SKIP_GW0=1 (the explicit opt-out path
  # — the phase2 default is now SKIP_GW0=0 so gw0 ships enabled) and
  # confirm it logs the skip line and exits 0.
  local out
  out="$(SKIP_GW0=1 bash /opt/raph-server-installer/scripts/install-gw0.sh 2>&1)" \
    || { _fail "install-gw0.sh exited non-zero with SKIP_GW0=1"; end_scenario; return; }
  if grep -q "SKIP_GW0=1, skipping" <<<"$out"; then
    _pass "install-gw0.sh logged the SKIP_GW0=1 banner"
  else
    _fail "install-gw0.sh did not log SKIP_GW0 skip banner. Got: $out"
  fi
  end_scenario
}

test_phase2_brings_up_stacks() {
  begin_scenario "phase2_brings_up_stacks"
  # Need phase1 sentinel + env file. Reuse what's there if a previous test
  # already produced it; otherwise rerun.
  if [[ ! -f "$PHASE1_DONE" || ! -f "$ENV_FILE" ]]; then
    reset_state
    if ! run_phase1 "phase2_brings_up_stacks_pre"; then
      _fail "phase1 prerequisite failed"
      end_scenario
      return
    fi
  fi

  # Wipe phase2 sentinel + the compose log so we observe a fresh run.
  rm -f "$PHASE2_DONE"
  : > "$COMPOSE_LOG"

  if ! run_phase2 "phase2_brings_up_stacks"; then
    _fail "bootstrap-phase2.sh exited non-zero"
    end_scenario
    return
  fi
  _pass "bootstrap-phase2.sh exited 0"

  assert_file_exists "$PHASE2_DONE" "phase2 sentinel created"

  # Each stack should have a `compose up` invocation logged.
  for stack in ingress authelia cloud console enrol; do
    if grep -qx "$stack" "$COMPOSE_LOG"; then
      _pass "compose up invoked for stack: $stack"
    else
      _fail "compose up NOT invoked for stack: $stack (compose log: $(cat "$COMPOSE_LOG" 2>/dev/null | tr '\n' ' '))"
    fi
  done

  # qedge should NOT have been invoked (default SKIP_QEDGE=1).
  if grep -qx "qedge" "$COMPOSE_LOG"; then
    _fail "qedge stack was started despite default SKIP_QEDGE=1"
  else
    _pass "qedge stack not started (default skip)"
  fi

  # And no real `cryptsetup luksFormat` should have been called.
  assert_stub_not_invoked "cryptsetup luksFormat" \
    "no real cryptsetup luksFormat invocation"
  assert_stub_not_invoked "STUB-systemctl-reboot-leak" \
    "no real systemctl reboot leak"

  end_scenario
}

# ---- wizard walkthrough (Wave 4B) --------------------------------------

# _wizard_setup_phases — ensure phase1 + phase2 have run for the current
# scenario name. Mirrors the on-demand prereq pattern used by
# test_phase2_brings_up_stacks; factored here because the wizard scenario
# needs both sentinels and a fully-rendered /opt/stacks tree.
_wizard_setup_phases() {
  local scenario="$1"
  if [[ ! -f "$PHASE1_DONE" || ! -f "$ENV_FILE" ]]; then
    reset_state
    if ! run_phase1 "${scenario}_phase1"; then
      _fail "phase1 prerequisite failed"
      return 1
    fi
  fi
  if [[ ! -f "$PHASE2_DONE" ]]; then
    if ! run_phase2 "${scenario}_phase2"; then
      _fail "phase2 prerequisite failed"
      return 1
    fi
  fi
  return 0
}

# _wizard_kill_enrol — best-effort SIGTERM on a child PID file written by
# the scenario; falls back to pkill on the binary path. Idempotent.
_wizard_kill_enrol() {
  local pidfile="$1"
  if [[ -f "$pidfile" ]]; then
    local pid
    pid="$(cat "$pidfile" 2>/dev/null || true)"
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      sleep 1
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$pidfile"
  fi
  pkill -f '/tmp/enrol-wizard' 2>/dev/null || true
}

# test_wizard_walkthrough — exercise setup.go's full HTTP route table by
# running an enrol binary directly inside the harness container and
# curling through every step from /setup → /setup/done. Verifies:
#
#   - GET /setup with ?token= 303s to /setup/<step> AND drops a
#     setup_token cookie (handleSetupRoot's resume logic + setSetupCookie)
#   - GET /setup/domain returns 200 and contains the operator's DOMAIN
#   - POST /setup/domain (cookie + dummy form) 303s to /setup/dns
#   - POST /setup/dns (provider=rfc2136 + dummy creds) 303s to /setup/admin
#   - POST /setup/admin (alice/12-char-password) 303s to /setup/storage
#   - GET /setup/storage returns 200 + a free-space line + size inputs
#   - POST /setup/storage (10 GiB / 50 GiB) 303s to /setup/finalize
#   - GET /setup/finalize returns 200
#   - GET /setup/events emits `event: done` within 30 s (every shell-out
#     in runFinalize either no-ops in TEST_MODE or hits a stub that
#     returns 0 — the assertion is the SSE done event itself)
#   - After finalize, /srv/store/.setup-complete exists and a fresh
#     GET /setup is now gated by setupRouteGate to a 404 wizard surface
test_wizard_walkthrough() {
  begin_scenario "wizard_walkthrough"

  if ! _wizard_setup_phases "wizard_walkthrough"; then
    end_scenario
    return
  fi

  local scenario_dir="$RUN_DIR_BASE/wizard_walkthrough"
  mkdir -p "$scenario_dir"
  local enrol_log="$scenario_dir/enrol.log"
  local enrol_pid="$scenario_dir/enrol.pid"
  local sse_body="$scenario_dir/finalize-sse.log"
  local cookie_jar="$scenario_dir/cookies.txt"
  : > "$enrol_log"
  : > "$sse_body"
  : > "$cookie_jar"

  # Read the setup token bootstrap wrote during phase1.
  if [[ ! -s "$TOKEN_FILE" ]]; then
    _fail "setup token file missing or empty: $TOKEN_FILE"
    end_scenario
    return
  fi
  local token
  token="$(tr -d '\n' < "$TOKEN_FILE")"
  if [[ ${#token} -lt 16 ]]; then
    _fail "setup token implausibly short: ${#token} chars"
    end_scenario
    return
  fi

  # Build enrol from the bind-mounted source. Use a unique output path
  # under /tmp so a flaky build doesn't fight a stale binary on rerun.
  local src_dir="$REPO_SRC/stacks/enrol"
  local bin="/tmp/enrol-wizard"
  rm -f "$bin"
  if ! (cd "$src_dir" && go build -buildvcs=false -o "$bin" .) >>"$enrol_log" 2>&1; then
    _fail "go build of stacks/enrol failed (see $enrol_log)"
    [[ "$TESTS_VERBOSE" == "1" ]] && tail -40 "$enrol_log"
    end_scenario
    return
  fi
  _pass "go build of stacks/enrol succeeded"

  # Pick a high port unlikely to clash. The harness container has full
  # control of its loopback so any port works; 18080 is convention.
  local port="18080"
  local base="http://127.0.0.1:${port}"

  # Pre-create dirs the wizard's finalize step writes into. Without these,
  # finalizeWriteAdmin's MkdirAll fails when we point usersDB into a
  # tempdir tree.
  mkdir -p /tmp/wizard-state /tmp/wizard-authelia
  rm -f /srv/store/.setup-complete   # belt-and-braces: ensure setup mode

  # Spawn enrol. ENROL_DOMAIN is required at startup. The other env vars
  # redirect every output path into a writable tmp tree so the harness
  # doesn't need root + isn't allowed to clobber /etc/authelia (that's
  # a bind-mounted target on the real VPS but a regular dir here).
  SETUP_TOKEN="$token" \
  ENROL_DOMAIN="$DOMAIN" \
  ENROL_LISTEN="127.0.0.1:${port}" \
  ENROL_TEMPLATES="$src_dir/web/templates" \
  ENROL_STATIC="$src_dir/web/static" \
  ENROL_USERS_DB="/tmp/wizard-authelia/users_database.yml" \
  ENROL_SETUP_STATE_DIR="/tmp/wizard-state" \
  ENROL_SETUP_COMPLETE="/srv/store/.setup-complete" \
  ENROL_SETUP_TOKEN_FILE="$TOKEN_FILE" \
  ENROL_STACKS_DIR="/opt/stacks" \
  ENROL_REPO_DIR="/opt/raph-server-installer" \
  ENROL_LAUNCHER_DIR="/tmp/wizard-launcher" \
  ENROL_PEERS_ARCHIVE_DIR="/tmp/wizard-peers-archive" \
  ENROL_AWG_DIR="/tmp/wizard-awg" \
  TEST_MODE=1 \
  TEST_STUB_LOG="$STUB_LOG" \
  TEST_COMPOSE_LOG="$COMPOSE_LOG" \
  "$bin" >>"$enrol_log" 2>&1 &
  echo $! > "$enrol_pid"

  # Trap so kill happens even if a later assertion bails via `return`.
  # (bash function returns don't unwind ERR traps cleanly; the explicit
  # _wizard_kill_enrol at the end is the actual cleanup.)
  # Wait for the listener to come up.
  local up=0
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl -fsS -o /dev/null --max-time 1 "$base/healthz" 2>/dev/null; then
      up=1
      break
    fi
    sleep 0.5
  done
  if [[ "$up" != "1" ]]; then
    _fail "enrol did not bind $base/healthz within 7.5s"
    [[ "$TESTS_VERBOSE" == "1" ]] && tail -40 "$enrol_log"
    _wizard_kill_enrol "$enrol_pid"
    end_scenario
    return
  fi
  _pass "enrol listening on $base/healthz"

  # ----- a. GET /setup?token=... → 303 + Set-Cookie -------------------
  assert_http_303_to "$base/setup?token=$token" "/setup/" \
    "GET /setup with token 303s to /setup/" -c "$cookie_jar"
  assert_http_cookie_set "$base/setup?token=$token" "setup_token" \
    "GET /setup with token sets setup_token cookie" -c "$cookie_jar"

  # ----- b. GET /setup/domain (cookie) → 200 + body contains DOMAIN ---
  local domain_body="$scenario_dir/domain.html"
  curl -sS -b "$cookie_jar" --max-time 5 -o "$domain_body" -w '%{http_code}' \
    "$base/setup/domain" > "$scenario_dir/domain.code" 2>/dev/null || true
  if [[ "$(cat "$scenario_dir/domain.code")" == "200" ]]; then
    _pass "GET /setup/domain returned 200"
  else
    _fail "GET /setup/domain returned $(cat "$scenario_dir/domain.code")"
  fi
  if grep -qF "$DOMAIN" "$domain_body"; then
    _pass "/setup/domain body contains '$DOMAIN'"
  else
    _fail "/setup/domain body does not contain '$DOMAIN'"
  fi

  # ----- c. POST /setup/domain → 303 to /setup/dns --------------------
  assert_http_303_to "$base/setup/domain" "/setup/dns" \
    "POST /setup/domain 303s to /setup/dns" \
    -b "$cookie_jar" -X POST -d "confirmed=yes"

  # ----- d. POST /setup/dns (rfc2136) → 303 to /setup/admin -----------
  # rfc2136 has the simplest field shape that's not the multi-line google
  # provider. All five fields (server/port/name/secret/algorithm) must be
  # non-empty or handleSetupDNS re-renders with an error.
  assert_http_303_to "$base/setup/dns" "/setup/admin" \
    "POST /setup/dns (rfc2136) 303s to /setup/admin" \
    -b "$cookie_jar" -X POST \
    -d "provider=rfc2136" \
    -d "dns_rfc2136_server=ns1.${DOMAIN}" \
    -d "dns_rfc2136_port=53" \
    -d "dns_rfc2136_name=tsig-key" \
    -d "dns_rfc2136_secret=dGVzdC10c2lnLXNlY3JldA==" \
    -d "dns_rfc2136_algorithm=HMAC-SHA512"

  # ----- e. POST /setup/admin (alice/12-char-password) → /setup/storage
  assert_http_303_to "$base/setup/admin" "/setup/storage" \
    "POST /setup/admin (alice) 303s to /setup/storage" \
    -b "$cookie_jar" -X POST \
    -d "name=alice" \
    -d "displayname=Alice" \
    -d "email=$ADMIN_EMAIL" \
    -d "password=alice-pw-1234" \
    -d "password_confirm=alice-pw-1234"

  # ----- e2. GET /setup/storage → 200 + size inputs --------------------
  local storage_body="$scenario_dir/storage.html"
  curl -sS -b "$cookie_jar" --max-time 5 -o "$storage_body" -w '%{http_code}' \
    "$base/setup/storage" > "$scenario_dir/storage.code" 2>/dev/null || true
  if [[ "$(cat "$scenario_dir/storage.code")" == "200" ]]; then
    _pass "GET /setup/storage returned 200"
  else
    _fail "GET /setup/storage returned $(cat "$scenario_dir/storage.code")"
  fi
  if grep -q 'name="personal_size_gib"' "$storage_body" && \
     grep -q 'name="shared_size_gib"' "$storage_body"; then
    _pass "/setup/storage body contains personal+shared size inputs"
  else
    _fail "/setup/storage body missing personal+shared size inputs"
  fi

  # ----- e3. POST /setup/storage (10 / 50 GiB) → /setup/finalize -------
  assert_http_303_to "$base/setup/storage" "/setup/finalize" \
    "POST /setup/storage (10/50 GiB) 303s to /setup/finalize" \
    -b "$cookie_jar" -X POST \
    -d "personal_size_gib=10" \
    -d "shared_size_gib=50"

  # ----- f. GET /setup/finalize → 200 ----------------------------------
  local final_code
  final_code="$(curl -sS -b "$cookie_jar" --max-time 5 -o "$scenario_dir/finalize.html" \
    -w '%{http_code}' "$base/setup/finalize" 2>/dev/null || echo 000)"
  if [[ "$final_code" == "200" ]]; then
    _pass "GET /setup/finalize returned 200"
  else
    _fail "GET /setup/finalize returned $final_code"
  fi

  # ----- g. GET /setup/events → SSE; expect `event: done` within 30s --
  # The handler runs the finalize pipeline synchronously and emits `event:
  # done` only after sentinel-touch (step 6). Stream into a file so we can
  # poll for the event marker; cap at 30s so a hung step doesn't block
  # the entire harness past its 5-min budget.
  curl -sS -N -b "$cookie_jar" --max-time 35 "$base/setup/events" \
    > "$sse_body" 2>>"$enrol_log" &
  local sse_pid=$!
  assert_sse_event_within "$sse_body" "done" 30 \
    "SSE 'event: done' arrives within 30s of /setup/events"
  # The curl will exit naturally when the handler returns after `done`
  # (the pipeline finished). Reap it explicitly so we don't leave a
  # zombie; the kill is a no-op if it already exited.
  kill "$sse_pid" 2>/dev/null || true
  wait "$sse_pid" 2>/dev/null || true

  # ----- h. After finalize: sentinel exists, /setup is 404 -------------
  assert_file_exists "/srv/store/.setup-complete" \
    "/srv/store/.setup-complete created by finalize"
  # setupRouteGate flips behaviour the moment the sentinel exists.
  # GET /setup with the cookie should now 404 (gate matches /setup
  # exactly + /setup/* — see server.go's setupRouteGate).
  local post_code
  post_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
    -b "$cookie_jar" "$base/setup" 2>/dev/null || echo 000)"
  if [[ "$post_code" == "404" ]]; then
    _pass "GET /setup returns 404 after finalize completes"
  else
    _fail "GET /setup returned $post_code after finalize (wanted 404)"
  fi

  # ----- cleanup --------------------------------------------------------
  _wizard_kill_enrol "$enrol_pid"

  end_scenario
}

# ---- driver -------------------------------------------------------------

run_all() {
  test_phase1_happy_path
  test_phase1_missing_domain
  test_phase1_idempotent
  test_render_check_passes
  test_render_preserves_nginx_vars
  test_phase2_skip_gw0_default
  test_phase2_brings_up_stacks
  test_wizard_walkthrough
}
