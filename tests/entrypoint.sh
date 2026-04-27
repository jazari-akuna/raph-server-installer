#!/usr/bin/env bash
# entrypoint.sh — run inside the test container. Sources assertions +
# scenarios, then dispatches based on the first arg.
#
#   entrypoint.sh run            — run all scenarios, exit FAIL count.
#   entrypoint.sh shell          — drop into an interactive bash.
#   entrypoint.sh run <name>     — run a single scenario by name.

set -uo pipefail

# shellcheck disable=SC1091
. /opt/test/assertions.sh
# shellcheck disable=SC1091
. /opt/test/scenarios.sh

mode="${1:-run}"
case "$mode" in
  shell)
    exec bash --login
    ;;
  run)
    name="${2:-}"
    if [[ -n "$name" ]]; then
      if declare -f "test_$name" >/dev/null; then
        "test_$name"
      else
        echo "no such scenario: $name" >&2
        echo "available: $(declare -F | awk '$3 ~ /^test_/ { sub(/^test_/, "", $3); print $3 }' | tr '\n' ' ')" >&2
        exit 2
      fi
    else
      run_all
    fi
    summarise
    exit $?
    ;;
  *)
    echo "usage: $0 {run [scenario]|shell}" >&2
    exit 2
    ;;
esac
