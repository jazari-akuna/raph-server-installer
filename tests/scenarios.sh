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

  # Run install-gw0.sh directly with SKIP_GW0=1 (the phase2 default) and
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

# ---- driver -------------------------------------------------------------

run_all() {
  test_phase1_happy_path
  test_phase1_missing_domain
  test_phase1_idempotent
  test_render_check_passes
  test_render_preserves_nginx_vars
  test_phase2_skip_gw0_default
  test_phase2_brings_up_stacks
}
