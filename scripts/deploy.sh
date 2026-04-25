#!/usr/bin/env bash
# deploy.sh — laptop -> VPS sync for rarcus-server.
#
# Edit on the laptop, commit, run this. Never edit files on the server.
# Source of truth lives at <repo>/ and is rsync'd to the VPS over SSH.
# No Ansible, no agents, no secrets in the script.
#
# Usage: ./scripts/deploy.sh [<vps-host>]
#        DEPLOY_HOST=foo.example   ./scripts/deploy.sh
#        DEPLOY_USER=marcus        ./scripts/deploy.sh   # login as marcus
#        DEPLOY_KEY=~/.ssh/id_foo  ./scripts/deploy.sh   # use a specific key
#        DEPLOY_DIRTY=1            ./scripts/deploy.sh   # skip dirty-tree check
#
# Idempotent: rsync only ships diffs; host-side configs are copied to
# their /etc destinations only when they actually differ, so reloads
# fire only on real change.
#
# Auth model: root SSH is disabled on the VPS. We log in as a non-root
# admin (sagan or marcus, both NOPASSWD-sudo) and use sudo on the remote
# end so rsync and the apply script can write under /root and /opt.

set -euo pipefail

# ---------------------------------------------------------------------------
# 0. argv / env / --help
# ---------------------------------------------------------------------------

print_help() {
    cat <<'EOF'
deploy.sh — sync stacks/, host/, scripts/ from this laptop repo to the VPS.

USAGE
    ./scripts/deploy.sh [<vps-host>]

ARGUMENTS
    <vps-host>      SSH host (<user>@<vps-host>). Defaults to:
                      $DEPLOY_HOST  if set
                      antarctica-engineering.com  otherwise

ENVIRONMENT
    DEPLOY_HOST     Default VPS host if no argv given.
    DEPLOY_USER     SSH login user. Default: sagan.
                    Accepts: sagan | marcus | root.
                    (root only useful if hardening is rolled back —
                    PermitRootLogin no is currently in force.)
    DEPLOY_KEY      Identity file path passed to ssh/rsync as -i.
                    If unset and DEPLOY_USER=marcus, defaults to
                    ~/.ssh/id_marcus_server. Otherwise SSH picks the
                    default key (typically ~/.ssh/id_ed25519).
    DEPLOY_DIRTY=1  Skip the "uncommitted changes" check. Use only when
                    you really mean it (e.g. mid-bisection).

WHAT IT DOES
    1. Verifies the working tree is clean (git status --porcelain).
    2. Refuses to run on the VPS itself.
    3. Probes SSH reachability with BatchMode (no password prompts).
    4. rsyncs three trees:
         host/    -> /root/host/        (--delete, .env-excluded)
         scripts/ -> /root/scripts/     (no --delete, +x)
         stacks/  -> /opt/stacks/       (no --delete, generated state preserved)
    5. Copies host-side configs into /etc only on diff, and reloads
       only the systemd units whose configs actually changed:
         sysctl/*.conf            -> /etc/sysctl.d/        (sysctl --system)
         ssh/sshd_config.d/*.conf -> /etc/ssh/sshd_config.d (sshd -t; reload ssh)
         fail2ban/jail.local      -> /etc/fail2ban/         (reload fail2ban)
         systemd/*.{service,timer,target,path}
                                  -> /etc/systemd/system/   (daemon-reload)
    6. Prints a per-step summary of what changed.

WHAT IT DOES NOT DO
    - Push to git (you commit; this just deploys).
    - Restart user-facing Docker stacks (do that via console / Portainer).
    - Touch /srv/store, /etc/wireguard, peers, or any generated state.

REMOTE PRIVILEGE
    Login user is unprivileged but has NOPASSWD sudo. rsync runs on the
    remote end via --rsync-path='sudo rsync' so it can write into /root
    and /opt/stacks. The host-config apply script is piped through
    'sudo bash -s'.
EOF
}

case "${1:-}" in
    -h|--help|help) print_help; exit 0 ;;
esac

HOST="${1:-${DEPLOY_HOST:-antarctica-engineering.com}}"
DEPLOY_USER="${DEPLOY_USER:-sagan}"

# Default key for marcus iff none was explicitly provided.
if [[ -z "${DEPLOY_KEY:-}" && "${DEPLOY_USER}" == "marcus" ]]; then
    DEPLOY_KEY="${HOME}/.ssh/id_marcus_server"
fi

SSH_TARGET="${DEPLOY_USER}@${HOST}"

# Build SSH/rsync option arrays. Add -i only when DEPLOY_KEY is set so we
# don't override ssh-config / agent defaults for the unspecified case.
SSH_OPTS=()
RSYNC_SSH="ssh"
if [[ -n "${DEPLOY_KEY:-}" ]]; then
    if [[ ! -r "${DEPLOY_KEY}" ]]; then
        echo "deploy.sh: DEPLOY_KEY=${DEPLOY_KEY} not readable." >&2
        exit 1
    fi
    SSH_OPTS+=(-i "${DEPLOY_KEY}")
    RSYNC_SSH="ssh -i ${DEPLOY_KEY}"
fi

# When the remote login user is not root, we need sudo on the far side
# for rsync (writing under /root, /opt/stacks) and for the apply script.
if [[ "${DEPLOY_USER}" == "root" ]]; then
    REMOTE_RSYNC_PATH="rsync"
    REMOTE_BASH=(bash -s)
else
    REMOTE_RSYNC_PATH="sudo rsync"
    REMOTE_BASH=(sudo bash -s)
fi

# ---------------------------------------------------------------------------
# 1. Locate repo root: walk up from script dir until we find docs/plan.md
# ---------------------------------------------------------------------------

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
REPO_ROOT="${SCRIPT_DIR}"
while [[ "${REPO_ROOT}" != "/" && ! -f "${REPO_ROOT}/docs/plan.md" ]]; do
    REPO_ROOT="$( dirname "${REPO_ROOT}" )"
done
if [[ ! -f "${REPO_ROOT}/docs/plan.md" ]]; then
    echo "deploy.sh: could not locate repo root (no docs/plan.md found)" >&2
    exit 1
fi
cd "${REPO_ROOT}"

# Sanity: the three trees we deploy must exist.
for d in host scripts stacks; do
    if [[ ! -d "${REPO_ROOT}/${d}" ]]; then
        echo "deploy.sh: missing ${REPO_ROOT}/${d}/ — refusing to deploy" >&2
        exit 1
    fi
done

# ---------------------------------------------------------------------------
# 2. Defensive guard: don't run on the VPS itself.
#    /srv/store is server-only; same for matching hostname.
# ---------------------------------------------------------------------------

if [[ -d /srv/store ]]; then
    echo "deploy.sh: /srv/store exists — looks like you're on the VPS." >&2
    echo "           Refusing to deploy onto itself." >&2
    exit 1
fi
LOCAL_HOSTNAME="$(hostname -f 2>/dev/null || hostname)"
if [[ "${LOCAL_HOSTNAME}" == "${HOST}" ]]; then
    echo "deploy.sh: local hostname matches VPS host (${HOST}). Refusing." >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# 3. Working-tree check (git): bail if dirty unless DEPLOY_DIRTY=1.
# ---------------------------------------------------------------------------

if [[ "${DEPLOY_DIRTY:-0}" != "1" ]]; then
    if ! git -C "${REPO_ROOT}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        echo "deploy.sh: ${REPO_ROOT} is not a git working tree." >&2
        echo "           Set DEPLOY_DIRTY=1 to bypass this check." >&2
        exit 1
    fi
    if [[ -n "$(git -C "${REPO_ROOT}" status --porcelain)" ]]; then
        echo "deploy.sh: you forgot to commit." >&2
        echo "           Working tree has uncommitted changes:" >&2
        git -C "${REPO_ROOT}" status --short >&2
        echo >&2
        echo "           Override with DEPLOY_DIRTY=1 if you really mean it." >&2
        exit 1
    fi
fi

# ---------------------------------------------------------------------------
# 4. SSH reachability probe (BatchMode -> no password prompt, fast fail).
# ---------------------------------------------------------------------------

echo ":: deploy target: ${SSH_TARGET}"
echo ":: probing SSH ..."
if ! ssh "${SSH_OPTS[@]}" \
        -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new \
        "${SSH_TARGET}" true; then
    echo "deploy.sh: SSH to ${SSH_TARGET} failed (BatchMode)." >&2
    echo "           Check key auth, host key, network reachability." >&2
    exit 1
fi

# Quick sanity: if we're not root, confirm NOPASSWD sudo works. Without
# this the rsync --rsync-path='sudo rsync' calls below fail mysteriously.
if [[ "${DEPLOY_USER}" != "root" ]]; then
    if ! ssh "${SSH_OPTS[@]}" -o BatchMode=yes -o ConnectTimeout=5 \
            "${SSH_TARGET}" "sudo -n true" 2>/dev/null; then
        echo "deploy.sh: ${DEPLOY_USER}@${HOST} cannot run passwordless sudo." >&2
        echo "           Expected /etc/sudoers.d/90-admins-nopasswd to grant it." >&2
        exit 1
    fi
fi

# ---------------------------------------------------------------------------
# 5. rsync the three trees.
#    Common excludes: .env (secrets), *.bak.* (sed-style backups).
# ---------------------------------------------------------------------------

RSYNC_COMMON=(
    -az
    --human-readable
    --itemize-changes
    -e "${RSYNC_SSH}"
    --rsync-path="${REMOTE_RSYNC_PATH}"
    --exclude='.env'
    --exclude='.env.*'
    --exclude='*.bak.*'
    --exclude='.git/'
    --exclude='.gitignore'
)

# Make sure the three destination roots exist before rsync. On a fresh
# box /opt/stacks may not exist yet; rsync would create the leaf but
# `install -d` is cheaper than relying on rsync's parent-creation.
echo ":: ensuring remote dirs exist ..."
ssh "${SSH_OPTS[@]}" "${SSH_TARGET}" \
    "${REMOTE_BASH[@]}" <<'EOF'
set -euo pipefail
install -d -m 0755 /root/host /root/scripts /opt/stacks
EOF

echo ":: rsync host/  -> ${SSH_TARGET}:/root/host/"
HOST_LOG="$(rsync "${RSYNC_COMMON[@]}" --delete \
    "${REPO_ROOT}/host/" "${SSH_TARGET}:/root/host/")"
echo "${HOST_LOG}"

echo ":: rsync scripts/  -> ${SSH_TARGET}:/root/scripts/"
SCRIPTS_LOG="$(rsync "${RSYNC_COMMON[@]}" \
    "${REPO_ROOT}/scripts/" "${SSH_TARGET}:/root/scripts/")"
echo "${SCRIPTS_LOG}"
ssh "${SSH_OPTS[@]}" "${SSH_TARGET}" \
    "${REMOTE_BASH[@]}" <<'EOF'
set -euo pipefail
chmod -R u+rwX,go+rX /root/scripts
chmod +x /root/scripts/*.sh
EOF

echo ":: rsync stacks/  -> ${SSH_TARGET}:/opt/stacks/"
# No --delete: stacks have generated state on the VPS (data/, letsencrypt/, tls/).
# Excludes guard against accidentally overwriting that state if the laptop
# ever ends up with same-named dirs. NOTE: do NOT exclude conf/ — stacks/cloud/conf/
# is checked-in source-of-truth (copyparty.conf), and qedge's rendered config
# lives at qedge/config/ (different name) so doesn't collide.
STACKS_LOG="$(rsync "${RSYNC_COMMON[@]}" \
    --exclude='data/' \
    --exclude='letsencrypt/' \
    --exclude='tls/' \
    "${REPO_ROOT}/stacks/" "${SSH_TARGET}:/opt/stacks/")"
echo "${STACKS_LOG}"

# ---------------------------------------------------------------------------
# 6. Apply host configs (diff-gated). All work happens on the remote;
#    we ship a single shell snippet so the reloads only fire on real diffs.
#
#    The remote script:
#      - For each src under /root/host/<group>/, compares with /etc/<dest>/
#      - Copies on diff (cmp -s)
#      - Records which groups changed; reloads at the end
#      - sshd: validate with `sshd -t` BEFORE reloading; bail loud if invalid
# ---------------------------------------------------------------------------

echo ":: applying host configs (diff-gated) on ${SSH_TARGET} ..."

REMOTE_APPLY=$(cat <<'REMOTE'
set -euo pipefail

declare -i CHANGES=0
SUMMARY=""
note() { SUMMARY="${SUMMARY}${1}"$'\n'; CHANGES=$((CHANGES+1)); }

# Copy file on diff. Args: <src> <dest>
copy_if_diff() {
    local src="$1" dest="$2"
    if [[ ! -f "$src" ]]; then return 1; fi
    if [[ -f "$dest" ]] && cmp -s "$src" "$dest"; then
        return 1   # unchanged
    fi
    install -m 0644 -D "$src" "$dest"
    return 0       # changed
}

# --- sysctl/*.conf -> /etc/sysctl.d/ ---
SYSCTL_CHANGED=0
if [[ -d /root/host/sysctl ]]; then
    while IFS= read -r -d '' f; do
        bn="$(basename "$f")"
        if copy_if_diff "$f" "/etc/sysctl.d/${bn}"; then
            SYSCTL_CHANGED=1
            note "sysctl: /etc/sysctl.d/${bn} updated"
        fi
    done < <(find /root/host/sysctl -maxdepth 1 -type f -name '*.conf' -print0)
fi
if (( SYSCTL_CHANGED )); then
    if sysctl --system >/dev/null; then
        note "  -> sysctl --system OK"
    else
        echo "deploy.sh(remote): sysctl --system failed" >&2
        exit 10
    fi
fi

# --- ssh/sshd_config.d/*.conf -> /etc/ssh/sshd_config.d/ ---
# Validate with sshd -t BEFORE reload. If validation fails, bail LOUD —
# do not reload sshd with a broken config; we'd lock ourselves out.
SSHD_CHANGED=0
if [[ -d /root/host/ssh/sshd_config.d ]]; then
    while IFS= read -r -d '' f; do
        bn="$(basename "$f")"
        if copy_if_diff "$f" "/etc/ssh/sshd_config.d/${bn}"; then
            SSHD_CHANGED=1
            note "ssh: /etc/ssh/sshd_config.d/${bn} updated"
        fi
    done < <(find /root/host/ssh/sshd_config.d -maxdepth 1 -type f -name '*.conf' -print0)
fi
if (( SSHD_CHANGED )); then
    if ! sshd -t; then
        echo "" >&2
        echo "============================================================" >&2
        echo "deploy.sh(remote): sshd -t REJECTED the new config." >&2
        echo "                   NOT reloading sshd. Existing sshd untouched." >&2
        echo "                   Investigate /etc/ssh/sshd_config.d/ on VPS." >&2
        echo "============================================================" >&2
        exit 11
    fi
    if systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null; then
        note "  -> sshd validated and reloaded"
    else
        echo "deploy.sh(remote): sshd reload failed" >&2
        exit 12
    fi
fi

# --- fail2ban/jail.local -> /etc/fail2ban/jail.local ---
F2B_CHANGED=0
if [[ -f /root/host/fail2ban/jail.local ]]; then
    if copy_if_diff /root/host/fail2ban/jail.local /etc/fail2ban/jail.local; then
        F2B_CHANGED=1
        note "fail2ban: /etc/fail2ban/jail.local updated"
    fi
fi
if (( F2B_CHANGED )); then
    if systemctl reload fail2ban >/dev/null 2>&1 \
        || systemctl restart fail2ban >/dev/null 2>&1; then
        note "  -> fail2ban reloaded"
    else
        echo "deploy.sh(remote): fail2ban reload failed" >&2
        exit 13
    fi
fi

# --- systemd/*.{service,timer,target,path} -> /etc/systemd/system/ ---
# .path units are inotify watchers (see qedge-cert-watcher.path); they
# get the same diff-gated copy treatment as services. The narrower glob
# below intentionally excludes .socket / .mount / .swap — none of our
# stacks ship those, and broadening blindly risks pulling in upstream
# vendor units if someone drops one into host/systemd/ by accident.
UNITS_CHANGED=0
if [[ -d /root/host/systemd ]]; then
    while IFS= read -r -d '' f; do
        bn="$(basename "$f")"
        if copy_if_diff "$f" "/etc/systemd/system/${bn}"; then
            UNITS_CHANGED=1
            note "systemd: /etc/systemd/system/${bn} updated"
        fi
    done < <(find /root/host/systemd -maxdepth 1 -type f \
                \( -name '*.service' -o -name '*.timer' -o -name '*.target' \
                   -o -name '*.path' \) -print0)
fi
if (( UNITS_CHANGED )); then
    if systemctl daemon-reload; then
        note "  -> systemctl daemon-reload OK (units NOT auto-restarted)"
    else
        echo "deploy.sh(remote): systemctl daemon-reload failed" >&2
        exit 14
    fi
fi

# --- enable + start path units (idempotent) ---
# .path units are inert until enabled. We enable --now any path unit we
# shipped, every deploy, regardless of whether it changed this run. This
# is idempotent (systemd is a no-op on already-enabled-and-active units)
# and recovers cleanly from a manual `systemctl disable` between deploys.
# Note: .service files referenced by these .path units are NOT enabled —
# they get triggered by the path unit, not by boot.
if [[ -d /root/host/systemd ]]; then
    while IFS= read -r -d '' f; do
        bn="$(basename "$f")"
        if systemctl enable --now "${bn}" >/dev/null 2>&1; then
            note "systemd: ${bn} enable --now OK"
        else
            echo "deploy.sh(remote): systemctl enable --now ${bn} failed" >&2
            exit 15
        fi
    done < <(find /root/host/systemd -maxdepth 1 -type f -name '*.path' -print0)
fi

if (( CHANGES == 0 )); then
    echo "host-config-summary: no host-side configs changed."
else
    echo "host-config-summary:"
    printf '%s' "$SUMMARY"
fi
REMOTE
)

set +e
REMOTE_OUT="$(ssh "${SSH_OPTS[@]}" "${SSH_TARGET}" \
    "${REMOTE_BASH[@]}" <<<"${REMOTE_APPLY}" 2>&1)"
REMOTE_RC=$?
set -e

echo "${REMOTE_OUT}"

# ---------------------------------------------------------------------------
# 7. Summary
# ---------------------------------------------------------------------------

echo
echo "============================================================"
echo "deploy summary"
echo "  target:        ${SSH_TARGET}"
echo "  repo root:     ${REPO_ROOT}"
echo "  rsync host/:   $(echo "${HOST_LOG}"    | grep -c '^[<>ch.]')"
echo "  rsync scripts: $(echo "${SCRIPTS_LOG}" | grep -c '^[<>ch.]')"
echo "  rsync stacks:  $(echo "${STACKS_LOG}"  | grep -c '^[<>ch.]')"
echo "  remote-apply rc: ${REMOTE_RC}"
echo "============================================================"

if (( REMOTE_RC != 0 )); then
    echo "deploy.sh: remote apply step failed (rc=${REMOTE_RC}). See output above." >&2
    exit "${REMOTE_RC}"
fi

echo "deploy.sh: done."
