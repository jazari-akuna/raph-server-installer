#!/usr/bin/env bash
# render-templates.sh — substitute env vars into every .template file.
#
# Wave 2A. Reads /opt/stacks/.env (or --env-file) and renders each template
# in the repo to its sibling output path (basename minus the `.template`
# suffix), via `envsubst` scoped per-file.
#
# Why per-file scope (NOT plain `envsubst < x > y`):
#   `stacks/authelia/configuration.yml.template` and the nginx auth snippet
#   contain literal `$variable` references (YAML anchors, nginx vars like
#   `$scheme`, `$arg_rd`, `$upstream_http_remote_user`) that MUST survive
#   substitution unchanged. Plain envsubst silently blanks every `$IDENT`
#   not in the env, corrupting these files. Scoping (`envsubst '${DOMAIN}'`)
#   limits substitution to the named vars only.
#   Constraint captured in .journals/wave-1b.md "Constraints for Wave 2A".
#
# Modes:
#   render-templates.sh                    Render with current env.
#   render-templates.sh --env-file PATH    Source PATH first, then render.
#   render-templates.sh --check            Render against a baked sample env
#                                          to /tmp; verify no leftover ${VAR}
#                                          tokens. Used by CI.
#   render-templates.sh --dry-run          List what would be rendered.
#
# Idempotent: re-running with the same env produces byte-identical output.

set -euo pipefail

# ---------- preflight ----------------------------------------------------
#
# `envsubst` lives in the gettext-base Debian package. If it's missing
# every per-template substitution below will fail with the same useless
# "envsubst: command not found" line repeated N times. Surface the
# missing-package case once, with a clear remediation pointer covering
# both the host (gettext-base) and the enrol container (apt install
# gettext-base in the Dockerfile runtime stage), instead.
if ! command -v envsubst >/dev/null 2>&1; then
  echo "render-templates: envsubst is not on PATH (gettext-base)" >&2
  echo "remediation:" >&2
  echo "  host  : DEBIAN_FRONTEND=noninteractive apt-get install -y gettext-base" >&2
  echo "  enrol : ensure stacks/enrol/Dockerfile runtime stage installs gettext-base" >&2
  echo "          (rebuild via: cd stacks/enrol && docker compose --env-file /opt/stacks/.env up -d --build)" >&2
  exit 127
fi

# ---------- args ----------------------------------------------------------

ENV_FILE=""
MODE="render"   # render | check | dry-run
while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      ENV_FILE="${2:?--env-file requires a path}"
      shift 2
      ;;
    --check)
      MODE="check"
      shift
      ;;
    --dry-run)
      MODE="dry-run"
      shift
      ;;
    -h|--help)
      sed -n '2,30p' "$0"
      exit 0
      ;;
    *)
      echo "render-templates: unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

# ---------- locate repo root ---------------------------------------------

# Walk up from this script's directory until we find docs/design.md (the
# repo-root sentinel established in Wave 1B). Mirrors deploy.sh's approach.
# The `-P` on cd resolves symlinks so a symlinked /opt/raph-server-installer
# (e.g. the test-harness `TEST_REPO_SRC` symlink) lands on the real path —
# without this, `find $repo_root` silently descends into nothing because
# find treats a symlink-as-target as a single file unless given `-L` or a
# trailing slash. (Discovered by tests/ harness.)
script_dir="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$script_dir"
while [[ "$repo_root" != "/" && ! -f "$repo_root/docs/design.md" ]]; do
  repo_root="$(dirname "$repo_root")"
done
if [[ ! -f "$repo_root/docs/design.md" ]]; then
  echo "render-templates: cannot locate repo root (docs/design.md not found)" >&2
  exit 1
fi

# ---------- env load -----------------------------------------------------

# In --check mode, ignore caller env entirely; render against a known-good
# sample so CI is deterministic.
if [[ "$MODE" == "check" ]]; then
  CHECK_ROOT="$(mktemp -d)"
  trap 'rm -rf "$CHECK_ROOT"' EXIT
  export DOMAIN="example.com"
  export ADMIN_USERS="alice"
  export ADMIN_USERS_SSH="alice"
  export QEDGE_PASSWORD="check-mode-placeholder"
  # shellcheck disable=SC2016  # literal $pbkdf2 prefix — pbkdf2 hashes
  # always start with this token; the wizard supplies the real hash later.
  export AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH='$pbkdf2-sha512$check-mode-placeholder'
  # shellcheck disable=SC2016
  export AUTHELIA_OIDC_CLOUD_CLIENT_SECRET_HASH='$pbkdf2-sha512$check-mode-placeholder'
  # shellcheck disable=SC2016
  export AUTHELIA_OIDC_TASK_CLIENT_SECRET_HASH='$pbkdf2-sha512$check-mode-placeholder'
  echo "[render-templates] --check: rendering with sample env to $CHECK_ROOT"
else
  # Default: source --env-file if given, else fall back to /opt/stacks/.env
  # if it exists. Caller-supplied env always wins (export before invoking).
  candidate=""
  if [[ -n "$ENV_FILE" ]]; then
    candidate="$ENV_FILE"
  elif [[ -f /opt/stacks/.env ]]; then
    candidate="/opt/stacks/.env"
  fi
  if [[ -n "$candidate" ]]; then
    if [[ ! -r "$candidate" ]]; then
      echo "render-templates: env file not readable: $candidate" >&2
      exit 1
    fi
    echo "[render-templates] sourcing env from $candidate"
    # shellcheck disable=SC1090
    set -a; . "$candidate"; set +a
  fi
fi

# Auto-derive ADMIN_USERS_SSH from ADMIN_USERS if unset (Wave 1B QC).
# The wizard never asks for ADMIN_USERS_SSH — operators only set ADMIN_USERS.
if [[ -z "${ADMIN_USERS_SSH:-}" && -n "${ADMIN_USERS:-}" ]]; then
  export ADMIN_USERS_SSH="$ADMIN_USERS"
fi

# ---------- per-template scope map ---------------------------------------

# Glob-discover *.template files, but maintain an explicit scope map keyed on
# basename. Adding a new template later requires adding an entry here OR
# accepting the default scope (which is conservatively '${DOMAIN}').
#
# Returns the envsubst scope string (a quoted argument like '${A} ${B}').
# shellcheck disable=SC2016  # the printf args are envsubst scope literals
# that MUST stay single-quoted — they tell envsubst which ${VAR} tokens
# are in-scope. Expanding them here would defeat the entire point.
template_scope() {
  local base="$1"
  case "$base" in
    configuration.yml.template)
      # Authelia config: DOMAIN + per-OIDC-client secret hashes. The
      # nginx-style YAML strings (none here, but the file uses `$pbkdf2`
      # in comment text) survive because $pbkdf2 isn't in our scope.
      # When adding a new OIDC client, add its hash env var here too.
      printf '%s' '${DOMAIN} ${AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH} ${AUTHELIA_OIDC_CLOUD_CLIENT_SECRET_HASH} ${AUTHELIA_OIDC_TASK_CLIENT_SECRET_HASH}'
      ;;
    authelia-authrequest.conf.template)
      # nginx snippet — protect $scheme, $upstream_http_remote_user etc.
      printf '%s' '${DOMAIN}'
      ;;
    99-hardening.conf.template)
      printf '%s' '${ADMIN_USERS_SSH}'
      ;;
    config.yaml.template)
      # Hysteria 2 config (qedge) — DOMAIN in comments, QEDGE_PASSWORD in
      # the auth block. Preserve any other $ tokens in the template.
      printf '%s' '${DOMAIN} ${QEDGE_PASSWORD}'
      ;;
    gw0.conf.template)
      # gw0 conf is rendered by install-gw0.sh with sed (it needs values
      # generated at install time: server private key, magic-header H1..H4).
      # render-templates.sh deliberately skips this one — return empty
      # scope as a sentinel.
      printf '%s' ''
      ;;
    *)
      # Conservative default: only DOMAIN. New templates that need more
      # variables MUST add an explicit entry above.
      printf '%s' '${DOMAIN}'
      ;;
  esac
}

# Returns the absolute output path for a template, or the empty string if
# the template should be skipped (e.g. handled by another script).
template_output() {
  local in="$1"
  local base
  base="$(basename "$in")"
  if [[ "$base" == "gw0.conf.template" ]]; then
    # install-gw0.sh handles this with sed + per-install-time secrets.
    printf '%s' ''
    return 0
  fi
  # Default: strip .template, render to sibling.
  printf '%s' "${in%.template}"
}

# ---------- main loop ----------------------------------------------------

# Collect templates with find (not a globstar), portable and stable order.
mapfile -d '' TEMPLATES < <(find "$repo_root" \
  -path '*/.git' -prune -o \
  -path '*/.journals' -prune -o \
  -path '*/node_modules' -prune -o \
  -type f -name '*.template' -print0 \
  | sort -z)

if [[ ${#TEMPLATES[@]} -eq 0 ]]; then
  echo "[render-templates] no .template files found under $repo_root" >&2
  exit 1
fi

require_var() {
  local v="$1"
  if [[ -z "${!v:-}" ]]; then
    echo "render-templates: required env var '$v' is unset (or empty)" >&2
    exit 1
  fi
}

rendered_count=0
skipped_count=0
failed_count=0

for tpl in "${TEMPLATES[@]}"; do
  base="$(basename "$tpl")"
  scope="$(template_scope "$base")"
  out="$(template_output "$tpl")"

  if [[ -z "$out" ]]; then
    echo "[render-templates] skip: $tpl (handled out-of-band)"
    skipped_count=$((skipped_count + 1))
    continue
  fi

  # Validate every var named in the scope is set, else envsubst will blank
  # it silently and the rendered file will be subtly wrong.
  for v in $(echo "$scope" | grep -oE '[A-Z_][A-Z0-9_]*' || true); do
    require_var "$v"
  done

  # In --check mode, render to a parallel tree under CHECK_ROOT so we don't
  # pollute the working copy. In normal mode, write next to the template.
  if [[ "$MODE" == "check" ]]; then
    rel="${out#"$repo_root"/}"
    dest="$CHECK_ROOT/$rel"
    install -d "$(dirname "$dest")"
  else
    dest="$out"
  fi

  if [[ "$MODE" == "dry-run" ]]; then
    echo "[render-templates] would render: $tpl -> $dest (scope: $scope)"
    rendered_count=$((rendered_count + 1))
    continue
  fi

  # Render via a temp file + atomic mv so a render failure mid-stream
  # never leaves a half-written output file.
  tmp="$(mktemp)"
  if envsubst "$scope" < "$tpl" > "$tmp"; then
    # In check mode, also assert no ${VAR} tokens remain in the rendered
    # output. We deliberately allow $bare_words (nginx vars like $scheme)
    # because per-file scoping leaves them literal by design.
    if [[ "$MODE" == "check" ]]; then
      if grep -nE '\$\{[A-Z_][A-Z0-9_]*\}' "$tmp" >/dev/null; then
        echo "render-templates: leftover \${VAR} tokens in $tpl rendering:" >&2
        grep -nE '\$\{[A-Z_][A-Z0-9_]*\}' "$tmp" | head -5 >&2
        rm -f "$tmp"
        failed_count=$((failed_count + 1))
        continue
      fi
    fi
    mv -f "$tmp" "$dest"
    chmod --reference="$tpl" "$dest" 2>/dev/null || chmod 0644 "$dest"
    echo "[render-templates] rendered: $(realpath --relative-to="$repo_root" "$tpl") -> $(realpath --relative-to="$repo_root" "$dest")"
    rendered_count=$((rendered_count + 1))
  else
    rm -f "$tmp"
    echo "render-templates: envsubst failed on $tpl" >&2
    failed_count=$((failed_count + 1))
  fi
done

echo "[render-templates] summary: rendered=$rendered_count skipped=$skipped_count failed=$failed_count"

if [[ $failed_count -gt 0 ]]; then
  exit 1
fi
exit 0
