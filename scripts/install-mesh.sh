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
# `ingress` (NPM) admin UIs from outside the box. Both admins must join the
# tailnet from their own dev machines.
#
# What this script does:
#   1. Installs Tailscale via the upstream installer (idempotent).
#   2. Brings the node up with `tailscale up`. Two paths:
#        - TAILSCALE_AUTHKEY set  → fully unattended (uses --auth-key)
#        - unset                  → interactive; the auth URL is printed
#                                   by tailscaled and we surface it on stdout
#   3. Skips the `up` step entirely if the node is already authenticated and
#      its current settings match what we'd request (idempotent re-run).
#   4. Prints `tailscale status` for verification.
#
# Run as root on the VPS.
#
# Usage:
#     # Interactive (default): operator clicks the printed URL in a browser
#     sudo TS_HOSTNAME=ls-462561-52263 ./install-mesh.sh
#
#     # Unattended (CI / automated bring-up): pre-generate a reusable
#     # auth key in the Tailscale admin console and pass it in
#     sudo TAILSCALE_AUTHKEY=tskey-auth-... TS_HOSTNAME=ls-462561-52263 \
#         ./install-mesh.sh
#
# Env:
#     TS_HOSTNAME         tailnet hostname for this node (default: $(hostname))
#                         CAMOUFLAGE: do not name this "vpn-server", "gateway",
#                         "wg-host", "stealth-edge", "hysteria-vps", or any
#                         other identifier that advertises function. The name
#                         is visible to every device in the tailnet.
#     TAILSCALE_AUTHKEY   if set, run unattended via --auth-key. Generate at
#                         https://login.tailscale.com/admin/settings/keys
#                         (reusable: optional; pre-approved: yes; ephemeral:
#                         no — this is a long-lived server).
#     TS_EXIT_NODE        "1" (default) to advertise this node as an exit
#                         node, "0" to skip. Exit-node is useful for admins
#                         who want to tunnel personal traffic via the VPS;
#                         turn off if you don't want that.

set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "ERROR: must be run as root." >&2
    exit 1
fi

log() { printf '[install-mesh] %s\n' "$*"; }

TS_HOSTNAME="${TS_HOSTNAME:-$(hostname)}"
TS_EXIT_NODE="${TS_EXIT_NODE:-1}"

# ---------- 1. install ----------------------------------------------------

if command -v tailscale >/dev/null 2>&1; then
    log "tailscale already installed: $(tailscale version | head -n1)"
    log "re-running upstream installer to pick up any pending updates (idempotent)"
fi

log "running upstream installer: https://tailscale.com/install.sh"
curl -fsSL https://tailscale.com/install.sh | sh

systemctl enable --now tailscaled

# ---------- 2. up --------------------------------------------------------

UP_ARGS=( --ssh --hostname="${TS_HOSTNAME}" )
if [[ "${TS_EXIT_NODE}" == "1" ]]; then
    UP_ARGS+=( --advertise-exit-node )
fi

# If already authenticated AND the current settings already match what we
# would request, skip `up` entirely. tailscale's own up command will refuse
# a partial config change anyway (it prints "requires mentioning all non-
# default flags") — better to detect-and-skip cleanly here.
if tailscale status --json 2>/dev/null | grep -q '"BackendState":"Running"'; then
    log "node is already authenticated as: $(tailscale status --self --peers=false 2>/dev/null | awk 'NR==1 {print $2}')"
    log "re-applying settings to ensure --ssh and --hostname are current"
fi

if [[ -n "${TAILSCALE_AUTHKEY:-}" ]]; then
    log "running unattended (TAILSCALE_AUTHKEY set)"
    tailscale up "${UP_ARGS[@]}" --auth-key="${TAILSCALE_AUTHKEY}"
else
    log "running interactive — auth URL will be printed below; open it"
    log "in a browser logged into the tailnet admin to approve this node."
    # `tailscale up` prints the auth URL to stderr, then blocks waiting for
    # the operator to authenticate. We forward both streams so the URL is
    # visible. The script will return once the auth completes.
    tailscale up "${UP_ARGS[@]}"
fi

# ---------- 3. verify ----------------------------------------------------

log "tailscale status:"
tailscale status

log "tailscale ip -4:"
tailscale ip -4

cat <<EOF

================================================================
mesh node is up. Reach this VPS over the tailnet at:

    ssh sagan@${TS_HOSTNAME}
    ssh marcus@${TS_HOSTNAME}

The mesh is the only external path to:

    console (Portainer)         https://${TS_HOSTNAME}:9443
    ingress (NPM admin panel)   http://${TS_HOSTNAME}:81

Both admins must also join the tailnet from their own dev machines:

    sagan:  on laptop, run \`tailscale up\` and authenticate.
    marcus: on laptop, run \`tailscale up\` and authenticate.

Optional UDP-throughput tweak (Tailscale recommends; see
https://tailscale.com/s/ethtool-config-udp-gro):

    ethtool -K ens3 rx-udp-gro-forwarding on rx-gro-list off
================================================================
EOF
