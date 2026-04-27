#!/usr/bin/env bash
# mount-stores.sh — interactive (re)mount of every store volume that exists.
#
# Run after every reboot, by an admin, on the VPS. For each known user, if
# their blob exists and isn't currently mounted, kick the systemd template
# unit which will prompt for the passphrase via the system password agent.
#
# Idempotent: skips users whose mountpoint is already a live mount.

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "must run as root (systemctl start needs it)" >&2
    exit 1
fi

data_dir=/srv/store/data
mnt_dir=/srv/store/mnt

# Discover users from the *.img files present in $data_dir. The store image
# filename is the user's login name. This avoids hardcoding admin usernames
# into the script and stays correct as users are added/removed via the wizard.
users=()
shopt -s nullglob
for img in "$data_dir"/*.img; do
    bn="$(basename "$img" .img)"
    [[ "$bn" == "_shared" ]] && continue   # shared volume handled separately
    users+=("$bn")
done
shopt -u nullglob

if [[ ${#users[@]} -eq 0 ]]; then
    echo "no per-user store images found in $data_dir"
    exit 0
fi

# If systemd-tty-ask-password-agent is available and we have a tty, hint that
# the user can answer prompts here. (The unit uses --no-tty so the prompt
# arrives via the password-agent infrastructure regardless.)
if [[ -t 0 ]] && command -v systemd-tty-ask-password-agent >/dev/null; then
    echo "tip: if a prompt does not appear, run in another shell:"
    echo "     systemd-tty-ask-password-agent --query"
    echo
fi

for user in "${users[@]}"; do
    img="$data_dir/$user.img"
    mountpoint="$mnt_dir/$user"

    if [[ ! -e $img ]]; then
        echo "[$user] no blob at $img — skipping"
        continue
    fi

    if mountpoint -q "$mountpoint"; then
        echo "[$user] already mounted at $mountpoint — skipping"
        continue
    fi

    echo "[$user] starting store-mount@$user.service"
    if systemctl start "store-mount@$user.service"; then
        echo "[$user] mounted at $mountpoint"
    else
        echo "[$user] FAILED — check: systemctl status store-mount@$user.service" >&2
    fi
done

echo
echo "summary:"
for user in "${users[@]}"; do
    mountpoint="$mnt_dir/$user"
    if mountpoint -q "$mountpoint"; then
        echo "  $user: mounted"
    else
        echo "  $user: NOT mounted"
    fi
done
