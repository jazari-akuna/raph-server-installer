#!/usr/bin/env bash
# bootstrap-host.sh — Build Sequence Step 1: base host hardening.
#
# Run as root on a fresh Ubuntu 24.04 VPS, AFTER the laptop's deploy.sh has
# rsync'd this repo's host/ tree to /root/host/. Idempotent — safe to re-run.
#
# Out of scope (handled in later steps / manual follow-up):
#   - sshd hardening drop-in (installed only after admin keys are verified)
#   - ufw enable (waits until Docker / ufw-docker rules are in place)
#   - Docker, gw0, store volumes, stacks

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "must run as root" >&2
  exit 1
fi

REPO_HOST_DIR="/root/host"
# ADMIN_USERS is supplied by the bootstrap orchestrator (whitespace-separated).
# In the turnkey installer flow the wizard creates exactly one admin, so the
# array has a single entry. The script supports multiple entries for operators
# who pre-seed admins through ADMIN_USERS before running this step.
read -r -a ADMINS <<<"${ADMIN_USERS:-}"
if [[ ${#ADMINS[@]} -eq 0 ]]; then
  echo "ADMIN_USERS env var must list one or more admin usernames" >&2
  exit 1
fi

echo "==> apt update + full-upgrade"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get -y full-upgrade

echo "==> installing base packages"
apt-get install -y \
  ufw \
  fail2ban \
  unattended-upgrades \
  curl \
  ca-certificates \
  gnupg \
  rsync

echo "==> ensuring admin users"
for u in "${ADMINS[@]}"; do
  if id "$u" &>/dev/null; then
    echo "    user $u exists"
  else
    useradd -m -s /bin/bash -G sudo "$u"
    echo "    created user $u"
  fi
  # Lock the password so only key auth works. -l is idempotent.
  passwd -l "$u" >/dev/null
done

echo "==> installing per-user authorized_keys"
ROOT_KEYS="/root/.ssh/authorized_keys"
for u in "${ADMINS[@]}"; do
  home="/home/$u"
  install -d -o "$u" -g "$u" -m 700 "$home/.ssh"
  per_user_keys="${REPO_HOST_DIR}/ssh/keys/${u}.authorized_keys"
  if [[ -f "$per_user_keys" ]]; then
    install -o "$u" -g "$u" -m 600 "$per_user_keys" "$home/.ssh/authorized_keys"
    echo "    $u: installed from $per_user_keys"
  elif [[ -f "$ROOT_KEYS" ]]; then
    install -o "$u" -g "$u" -m 600 "$ROOT_KEYS" "$home/.ssh/authorized_keys"
    echo "    $u: fell back to mirroring $ROOT_KEYS"
  else
    echo "    ERROR: no $per_user_keys and no $ROOT_KEYS — refusing to create admin $u without keys" >&2
    exit 1
  fi
done

echo "==> NOPASSWD sudo for sudo group"
# WHY: key-only SSH + locked passwords means sudo otherwise can't authenticate.
SUDOERS_DROPIN="/etc/sudoers.d/90-admins-nopasswd"
TMP_SUDOERS="$(mktemp)"
printf '%%sudo ALL=(ALL) NOPASSWD: ALL\n' > "$TMP_SUDOERS"
chmod 0440 "$TMP_SUDOERS"
if visudo -cf "$TMP_SUDOERS" >/dev/null; then
  install -m 0440 -o root -g root "$TMP_SUDOERS" "$SUDOERS_DROPIN"
  rm -f "$TMP_SUDOERS"
  echo "    installed $SUDOERS_DROPIN"
else
  rm -f "$TMP_SUDOERS"
  echo "    ERROR: visudo validation failed for sudoers drop-in" >&2
  exit 1
fi

echo "==> swapfile"
SWAPFILE="/swapfile"
if [[ ! -e "$SWAPFILE" ]]; then
  # fallocate is fast but produces a file ext4/xfs may reject for swap on some
  # configs; dd is the safe, universally-accepted form.
  dd if=/dev/zero of="$SWAPFILE" bs=1M count=4096 status=progress
  chmod 600 "$SWAPFILE"
  mkswap "$SWAPFILE"
  swapon "$SWAPFILE"
  echo "    created 4 GB swapfile"
else
  echo "    $SWAPFILE exists"
  swapon --show | grep -q "^$SWAPFILE " || swapon "$SWAPFILE" || true
fi
if ! grep -qE "^${SWAPFILE}[[:space:]]" /etc/fstab; then
  printf '%s none swap sw 0 0\n' "$SWAPFILE" >> /etc/fstab
  echo "    added $SWAPFILE to /etc/fstab"
fi

echo "==> sysctl drop-in"
install -m 644 "$REPO_HOST_DIR/sysctl/99-host.conf" /etc/sysctl.d/99-host.conf
sysctl --system >/dev/null

echo "==> fail2ban jail.local"
install -m 644 "$REPO_HOST_DIR/fail2ban/jail.local" /etc/fail2ban/jail.local
systemctl enable --now fail2ban
systemctl reload fail2ban 2>/dev/null || systemctl restart fail2ban

echo "==> unattended-upgrades (security only, non-interactive)"
# Pre-seed debconf so dpkg-reconfigure runs unattended.
echo 'unattended-upgrades unattended-upgrades/enable_auto_updates boolean true' \
  | debconf-set-selections
dpkg-reconfigure -f noninteractive unattended-upgrades
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF
# Strip non-security origins from the allowed list so only -security lands
# unattended. Re-applies cleanly on every run.
cat > /etc/apt/apt.conf.d/50unattended-upgrades.local <<'EOF'
Unattended-Upgrade::Allowed-Origins {
    "${distro_id}:${distro_codename}-security";
    "${distro_id}ESMApps:${distro_codename}-apps-security";
    "${distro_id}ESM:${distro_codename}-infra-security";
};
Unattended-Upgrade::Automatic-Reboot "false";
EOF

echo "==> ufw rules (NOT enabling yet)"
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 443/udp
ufw allow 51820/udp
echo
echo "    NOTE: ufw is configured but NOT enabled. Docker installs its own"
echo "    iptables rules and ufw-docker integration is added in Build Step 3."
echo "    Run 'ufw enable' only after that step, or you'll lock out Docker"
echo "    networking and possibly your SSH session."

cat <<'EOF'

================================================================
Step 1 base hardening: DONE
================================================================

NEXT: install the sshd hardening drop-in — but ONLY after you have
verified that every admin key works in a fresh session.

Verification commands (run from your laptop, in a NEW terminal,
keeping this root session open as a safety net):

    ssh <admin>@<vps-ip>  'whoami && sudo -n true || sudo -v'

Repeat for each admin. All must succeed. Then, on the VPS as root:

    install -m 644 /root/host/ssh/sshd_config.d/99-hardening.conf \
        /etc/ssh/sshd_config.d/99-hardening.conf
    sshd -t && systemctl reload ssh

After reload, open ANOTHER fresh ssh as <admin> before closing
this root session. If the new session works, you're done with Step 1.

Reminder: ufw is staged but not active. Enable it in Step 3 after
Docker + ufw-docker are configured.
EOF
