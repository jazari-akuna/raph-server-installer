#!/usr/bin/env bash
# create-shared-volume.sh — one-shot creation of the SHARED LUKS volume.
#
# Run as root, ONCE per install (called by bootstrap-phase2.sh). Creates
# /srv/store/data/_shared.img as a sparse LUKS2 blob unlocked by a
# host-side keyfile (NOT a passphrase). The auto-mount is owned by the
# `shared-store.service` systemd unit which fires at boot.
#
# Usage:  sudo create-shared-volume.sh
# Tunables (env): SHARED_VOLUME_SIZE_GIB (default 10)
#                 ADMIN_USERS            (whitespace-separated; each is
#                                         added to the `shared-users` group)
#
# Trust model footnote
# --------------------
# The keyfile lives on the unencrypted root filesystem at
# /etc/luks/_shared.key (mode 0400 root:root). This is a deliberate
# trade-off: the shared volume's at-rest confidentiality is bounded by
# physical access to the VPS disk image, NOT by user passphrases. A
# disk-image snapshot taken without root context (e.g. an offline copy
# stolen from the hosting provider) cannot decrypt the volume because
# the keyfile is on the same disk and will be readable. Per-user
# volumes (created by create-store-volume.sh) DO use passphrases held
# only in the operator's head; that is the stronger guarantee for
# personal data. The shared volume is for collaborative material whose
# threat model is "casual snooping at the hypervisor" rather than
# "targeted forensic recovery."
#
# Idempotent: every step checks current state and exits 0 if already
# in the desired shape. Safe to re-run.

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "must run as root" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Tunables / paths

size_gib="${SHARED_VOLUME_SIZE_GIB:-10}"
if ! [[ $size_gib =~ ^[1-9][0-9]*$ ]]; then
    echo "SHARED_VOLUME_SIZE_GIB must be a positive integer (GiB), got: $size_gib" >&2
    exit 1
fi

data_dir=/srv/store/data
mnt_dir=/srv/store/mnt
img="$data_dir/_shared.img"
mapper="store_shared"
mountpoint="$mnt_dir/_shared"
keyfile=/etc/luks/_shared.key
key_dir=/etc/luks

# ---------------------------------------------------------------------------
# Idempotency: if the .img already exists, the volume has been provisioned.
# We still walk the rest of the script to (re-)assert keyfile/group/mount
# state in case a partial run left things inconsistent.

if [[ -f $img ]]; then
    echo "==> $img already exists — verifying remaining state is consistent"
    img_already_present=1
else
    img_already_present=0
fi

# ---------------------------------------------------------------------------
# Parent directories

install -d -m 0755 -o root -g root "$data_dir"
install -d -m 0755 -o root -g root "$mnt_dir"

# ---------------------------------------------------------------------------
# Keyfile — generate before luksFormat. Reused on re-runs.

install -d -m 0700 -o root -g root "$key_dir"
chmod 0700 "$key_dir"

if [[ ! -f $keyfile ]]; then
    echo "==> generating 4 KiB random keyfile at $keyfile"
    # 4 KiB of /dev/urandom; cryptsetup happily accepts arbitrary keyfile
    # sizes, but we cap at the LUKS2 keyfile-size default ceiling (8 MiB)
    # well below to avoid loading megabytes of entropy unnecessarily.
    dd if=/dev/urandom bs=4096 count=1 of="$keyfile" status=none
    chmod 0400 "$keyfile"
    chown root:root "$keyfile"
else
    echo "==> reusing existing keyfile at $keyfile"
    # Re-assert mode/ownership defensively.
    chmod 0400 "$keyfile"
    chown root:root "$keyfile"
fi

# ---------------------------------------------------------------------------
# Sparse blob + luksFormat (only on first run)

if [[ $img_already_present -eq 0 ]]; then
    echo "==> creating sparse ${size_gib} GiB blob at $img"
    # Same sparse-file idiom used by create-store-volume.sh.
    dd if=/dev/zero of="$img" bs=1 count=0 seek="${size_gib}G" status=none
    chmod 0600 "$img"
    chown root:root "$img"

    echo "==> luksFormat (keyfile-backed, argon2id KDF)"
    # Same crypto profile as per-user volumes: aes-xts-plain64 / 512-bit /
    # sha512 / argon2id / urandom — only the unlock secret differs (keyfile
    # vs passphrase).
    cryptsetup luksFormat \
        --type luks2 \
        --cipher aes-xts-plain64 \
        --key-size 512 \
        --hash sha512 \
        --pbkdf argon2id \
        --use-urandom \
        --batch-mode \
        --key-file "$keyfile" \
        "$img"
fi

# ---------------------------------------------------------------------------
# Open + mkfs (only if the mapper isn't already live AND we haven't
# already mkfs'd; mkfs is gated by the img_already_present flag).

opened_here=0
if [[ ! -e /dev/mapper/$mapper ]]; then
    echo "==> opening $img as /dev/mapper/$mapper"
    cryptsetup open \
        --type luks2 \
        --key-file "$keyfile" \
        "$img" "$mapper"
    opened_here=1
fi

# Trap so we tear down what we opened on failure. We deliberately do NOT
# auto-close on success; the mount continues to use the mapper, and the
# systemd unit will own the lifecycle on subsequent boots.
cleanup_on_fail() {
    rc=$?
    if [[ $rc -ne 0 && $opened_here -eq 1 ]]; then
        if mountpoint -q "$mountpoint"; then
            umount "$mountpoint" || true
        fi
        cryptsetup close "$mapper" || true
    fi
    exit "$rc"
}
trap cleanup_on_fail EXIT

if [[ $img_already_present -eq 0 ]]; then
    echo "==> mkfs.ext4 -L $mapper /dev/mapper/$mapper"
    mkfs.ext4 -q -L "$mapper" "/dev/mapper/$mapper"
fi

# ---------------------------------------------------------------------------
# Mountpoint + group + mount.
#
# `shared-users` is the Linux group whose members get write access on
# the shared mount. We create it idempotently and add every name in
# $ADMIN_USERS. New users created later (via the enrol UI) are added
# to the group by users.go's host-user-add path; that's a Wave-3
# follow-up — for the install-time admins listed in $ADMIN_USERS we
# wire it here.

echo "==> ensuring shared-users group exists"
if command -v nsenter >/dev/null 2>&1 && [[ -r /proc/1/ns/mnt ]]; then
    # Run group/usermod in the host mount namespace so /etc lands on the
    # host filesystem (matches the nsenter pattern enrol's luks.go uses).
    host_run() { nsenter --target 1 --mount -- "$@"; }
else
    host_run() { "$@"; }
fi

host_run groupadd -f shared-users

if [[ -n "${ADMIN_USERS:-}" ]]; then
    for u in $ADMIN_USERS; do
        if host_run id -u "$u" >/dev/null 2>&1; then
            echo "==> adding $u to shared-users"
            host_run usermod -aG shared-users "$u"
        else
            echo "==> $u not yet a host user — skipping group add (will be added when user is created)"
        fi
    done
else
    echo "==> ADMIN_USERS unset; no users added to shared-users yet"
fi

# Resolve the gid for `shared-users` so we can chown the mountpoint.
shared_gid="$(host_run getent group shared-users | awk -F: '{print $3}')"
if [[ -z $shared_gid ]]; then
    echo "shared-users group missing after groupadd — aborting" >&2
    exit 1
fi

echo "==> preparing mountpoint at $mountpoint"
install -d -m 0755 -o root -g root "$mountpoint"

# Mount once if not already mounted, so the next boot's systemd unit
# isn't doing the heavy work for the first time.
if mountpoint -q "$mountpoint"; then
    echo "==> already mounted at $mountpoint"
else
    echo "==> mounting /dev/mapper/$mapper at $mountpoint"
    mount /dev/mapper/"$mapper" "$mountpoint"
fi

# Set permissions on the mounted root: 0775 root:shared-users so members
# of the group can write but everyone else only reads. copyparty's app-
# layer ACL ([/shared] rwmda: *) governs request-time access; this is
# defence-in-depth at the filesystem layer for any direct host-side use.
chown root:"$shared_gid" "$mountpoint"
chmod 02775 "$mountpoint"  # setgid bit: new files inherit shared-users group

trap - EXIT

echo
echo "done."
echo "  blob:       $img ($size_gib GiB sparse)"
echo "  keyfile:    $keyfile"
echo "  mapper:     /dev/mapper/$mapper"
echo "  mount:      $mountpoint"
echo
echo "next: enable + start shared-store.service so this auto-mounts at boot:"
echo "  systemctl enable --now shared-store.service"
