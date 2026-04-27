#!/usr/bin/env bash
# run-tests.sh — host-side driver. Builds the test image, runs the harness
# inside a container, captures logs to tests/.last-run/.
#
# Env switches:
#   TESTS_KEEP=1    Keep the container around after the run (otherwise --rm).
#   TESTS_VERBOSE=1 Stream the container's stdout to the host as it runs.
#
# Exits 0 if all assertions pass, non-zero otherwise.

set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tests_dir="$repo_root/tests"
last_run="$tests_dir/.last-run"
image_tag="raph-server-installer-tests:dev"
container_name="raph-server-installer-tests-$(date +%s)-$$"

verbose="${TESTS_VERBOSE:-0}"
keep="${TESTS_KEEP:-0}"

mkdir -p "$last_run"
rm -f "$last_run/build.log" "$last_run/container.log" \
      "$last_run/assertions.log" "$last_run/exit.code"

echo "[run-tests] image:     $image_tag"
echo "[run-tests] container: $container_name"
echo "[run-tests] last-run:  $last_run"
echo "[run-tests] repo:      $repo_root"

# ---- 1. build image -----------------------------------------------------

echo "[run-tests] === building image ==="
build_args=()
if [[ "$verbose" == "1" ]]; then
  docker build --progress=plain -t "$image_tag" "$tests_dir" 2>&1 | tee "$last_run/build.log"
else
  docker build -t "$image_tag" "$tests_dir" >"$last_run/build.log" 2>&1 \
    || { echo "[run-tests] build failed; tail of build log:"; tail -40 "$last_run/build.log"; exit 1; }
fi

# ---- 2. run container ---------------------------------------------------

echo "[run-tests] === running scenarios ==="
docker_args=(
  --name "$container_name"
  -e "TESTS_VERBOSE=$verbose"
  # NOTE: bind READ-WRITE. The harness causes render-templates.sh to write
  # rendered .conf/.yml/.yaml siblings next to the .template files inside
  # stacks/. Those rendered files are gitignored, so this is safe — but
  # it means a failing test can leave stale rendered artifacts on the host.
  # `make test` is expected to run from a clean checkout; CI uses ephemeral
  # workspaces. If you're poking locally and want to be sure, run
  # `git clean -fdX stacks/` after.
  -v "$repo_root:/opt/raph-server-installer-src:rw"
)
# Note: we don't pass --rm even when not keeping; we copy logs out manually
# (below) and then `docker rm` at the end. This is the only way to retrieve
# /tmp/test-runs/<scenario>/phase{1,2}.log (which lives inside the container)
# regardless of pass/fail.

# We don't need --privileged: TEST_MODE short-circuits the cryptsetup/docker
# paths that would otherwise need it. If we ever do need to run under
# --privileged, gate it behind TESTS_PRIVILEGED=1 and document why.
rc=0
if [[ "$verbose" == "1" ]]; then
  docker run "${docker_args[@]}" "$image_tag" run 2>&1 | tee "$last_run/container.log"
  rc=${PIPESTATUS[0]}
else
  docker run "${docker_args[@]}" "$image_tag" run >"$last_run/container.log" 2>&1 || rc=$?
fi

# ---- 3. extract assertion log + per-scenario phase logs ----------------

# scenarios.sh prints PASS/FAIL/scenario lines to stdout, which is captured
# in container.log. Distill the assertion lines for an at-a-glance summary.
grep -E '^(PASS|FAIL|--- scenario|--- end|[0-9]+ PASSED)' "$last_run/container.log" \
  > "$last_run/assertions.log" 2>/dev/null || true

# Pull /tmp/test-runs out of the container so failed scenarios leave their
# phase1.log / phase2.log on the host for inspection.
rm -rf "$last_run/test-runs"
docker cp "$container_name":/tmp/test-runs "$last_run/test-runs" 2>/dev/null || true
docker cp "$container_name":/tmp/test-stubs.log "$last_run/stubs.log" 2>/dev/null || true
docker cp "$container_name":/tmp/test-compose.log "$last_run/compose.log" 2>/dev/null || true

echo "$rc" > "$last_run/exit.code"

# ---- 4. report ---------------------------------------------------------

echo
echo "[run-tests] === results ==="
if [[ -s "$last_run/assertions.log" ]]; then
  cat "$last_run/assertions.log"
fi
echo
echo "[run-tests] full container log: $last_run/container.log"
echo "[run-tests] exit code:          $rc"

if [[ "$keep" == "1" ]]; then
  echo
  echo "[run-tests] container kept (TESTS_KEEP=1):"
  echo "    docker rm -f $container_name   # when done"
  echo "    (per-scenario logs already copied to $last_run/test-runs/)"
else
  docker rm -f "$container_name" >/dev/null 2>&1 || true
fi

exit "$rc"
