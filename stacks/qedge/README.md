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

## Cert provisioning (one-time, must run before first start)

Hysteria2 needs a cert + key on disk. We reuse the wildcard
`*.${DOMAIN}` cert that NPM (`ingress`) already provisioned
via OVH DNS-01. NPM stores certs in numbered live/ directories under
`/opt/stacks/ingress/letsencrypt/live/npm-N/` (the `N` is whatever number NPM
assigned the cert when you created it in the admin UI — typically `npm-1`
for the first wildcard, but check before you symlink).

On the VPS, as root:

```bash
cd /opt/stacks/qedge

# 1. find the right NPM cert directory (the one whose fullchain.pem covers
#    *.${DOMAIN} — usually the first wildcard you made)
ls -la /opt/stacks/ingress/letsencrypt/live/

# 2. confirm the cert is the wildcard you expect
openssl x509 -in /opt/stacks/ingress/letsencrypt/live/npm-1/fullchain.pem \
    -noout -text | grep -E 'Subject:|DNS:'

# 3. create ./tls/ and symlink the cert pair in.
#    Replace `npm-1` with the actual directory name from step 1.
install -d -m 0750 ./tls
ln -sfn /opt/stacks/ingress/letsencrypt/live/npm-1/fullchain.pem ./tls/fullchain.pem
ln -sfn /opt/stacks/ingress/letsencrypt/live/npm-1/privkey.pem   ./tls/privkey.pem

# 4. sanity check that the symlinks resolve (readable by the container's
#    UID — Hysteria runs as root in the upstream image, so plain root-readable
#    is fine)
ls -lL ./tls/
```

NPM auto-renews the cert in place; the symlinks keep pointing at the live
files, so `qedge` always picks up the renewed cert at next start. No cron
needed on the qedge side.

If you ever rotate the wildcard cert in NPM and it lands in a NEW numbered
dir (`npm-2`, etc.), re-point the symlinks and `docker compose restart
qedge` (only if the stack is currently up — usually it isn't).

## Initial config render

The `config.yaml.template` in this directory uses `${QEDGE_PASSWORD}` as a
placeholder for the auth password. Render it once, on the VPS:

```bash
cd /opt/stacks/qedge

# 1. copy .env.example to .env and fill in QEDGE_PASSWORD
cp .env.example .env
chmod 0600 .env
# edit .env, set QEDGE_PASSWORD=$(openssl rand -base64 32)

# 2. render the live config
install -d -m 0750 ./config
set -a; . ./.env; set +a
envsubst < config.yaml.template > ./config/config.yaml
chmod 0640 ./config/config.yaml

# 3. sanity check — no unsubstituted ${...} should remain
grep -F '${' ./config/config.yaml || echo "config render OK"
```

Re-run the render step any time you rotate `QEDGE_PASSWORD` (see "Rotating
the password" below).

## Switchover procedure: gw0 → qedge

Run as root on the VPS. Both steps are required: leaving `gw0` up while
`qedge` is up is fine on different ports, but the whole point of bringing
`qedge` up is that `gw0` isn't working — stop it so peers don't waste
reconnect attempts on a dead path.

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
