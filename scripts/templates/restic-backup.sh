#!/usr/bin/env bash
# restic-backup.sh — laptop-side daily pull-backup of /srv/store/data/<USER>.img.
#
# Runs on the ADMIN'S LAPTOP. Never on the VPS. The server holds no backup
# credentials; this script is the entire backup pipeline.
#
# Pipeline:
#   1. rsync /srv/store/data/<USER>.img from the VPS into a local staging dir
#      using a dedicated SSH key (server-side authorized_keys forces the rsync
#      command — see docs/backups.md for the exact authorized_keys line).
#   2. `restic backup` the staged file into a local repo. Restic deduplicates
#      against prior snapshots, so wire/disk cost is proportional to the
#      changed ciphertext bytes, not the full blob size.
#   3. `restic forget --prune` to age out old snapshots per the retention
#      policy below.
#
# Placeholders to substitute when installing this template per laptop:
#   <USER>      — sagan | marcus  (the VPS account whose .img is being pulled)
#   <VPS_HOST>  — the SSH-reachable hostname of the VPS
#                 (e.g. antarctica-engineering.com)
#
# Substitute via:
#   sed "s/<USER>/sagan/g; s|<VPS_HOST>|antarctica-engineering.com|g" \
#       restic-backup.sh > ~/.local/bin/restic-backup-sagan.sh
#   chmod 0755 ~/.local/bin/restic-backup-sagan.sh
#
# Expected layout on the laptop (created on demand below where reasonable):
#   ~/.ssh/id_restic_<USER>                       # dedicated keypair (0600)
#   ~/.config/restic-rarcus/<USER>.passwd         # restic repo password (0600)
#   ~/.local/share/restic-rarcus/<USER>/          # restic repo (created by `restic init`)
#   ~/.cache/restic-rarcus/<USER>/                # rsync staging dir
#
# See docs/backups.md for one-time setup, recovery procedure, and operational
# rules.

set -euo pipefail

USER_NAME="<USER>"
VPS_HOST="<VPS_HOST>"

STAGING_DIR="${HOME}/.cache/restic-rarcus/${USER_NAME}"
RESTIC_REPO="${HOME}/.local/share/restic-rarcus/${USER_NAME}"
RESTIC_KEY="${HOME}/.ssh/id_restic_${USER_NAME}"
PASSWORD_FILE="${HOME}/.config/restic-rarcus/${USER_NAME}.passwd"

# --- preconditions ----------------------------------------------------------

if [[ "${USER_NAME}" == "<USER>" || "${VPS_HOST}" == "<VPS_HOST>" ]]; then
    echo "error: placeholders not substituted — edit USER_NAME and VPS_HOST" >&2
    exit 1
fi

for bin in rsync restic ssh; do
    if ! command -v "${bin}" >/dev/null 2>&1; then
        echo "error: '${bin}' not found in PATH" >&2
        exit 1
    fi
done

if [[ ! -r "${RESTIC_KEY}" ]]; then
    echo "error: SSH key not readable: ${RESTIC_KEY}" >&2
    exit 1
fi

if [[ ! -r "${PASSWORD_FILE}" ]]; then
    echo "error: restic password file not readable: ${PASSWORD_FILE}" >&2
    exit 1
fi

if [[ ! -d "${RESTIC_REPO}" ]]; then
    echo "error: restic repo not initialized at ${RESTIC_REPO}" >&2
    echo "       run: restic init -r ${RESTIC_REPO} --password-file ${PASSWORD_FILE}" >&2
    exit 1
fi

# --- staging dir ------------------------------------------------------------

mkdir -p "${STAGING_DIR}"
chmod 0700 "${STAGING_DIR}"

# --- 1. pull the .img via rsync over the dedicated key ----------------------
#
# `--inplace` ensures rsync overwrites the existing staged copy in-place
# rather than writing a temp file and rename — keeps disk usage flat at one
# blob's worth, and restic's content-defined chunking handles the dedup just
# fine without filename stability tricks.
#
# `BatchMode=yes` makes ssh fail rather than prompt for a passphrase or host
# key confirmation; the timer-driven service has no tty.

rsync \
    --inplace \
    --partial \
    --times \
    -e "ssh -i ${RESTIC_KEY} -o BatchMode=yes -o ConnectTimeout=30" \
    "${USER_NAME}@${VPS_HOST}:/srv/store/data/${USER_NAME}.img" \
    "${STAGING_DIR}/${USER_NAME}.img"

# --- 2. restic backup the staged file ---------------------------------------

export RESTIC_REPOSITORY="${RESTIC_REPO}"
export RESTIC_PASSWORD_FILE="${PASSWORD_FILE}"

restic backup \
    --tag daily \
    --tag "user:${USER_NAME}" \
    "${STAGING_DIR}/${USER_NAME}.img"

# --- 3. retention -----------------------------------------------------------
#
# Keep:
#   - 14 daily snapshots  (recent history at full granularity)
#   -  8 weekly snapshots (~2 months at weekly granularity)
#   - 12 monthly snapshots (~1 year at monthly granularity)
# `--prune` rewrites pack files to actually free space; without it, forget
# only removes snapshot pointers and the blobs hang around.

restic forget \
    --keep-daily 14 \
    --keep-weekly 8 \
    --keep-monthly 12 \
    --prune

# --- 4. integrity check (cheap variant) -------------------------------------
#
# `restic check` without --read-data verifies repo metadata structure but
# doesn't re-read every pack. Cheap enough to run every time. The deeper
# `--read-data-subset 5%` belongs in a less-frequent timer if at all.

restic check
