# console

Docker management UI for the shared VPS. Internally this is Portainer CE;
externally and in all paths/names it is `console`. See `docs/plan.md` step 4.

## Hard rules

- **Never bind to `0.0.0.0`.** Loopback only. Reach the UI via SSH tunnel
  or via the `mesh` (Tailscale) overlay once that stack is up.
- **Never publish a public proxy host for `console` in `ingress`.** This
  is the highest-value compromise target on the box.
- **Enable MFA in Portainer immediately after the first admin login.**
- **Set per-stack memory limits via Portainer.** Host has 4 GB RAM total
  and `cloud` + `ingress` + `gw0` + `mesh` already eat into it.

## First-time bootstrap (Option A — one-shot `docker run`)

This is the chicken-and-egg path used the very first time, before this
compose file has been deployed. Run on the VPS as a user in the `docker`
group:

```sh
# create the named volume and the shared edge network up front
docker volume create portainer_data
docker network create edge   # idempotent; harmless if already created

docker run -d \
  --name console \
  --restart unless-stopped \
  --memory 256m \
  --network edge \
  -p 127.0.0.1:9443:9443 \
  -p 127.0.0.1:8000:8000 \
  -v portainer_data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  portainer/portainer-ce:2.39.1
```

Then immediately tunnel in and create the initial admin account
(see *Access* below). **Portainer wipes the admin-creation form ~5
minutes after first boot for security**; if you miss the window,
`docker restart console` and reconnect within the new window.

After the admin accounts exist, tear down the one-shot container and
adopt the durable compose form:

```sh
docker rm -f console
cd /opt/stacks/console
docker compose up -d
```

The named volume `portainer_data` carries the admin accounts forward,
so you do **not** redo the bootstrap form.

## Steady state (durable form)

From `/opt/stacks/console/` on the VPS:

```sh
docker compose up -d
docker compose ps
docker compose logs -f console
```

To upgrade the pinned image, edit the tag in `docker-compose.yml`,
commit on the laptop, deploy, then:

```sh
docker compose pull && docker compose up -d
```

## Access

The UI is bound to `127.0.0.1:9443` on the VPS. Two supported paths:

### SSH tunnel (always works)

```sh
ssh -L 9443:127.0.0.1:9443 sagan@<vps>
# then open https://localhost:9443 in a browser
```

Self-signed cert on first boot — accept the warning; the cert never
leaves the loopback path. Replace with the wildcard cert later if
desired (not required, since traffic is loopback-only).

### Mesh (preferred once `mesh` is up)

After step 10 in the build sequence, both admins are on the `mesh`
overlay. Reach the UI directly via Tailscale magic DNS:

```
https://<vps-magicdns-name>:9443
```

No SSH tunnel needed. The `mesh` interface is the only non-loopback
path that ever reaches port 9443; nothing on the public internet can
touch it.

## Initial admin setup

1. Create the `sagan` admin account within ~5 minutes of first boot.
2. Create the `marcus` admin account (same admin role; co-admins).
3. **Enable MFA** for both accounts (`Settings -> Authentication`).
4. Connect the local Docker environment (it is auto-detected via the
   mounted socket; no extra setup needed).
5. Confirm the `edge` network is visible under
   `Environments -> local -> Networks`.

## Operational notes

- The docker socket is mounted **read-only** by default. Many Portainer
  features (deploy stack, pull image, container start/stop) require
  write access. If you hit "permission denied" on those operations,
  drop the `:ro` from the socket mount — and understand that this then
  makes a Portainer compromise equivalent to host root. Keep MFA on.
- The `:8000` edge-agent tunnel is included but only used if remote
  Edge agents are adopted. Safe to leave bound to loopback. Remove the
  port line entirely if you are sure you will never use it.
- Per-stack memory limits: every stack deployed via `console` should
  declare `mem_limit` (compose v2) or the equivalent in the Portainer
  stack form. Budget against ~1.5 GB total headroom on a 4 GB host
  after `ingress` / `cloud` / `gw0` / `mesh` are running.
