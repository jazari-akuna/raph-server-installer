#!/usr/bin/env bash
# create-store-volume.sh — one-time creation of an encrypted store volume.
#
# Run as root, once per user. Creates /srv/store/data/<user>.img as a sparse
# LUKS2 blob, formats it ext4, and leaves it closed. The systemd template unit
# `store-mount@<user>.service` is what unlocks it on demand thereafter.
#
# Usage:  sudo create-store-volume.sh <username> <size-GB>
# Example: sudo create-store-volume.sh alice 50
#
# Pattern derived from <https://ocv.me/doc/unix/portable-luks.sh>, adjusted to
# the cipher/KDF choices recorded in docs/design.md.

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "must run as root" >&2
    exit 1
fi

if [[ $# -ne 2 ]]; then
    echo "usage: $0 <username> <size-GB>" >&2
    exit 1
fi

user="$1"
size="$2"

if ! [[ $size =~ ^[1-9][0-9]*$ ]]; then
    echo "size must be a positive integer (GB), got: $size" >&2
    exit 1
fi

if ! id -u "$user" >/dev/null 2>&1; then
    echo "user '$user' does not exist on this host" >&2
    exit 1
fi

data_dir=/srv/store/data
mnt_dir=/srv/store/mnt
img="$data_dir/$user.img"
mapper="store_$user"
mountpoint="$mnt_dir/$user"

if [[ -e $img ]]; then
    echo "$img already exists — refusing to clobber" >&2
    exit 1
fi

# Parent directories. 0755 root:root — the per-user lockdown is on the
# mountpoint itself, not on these.
install -d -m 0755 -o root -g root "$data_dir"
install -d -m 0755 -o root -g root "$mnt_dir"

echo "==> creating sparse ${size}G blob at $img"
# `dd ... bs=1 count=0 seek=${size}G` is the maintainer-spec idiom for a
# zero-byte-actually-written sparse file of the requested apparent size.
dd if=/dev/zero of="$img" bs=1 count=0 seek="${size}G"
chmod 0600 "$img"
chown root:root "$img"

echo "==> luksFormat (you will be prompted for a passphrase)"
# luks2 + aes-xts-plain64 + 512-bit key (=256-bit XTS) + sha512 + argon2id +
# kernel /dev/urandom for the master key. Interactive: cryptsetup will
# YES-prompt then passphrase-prompt on the controlling tty.
cryptsetup luksFormat \
    --type luks2 \
    --cipher aes-xts-plain64 \
    --key-size 512 \
    --hash sha512 \
    --pbkdf argon2id \
    --use-urandom \
    "$img"

echo "==> open + mkfs + initial chown"
cryptsetup open --type luks2 "$img" "$mapper"

# Trap so we always close the mapper if anything below fails.
cleanup() {
    rc=$?
    if mountpoint -q "$mountpoint"; then
        umount "$mountpoint" || true
    fi
    if [[ -e /dev/mapper/$mapper ]]; then
        cryptsetup close "$mapper" || true
    fi
    exit "$rc"
}
trap cleanup EXIT

mkfs.ext4 -L "$mapper" "/dev/mapper/$mapper"

# Mountpoint: 0700 owned by the user. When the volume is unmounted, this
# directory itself is the user's only view — empty, and not traversable by
# anyone else. When mounted, the ext4 root inherits its own perms.
install -d -m 0700 -o "$user" -g "$user" "$mountpoint"

mount "/dev/mapper/$mapper" "$mountpoint"
# Hand the freshly-formatted filesystem root to the user.
chown "$user:$user" "$mountpoint"
chmod 0700 "$mountpoint"

umount "$mountpoint"
cryptsetup close "$mapper"
trap - EXIT

# After unmount the directory at $mountpoint is the on-disk one again
# (0700 user:user from above). Re-assert in case anything funny happened.
chown "$user:$user" "$mountpoint"
chmod 0700 "$mountpoint"

echo
echo "done. $img is ready."
echo "to unlock: systemctl start store-mount@$user.service"
