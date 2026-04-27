#!/usr/bin/env bash
# bootstrap-host.sh — Build Sequence Step 1: base host hardening.
#
# Run as root on a fresh Ubuntu 24.04 VPS, AFTER the laptop's deploy.sh has
# rsync'd this repo's host/ tree to /root/host/ (legacy laptop path) OR
# under bootstrap.sh which symlinks /root/host -> /opt/raph-server-installer/host
# (Phase 1 entrypoint expects DOMAIN to be exported and ADMIN_USERS to
# have been set; bootstrap.sh threads both through). Idempotent — safe to
# re-run.
#
# Out of scope (handled in later steps / manual follow-up):
#   - sshd hardening drop-in (installed only after admin keys are verified)
#   - ufw enable (waits until Docker / ufw-docker rules are in place)
#   - Docker, gw0, store volumes, stacks

# Strict mode + structured failure reporting (shared lib).
SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/strict.sh
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="bootstrap-host.sh"

# Preflight: only check what THIS script actually uses BEFORE installing
# anything. ufw/fail2ban are installed by this same script — don't preflight
# them. The base coreutils (install, dd, mkswap) are guaranteed.
require_root
if [[ "${TEST_MODE:-0}" != "1" ]]; then
  require_cmd apt-get useradd usermod passwd visudo systemctl install dd mkswap swapon
fi
require_env ADMIN_USERS

# Persist DOMAIN to /etc/server-domain so downstream scripts that derive
# values from it (provision-peer.sh, cert-renewal-hook.sh) Just Work even
# if the operator runs them in a fresh shell. Idempotent: only writes when
# DOMAIN is in the env, leaves an existing file alone otherwise.
if [[ -n "${DOMAIN:-}" ]]; then
  if [[ ! -f /etc/server-domain ]] \
     || [[ "$(cat /etc/server-domain 2>/dev/null)" != "$DOMAIN" ]]; then
    printf '%s\n' "$DOMAIN" > /etc/server-domain
    chmod 0644 /etc/server-domain
    echo "==> wrote /etc/server-domain ($DOMAIN)"
  fi
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

strict_step "apt full-upgrade"
echo "==> apt update + full-upgrade"
export DEBIAN_FRONTEND=noninteractive
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  echo "    TEST_MODE: skipping apt-get update + full-upgrade"
else
  apt-get update
  apt-get -y full-upgrade
fi

strict_step "install base packages"
echo "==> installing base packages"
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  echo "    TEST_MODE: skipping apt-get install (ufw fail2ban unattended-upgrades curl ca-certificates gnupg rsync)"
else
  apt-get install -y \
    ufw \
    fail2ban \
    unattended-upgrades \
    curl \
    ca-certificates \
    gnupg \
    rsync
fi

strict_step "ensure admin users"
echo "==> ensuring admin users"
for u in "${ADMINS[@]}"; do
  if id "$u" &>/dev/null; then
    echo "    user $u exists"
  else
    # A group with the same name may already exist (notably Ubuntu's default
    # `admin` group at GID 110); useradd's implicit user-private-group create
    # would fail. Reuse it as the primary group when present.
    if getent group "$u" >/dev/null; then
      useradd -m -s /bin/bash -G sudo -g "$u" "$u"
    else
      useradd -m -s /bin/bash -G sudo "$u"
    fi
    echo "    created user $u"
  fi
  # Lock the password so only key auth works. -l is idempotent.
  if [[ "${TEST_MODE:-0}" == "1" ]]; then
    echo "    TEST_MODE: skipping passwd -l $u"
  else
    passwd -l "$u" >/dev/null
  fi
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
  elif [[ "${TEST_MODE:-0}" == "1" ]]; then
    echo "    TEST_MODE: skipping authorized_keys install for $u (no key fixture present)"
    install -o "$u" -g "$u" -m 600 /dev/null "$home/.ssh/authorized_keys"
  else
    echo "    ERROR: no $per_user_keys and no $ROOT_KEYS — refusing to create admin $u without keys" >&2
    exit 1
  fi
done

strict_step "install sudoers drop-in"
echo "==> NOPASSWD sudo for sudo group"
# WHY: key-only SSH + locked passwords means sudo otherwise can't authenticate.
SUDOERS_DROPIN="/etc/sudoers.d/90-admins-nopasswd"
TMP_SUDOERS="$(mktemp)"
printf '%%sudo ALL=(ALL) NOPASSWD: ALL\n' > "$TMP_SUDOERS"
chmod 0440 "$TMP_SUDOERS"
if [[ "${TEST_MODE:-0}" == "1" ]] && ! command -v visudo >/dev/null 2>&1; then
  echo "    TEST_MODE: skipping visudo validation (not installed); installing $SUDOERS_DROPIN as-is"
  install -d -m 0755 /etc/sudoers.d
  install -m 0440 -o root -g root "$TMP_SUDOERS" "$SUDOERS_DROPIN"
  rm -f "$TMP_SUDOERS"
elif visudo -cf "$TMP_SUDOERS" >/dev/null; then
  install -m 0440 -o root -g root "$TMP_SUDOERS" "$SUDOERS_DROPIN"
  rm -f "$TMP_SUDOERS"
  # Verify perms — a wrong mode here is a CVE-grade footgun.
  got_mode="$(stat -c %a "$SUDOERS_DROPIN")"
  if [[ "$got_mode" != "440" ]]; then
    echo "    ERROR: $SUDOERS_DROPIN mode is $got_mode, expected 440" >&2
    exit 1
  fi
  echo "    installed $SUDOERS_DROPIN (mode 0440)"
else
  rm -f "$TMP_SUDOERS"
  echo "    ERROR: visudo validation failed for sudoers drop-in" >&2
  exit 1
fi

strict_step "swapfile"
echo "==> swapfile"
SWAPFILE="/swapfile"
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  echo "    TEST_MODE: skipping 4 GiB swapfile creation at $SWAPFILE"
  : > "$SWAPFILE"  # touch a placeholder so downstream existence checks pass
  chmod 600 "$SWAPFILE"
elif [[ ! -e "$SWAPFILE" ]]; then
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
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  echo "    TEST_MODE: skipping sysctl --system reload"
else
  sysctl --system >/dev/null
fi

echo "==> fail2ban jail.local"
install -d -m 0755 /etc/fail2ban
install -m 644 "$REPO_HOST_DIR/fail2ban/jail.local" /etc/fail2ban/jail.local
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  echo "    TEST_MODE: skipping systemctl enable/reload fail2ban"
else
  systemctl enable --now fail2ban
  systemctl reload fail2ban 2>/dev/null || systemctl restart fail2ban
fi

echo "==> unattended-upgrades (security only, non-interactive)"
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  echo "    TEST_MODE: skipping dpkg-reconfigure unattended-upgrades"
else
  # Pre-seed debconf so dpkg-reconfigure runs unattended.
  echo 'unattended-upgrades unattended-upgrades/enable_auto_updates boolean true' \
    | debconf-set-selections
  dpkg-reconfigure -f noninteractive unattended-upgrades
fi
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

strict_step "ufw rules (staged, not enabled)"
echo "==> ufw rules (NOT enabling yet)"
if [[ "${TEST_MODE:-0}" == "1" ]] && ! command -v ufw >/dev/null 2>&1; then
  echo "    TEST_MODE: skipping ufw rule staging (ufw not installed)"
else
  ufw default deny incoming
  ufw default allow outgoing
  ufw allow OpenSSH
  ufw allow 80/tcp
  ufw allow 443/tcp
  ufw allow 443/udp
  ufw allow 51820/udp
fi
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
