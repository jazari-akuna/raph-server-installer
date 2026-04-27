#!/usr/bin/env bash
# cert-renewal-hook.sh — keep qedge's TLS symlinks pointed at NPM's
# current wildcard cert directory, even after NPM rotates.
#
# Background
# ----------
# NPM (`ingress`) stores Let's Encrypt material in numbered directories
# under /opt/stacks/ingress/letsencrypt/live/npm-N/. On renewal (~every
# 60 days, ~30 days before expiry) it does NOT overwrite npm-N in place
# — it creates a fresh npm-(N+1)/ and updates its own internal config to
# point at it. qedge (Hysteria 2) reads its cert via two symlinks under
# /opt/stacks/qedge/tls/ that were hand-pinned to a specific npm-N at
# install time (see stacks/qedge/README.md). Without intervention those
# symlinks go stale silently the moment NPM rotates and qedge would
# eventually serve an expired cert (or fail to start) on the next
# switchover.
#
# This script is the host-side fix. Triggered by the matching .path unit
# whenever the live/ directory changes, it:
#   1. Enumerates live/npm-* directories.
#   2. Picks the newest one whose fullchain.pem actually covers
#      *.${DOMAIN} (defensive: NPM may grow other certs later if we add
#      more sites under different roots).
#   3. Re-points qedge's two symlinks at that dir if they don't already.
#   4. If qedge is currently running, lets the change pick up naturally
#      — Hysteria 2 (>= v2.2.3) re-reads local TLS files on every
#      handshake, so swapping symlink targets is sufficient. No signal,
#      no restart needed. We log the running state for visibility.
#   5. If qedge is stopped (the default per stacks/qedge/README.md),
#      just updates the symlinks silently; the next `docker compose up
#      -d` will pick up the current cert.
#
# Idempotent. Safe to invoke on a quiet directory: if the symlinks
# already point at the newest matching dir, the script logs "no change"
# and exits 0.
#
# Logging goes to syslog via `logger -t cert-renewal-hook`; tail with
#   journalctl -t cert-renewal-hook
# or
#   journalctl -u qedge-cert-watcher.service

set -euo pipefail

# --- config ---------------------------------------------------------------
LIVE_DIR="/opt/stacks/ingress/letsencrypt/live"
QEDGE_DIR="/opt/stacks/qedge"
QEDGE_TLS="${QEDGE_DIR}/tls"
# DOMAIN is written by bootstrap to /etc/server-domain. Allow override via env
# (useful for tests). Hardcoding a real domain here would leak it into the
# committed source — instead we fail loudly if neither source is available.
if [[ -z "${DOMAIN:-}" ]]; then
    if [[ -r /etc/server-domain ]]; then
        DOMAIN="$(tr -d '[:space:]' </etc/server-domain)"
    fi
fi
WILDCARD_DOMAIN="*.${DOMAIN:-}"
TAG="cert-renewal-hook"

[[ -n "${DOMAIN:-}" ]] || { echo "${TAG}: DOMAIN unset and /etc/server-domain unreadable" >&2; exit 1; }

# --- logging --------------------------------------------------------------
log() { logger -t "${TAG}" -- "$*"; printf '%s: %s\n' "${TAG}" "$*"; }
die() { logger -t "${TAG}" -p user.err -- "ERROR: $*"; printf '%s: ERROR: %s\n' "${TAG}" "$*" >&2; exit 1; }

# --- preflight ------------------------------------------------------------
[[ $EUID -eq 0 ]] || die "must run as root (need to read NPM's letsencrypt store and write under ${QEDGE_DIR})"
[[ -d "${LIVE_DIR}"  ]] || die "letsencrypt live dir missing: ${LIVE_DIR}"
[[ -d "${QEDGE_DIR}" ]] || die "qedge stack dir missing: ${QEDGE_DIR}"

command -v openssl >/dev/null 2>&1 || die "openssl not found in PATH"
command -v docker  >/dev/null 2>&1 || die "docker not found in PATH"

# --- find the newest npm-N dir whose cert covers the wildcard -------------
# `ls -1 -t` orders by mtime newest first. NPM's renewal creates a new dir
# with a fresh mtime, so newest mtime == newest cert. We still verify the
# SAN to defend against a stranger cert living next to ours (e.g. someone
# adds a non-wildcard site to NPM later).

cert_covers_wildcard() {
    local fc="$1"
    [[ -f "${fc}" ]] || return 1
    # `openssl x509 -ext subjectAltName` prints the SAN extension; grep for
    # the literal wildcard DNS entry. Quote-and-fixed-string match so the
    # asterisk isn't interpreted as a regex meta.
    openssl x509 -in "${fc}" -noout -ext subjectAltName 2>/dev/null \
        | grep -F "DNS:${WILDCARD_DOMAIN}" >/dev/null
}

selected=""
# Iterate npm-* dirs newest-first by mtime. Use find -printf to get a
# stable, parseable mtime+path stream, sort numerically descending.
while IFS= read -r dir; do
    [[ -n "${dir}" ]] || continue
    fc="${dir}/fullchain.pem"
    pk="${dir}/privkey.pem"
    [[ -f "${fc}" && -f "${pk}" ]] || continue
    if cert_covers_wildcard "${fc}"; then
        selected="${dir}"
        break
    fi
done < <(find "${LIVE_DIR}" -mindepth 1 -maxdepth 1 -type d -name 'npm-*' -printf '%T@\t%p\n' \
            | sort -rn | cut -f2-)

[[ -n "${selected}" ]] || die "no live/npm-* dir found whose cert covers ${WILDCARD_DOMAIN}"

log "newest matching cert dir: ${selected}"

# --- ensure qedge/tls/ exists --------------------------------------------
if [[ ! -d "${QEDGE_TLS}" ]]; then
    install -d -m 0750 "${QEDGE_TLS}"
    log "created ${QEDGE_TLS}"
fi

# --- repoint symlinks if they're stale -----------------------------------
desired_fc="${selected}/fullchain.pem"
desired_pk="${selected}/privkey.pem"
link_fc="${QEDGE_TLS}/fullchain.pem"
link_pk="${QEDGE_TLS}/privkey.pem"

current_fc="$(readlink -- "${link_fc}" 2>/dev/null || true)"
current_pk="$(readlink -- "${link_pk}" 2>/dev/null || true)"

changed=0
if [[ "${current_fc}" != "${desired_fc}" ]]; then
    ln -sfn -- "${desired_fc}" "${link_fc}"
    log "repointed fullchain.pem -> ${desired_fc} (was: ${current_fc:-<missing>})"
    changed=1
fi
if [[ "${current_pk}" != "${desired_pk}" ]]; then
    ln -sfn -- "${desired_pk}" "${link_pk}"
    log "repointed privkey.pem -> ${desired_pk} (was: ${current_pk:-<missing>})"
    changed=1
fi

if (( ! changed )); then
    log "no change needed; symlinks already point at ${selected}"
    exit 0
fi

# --- decide whether to nudge qedge ---------------------------------------
# Hysteria 2 (>= v2.2.3, we run v2.8.1 per stacks/qedge/docker-compose.yml)
# re-reads local TLS files on every handshake. Swapping the symlink
# targets is sufficient: existing connections keep their already-
# negotiated TLS, and the next handshake from any client picks up the
# new cert. No SIGHUP, no restart needed.
#
# We still detect running state so we can log it (operationally useful
# during a renewal post-mortem) and so we have a hook point if a future
# upstream change ever requires explicit reload.

# `docker compose ps --status running -q` returns container IDs of the
# qedge service if running. Empty stdout == not running. Run from the
# stack's compose dir.
running_id="$(cd "${QEDGE_DIR}" && docker compose ps --status running -q qedge 2>/dev/null || true)"

if [[ -n "${running_id}" ]]; then
    log "qedge is running (${running_id:0:12}); Hysteria 2 auto-reloads local TLS on next handshake — no restart issued"
else
    log "qedge is stopped (the default state); next 'docker compose up -d' will pick up the new cert"
fi

exit 0
