#!/usr/bin/env bash
#
# install-mesh.sh — Build Sequence Step 10: install the admin overlay (Tailscale).
#
# Camouflage: the overlay is referred to as `mesh` everywhere in this repo and
# in any operational doc. The package itself is `tailscale` — package names are
# explicitly out of scope for the camouflage layer (see docs/plan.md). What we
# DO control: the host's tailnet name (the `--hostname=` we recommend below)
# and the fact that no public DNS / no inbound public port advertises this
# service. The mesh is the ONLY external path to the `console` (Portainer) and
# `ingress` (NPM) admin UIs from outside the box. Both admins (sagan, marcus)
# must join the tailnet from their own dev machines.
#
# What this script does:
#   1. Installs Tailscale via the upstream installer (idempotent — safe to
#      re-run; the installer is a no-op if already installed and up to date).
#   2. Prints next-step INSTRUCTIONS for the operator. It does NOT auto-run
#      `tailscale up` because that flow is interactive (it prints a one-shot
#      auth URL that has to be opened in a browser and approved against the
#      tailnet's admin console). Auto-running it would either hang waiting on
#      stdin or burn the URL into the journal.
#   3. Echoes verification commands (`tailscale status`, `tailscale ip -4`)
#      for the operator to run after authenticating.
#
# Run as root on the VPS.

set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "ERROR: must be run as root." >&2
    exit 1
fi

log() { printf '[install-mesh] %s\n' "$*"; }

# ---------- 1. install ----------------------------------------------------

if command -v tailscale >/dev/null 2>&1; then
    log "tailscale already installed: $(tailscale version | head -n1)"
    log "re-running upstream installer to pick up any pending updates (idempotent)"
fi

log "running upstream installer: https://tailscale.com/install.sh"
curl -fsSL https://tailscale.com/install.sh | sh

# The upstream installer enables and starts the tailscaled service unit.
# Make sure that's actually true before we tell the operator to run `up`.
systemctl enable --now tailscaled

# ---------- 2. recommend a neutral hostname -------------------------------
#
# Tailscale derives the default tailnet hostname from the box's `hostname`.
# That's USUALLY fine — but the camouflage rule (docs/plan.md, claude-readme.md)
# is that nothing visible should advertise function. So:
#
#   * GOOD: the box's existing neutral hostname (e.g. "hk-edge-01"), or any
#           generic identifier that doesn't say what the box does.
#   * BAD:  "vpn-server", "gateway", "stealth", "wg-host", "hysteria-vps",
#           anything containing the camouflaged words.
#
# The tailnet hostname is visible to every device in the tailnet. If marcus's
# laptop is ever inspected, that name is on it. Pick neutrally.

CURRENT_HOSTNAME="$(hostname)"

cat <<EOF

================================================================
  mesh (Tailscale) is installed. Next step is INTERACTIVE.
================================================================

Run, as root, on this VPS:

    tailscale up --ssh --hostname=${CURRENT_HOSTNAME}

  - Replace ${CURRENT_HOSTNAME} with whatever neutral identifier you want
    this node to appear as in the tailnet. The current system hostname
    (${CURRENT_HOSTNAME}) is fine IF it is already neutral.
  - Do NOT use names like "vpn-server", "gateway", "wg-host",
    "stealth-edge", "hysteria-vps", or anything that advertises function.
    Camouflage rule: see docs/plan.md and claude-readme.md.
  - The --ssh flag enables Tailscale SSH on this node so admins can reach
    a shell over the mesh without exposing a public sshd more than already.

The command will print a one-shot auth URL. Open it in a browser logged
into the tailnet's admin account and approve this node.

Both admins must join the tailnet from their own dev machines:

    sagan:  on laptop, run \`tailscale up\` and authenticate.
    marcus: on laptop, run \`tailscale up\` and authenticate.

The mesh is the ONLY external path to:

    * console (Portainer)         https://<vps-mesh-name>:9443
    * ingress (NPM admin panel)   http://<vps-mesh-name>:81

Neither of those has a public DNS record. Do not add one.

After running \`tailscale up\` and approving the node, verify with:

    tailscale status
    tailscale ip -4

================================================================
EOF
