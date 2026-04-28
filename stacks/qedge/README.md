# qedge — alternate ingress (runbook)

Internally: Hysteria 2 server on `:443/udp`. Stays **stopped by default**.
Started only when the primary `gw0` (AmneziaWG) gateway path is silent under
heavy regional pressure and an admin has decided to switch over.

This README is the operational runbook for that switchover. Everything else
about why this stack exists is in `docs/design.md` (Step 11).

## When to bring this up

You bring `qedge` up when, **and only when**, all of the following hold:

- `gw0` traffic from a probe client is being dropped or reset within
  seconds of bringing the tunnel up — i.e. active interference, not
  just packet loss.
- You've already verified the VPS itself is reachable (`mesh` works, the box
  responds to pings or HTTPS on `cloud.${DOMAIN}`).
- You've confirmed with the other admin (out-of-band) that the switch is
  happening, so neither of you wastes time debugging the wrong path.

If any of those is false: don't switch. The default daily driver is `gw0`.

## One-time setup (config render + cert symlinks)

Both the rendered Hysteria2 config and the TLS material live under a
single `./config/` bind, mounted into the container at `/etc/hysteria`.
Why one bind and not two: docker can't `mkdir` a child mountpoint
(`/etc/hysteria/tls`) inside a `:ro` parent bind, so the historical
two-mount layout (`./config:/etc/hysteria:ro` + `./tls:/etc/hysteria/tls:ro`)
fails container start with `read-only file system`. Putting `tls/` *under*
`./config/` collapses to one mount and works cleanly. The compose file
also mounts NPM's letsencrypt live store read-only at the same host path
inside the container so the cert symlinks resolve.

On the VPS, as root:

```bash
cd /opt/stacks/qedge

# 1. mint the auth password into .env
cp .env.example .env
chmod 0600 .env
sed -i "s|^QEDGE_PASSWORD=.*|QEDGE_PASSWORD=$(openssl rand -base64 32)|" .env

# 2. render the live config from the template
install -d -m 0750 ./config
set -a; . ./.env; set +a
envsubst < config.yaml.template > ./config/config.yaml
chmod 0640 ./config/config.yaml
grep -F '${' ./config/config.yaml || echo "config render OK"

# 3. find the wildcard cert dir (NPM uses either live/${DOMAIN}/ or
#    live/npm-N/ depending on how the cert was created)
ls -la /opt/stacks/ingress/letsencrypt/live/
NPM_LIVE=/opt/stacks/ingress/letsencrypt/live/<your-cert-dir>
openssl x509 -in "${NPM_LIVE}/fullchain.pem" -noout -text \
    | grep -E 'Subject:|DNS:'

# 4. symlink the cert pair under ./config/tls/. NPM auto-renews in place,
#    so the symlinks always point at the current cert; no watcher needed.
install -d -m 0750 ./config/tls
ln -sfn "${NPM_LIVE}/fullchain.pem" ./config/tls/fullchain.pem
ln -sfn "${NPM_LIVE}/privkey.pem"   ./config/tls/privkey.pem
ls -lL ./config/tls/
```

If the wildcard cert ever lands in a different live/ dir, re-point the
symlinks and `docker compose restart qedge` (only if the stack is up —
usually it isn't).

Re-run step 2 any time you rotate `QEDGE_PASSWORD` (see "Rotating the
password" below).

## Switchover procedure: gw0 → qedge

Run as root on the VPS. Both steps are required, and order matters:
`gw0` and `qedge` both bind UDP/443 (gw0 was moved off 51820 to dodge
non-443 UDP shaping), so only one can be up at a time. Stop `gw0` first,
then start `qedge`.

```bash
# 1. stop the primary gateway
systemctl stop awg-quick@gw0

# 2. bring up qedge
cd /opt/stacks/qedge
docker compose up -d

# 3. verify
docker compose ps              # qedge should be Up
docker compose logs --tail=50  # look for "server up and running"
ss -ulpn | grep ':443'         # 443/udp should be listening
```

You can also start/stop `qedge` from `console` (Portainer) — it's deployed
as a stack there. Same effect.

## Switchover procedure: qedge → gw0 (going back)

```bash
# 1. tear qedge back down
cd /opt/stacks/qedge
docker compose down

# 2. bring the primary gateway back
systemctl start awg-quick@gw0
systemctl status awg-quick@gw0
```

Confirm with the other admin out-of-band before flipping back, same as for
the forward switch.

## Client side

A pre-prepared **sing-box** client config is the recommended client for the
`qedge` path. The pattern (full config generation is handled by the peer
provisioning script later — out of scope for this stack):

- Outbound: Hysteria2 to `cdn.${DOMAIN}:443/udp`,
  using `QEDGE_PASSWORD`, with `tls.server_name` =
  `cdn.${DOMAIN}` (matches the wildcard SNI the server
  presents).
- Route rules: built-in GeoIP database, with `{"geoip": ["cn"], "outbound":
  "direct"}` for domestic-direct split routing, and a default `proxy`
  outbound for everything else. This mirrors the regional-split behavior of
  the `gw0` path, just driven by GeoIP DB lookup on the client instead of
  by the `AllowedIPs` complement.

Hand each admin their pre-rendered sing-box profile (one per device) at the
same time you hand them the `gw0` peer config. Stash it; activate when the
switchover happens.

## Rotating the password

When to rotate:

- After any switchover (assume the password may have been observed in the
  wild during the active-block window).
- When re-issuing the pre-prepared sing-box client profile to either admin.
- On a routine cadence — at minimum yearly.

Procedure:

```bash
cd /opt/stacks/qedge

# 1. mint a new password into .env
NEW="$(openssl rand -base64 32)"
sed -i "s|^QEDGE_PASSWORD=.*|QEDGE_PASSWORD=${NEW}|" .env
chmod 0600 .env

# 2. re-render the live config from the template
set -a; . ./.env; set +a
envsubst < config.yaml.template > ./config/config.yaml
chmod 0640 ./config/config.yaml

# 3. if qedge is currently UP, restart it; if it's stopped (the default),
#    skip this — the new password will load on the next switchover.
if docker compose ps --status running --quiet | grep -q .; then
    docker compose restart
fi

# 4. distribute the new password to the other admin via the mesh / sealed
#    note (NEVER plaintext mail / chat). They re-render their sing-box
#    client profile with the new password before the next switchover.
```

## Constraints recap

- `restart: "no"` in compose — must NOT be `unless-stopped`. Stays cold.
- `:443/udp` only. NPM owns `:443/tcp`. Do not add a TCP mapping.
- `.env` is gitignored. `.env.example` ships with an empty placeholder.
- Camouflage: nothing in this stack's paths, container name, or SNI says
  `hysteria` / `quic` / `vpn` / `tunnel`. The internal name is `qedge`; the
  public hostname is `cdn.${DOMAIN}`.
