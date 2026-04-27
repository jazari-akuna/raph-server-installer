# Maintenance & Ops Runbook

Day-2 operations playbook for any VPS provisioned by
`raph-server-installer`. Everything in this doc happens **after**
initial bring-up — `docs/design.md` covers the architecture and build
sequence, `docs/verification.md` covers sign-off, this file covers
keeping the box healthy thereafter.

The procedures below are written for a generic operator. Substitute
your own values for the placeholders — `<your-domain>` is your apex,
`<admin>` is the admin OS account that owns SSH access, `<user>` is
any account managed through the `enrol` UI.

> Where a procedure is documented in detail elsewhere, this file links
> rather than copies. Keep cross-cutting operational knowledge here;
> keep stack-specific runbooks in the relevant `stacks/*/README.md`.

---

## Cadence at a glance

| Cadence    | Task                                                 | Where it lives                                     |
|------------|------------------------------------------------------|----------------------------------------------------|
| Per reboot | `mount-stores.sh` (manual per-user LUKS unlock; shared volume auto-mounts via keyfile) | `scripts/mount-stores.sh`, `docs/design.md` Build Sequence |
| Weekly     | fail2ban sanity, cert validity > 30 days, free RAM   | this doc § Quick health check                      |
| Weekly     | `docker stats` glance for runaway containers         | this doc § Quick health check                      |
| Monthly    | chnroutes2 refresh (cron) + verify peer .conf drift  | this doc § Monthly chnroutes2 refresh              |
| Monthly    | restic recovery drill (one admin, alternating)       | `docs/backups.md` § Test cadence                   |
| Monthly    | full verification pass (all 7 checks)                | `docs/verification.md`                             |
| Quarterly  | image bump audit (compose tags vs upstream releases) | this doc § Image bump procedure                    |
| Quarterly  | sysctl review against `host/sysctl/99-host.conf`     | `docs/perf-tuning.md` § Network sysctl rationale   |
| Quarterly  | qedge cert symlink verification                      | this doc § Cert renewal verification               |
| Yearly     | DNS-01 token rotation (whichever provider was chosen) | `stacks/ingress/README.md` § 2                    |
| Yearly     | Authelia OIDC key rotation                           | `stacks/authelia/README.md` § Secret rotation      |
| Yearly     | qedge `QEDGE_PASSWORD` rotation (or post-switchover) | `stacks/qedge/README.md` § Rotating the password   |
| As-needed  | kernel update + DKMS rebuild                         | this doc § Kernel updates                          |
| As-needed  | LUKS passphrase rotation                             | this doc § LUKS rotation + recovery                |
| As-needed  | user add / TOTP regen / password rotate              | this doc § Authelia user / TOTP / password rotation |
| As-needed  | qedge switchover (only when `gw0` actively blocked)  | `stacks/qedge/README.md` § Switchover procedure    |

---

## Quick health check

A `scripts/smoke-test.sh` is planned to wrap all of this; until it
exists, the manual one-liners below are the canonical weekly check.
Run from an admin's laptop (most) or via SSH on the VPS (where noted).

```sh
# 1. fail2ban: any sshd jail bans active right now?
ssh <admin>@<your-domain> 'sudo fail2ban-client status sshd'

# 2. cert validity (NotAfter must be > 30 days out)
ssh <admin>@<your-domain> \
    'sudo openssl x509 -noout -dates \
        -in /opt/stacks/ingress/letsencrypt/live/npm-*/fullchain.pem'

# 3. public HTTPS reachable + cert chain valid (off-VPS check)
curl -sI https://cloud.<your-domain> | head -1
curl -sI https://auth.<your-domain>/api/health

# 4. gw0 has at least one peer with a recent handshake
ssh <admin>@<your-domain> 'sudo awg show gw0 | grep "latest handshake"'

# 5. RAM headroom (target: ≤ 2.5 GB used with qedge stopped)
ssh <admin>@<your-domain> 'free -m && \
    sudo docker stats --no-stream --format "table {{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}"'

# 6. all expected containers Up, no restarts
ssh <admin>@<your-domain> 'sudo docker ps --format \
    "table {{.Names}}\t{{.Status}}"'
```

Anything red here goes to § Triage / log locations.

---

## Image bump procedure

Pinned-tag bumps are deliberate, one stack at a time. The repeatable
recipe is the same for every compose-managed stack:

1. **Read the upstream changelog** for the pinned image. Look for
   breaking config changes in particular — IdP-mode directives,
   forward-auth header names, OIDC scope handling.
   - `ingress` (NPM): https://github.com/NginxProxyManager/nginx-proxy-manager/releases
   - `console` (Portainer CE): https://github.com/portainer/portainer/releases
   - `cloud` (copyparty/ac): https://github.com/9001/copyparty/releases
   - `qedge` (Hysteria 2): https://github.com/apernet/hysteria/releases
   - `authelia`: https://github.com/authelia/authelia/releases
2. **Bump the tag** in the laptop repo's `stacks/<name>/docker-compose.yml`
   (single source of truth — never edit the file on the VPS).
3. **Deploy + pull + up.** From the laptop:
   ```sh
   git commit -am 'stacks/<name>: bump to <new-tag>'
   ./scripts/deploy.sh
   ssh <admin>@<your-domain> \
       'cd /opt/stacks/<name> && sudo docker compose pull && sudo docker compose up -d'
   ```
4. **Verify** with a curl probe matching the stack:
   - `cloud`: `curl -sIL https://cloud.<your-domain> | head -20` (expect 302 → Authelia).
   - `ingress`: same probe — NPM is the proxy on the path.
   - `authelia`: `curl -sI https://auth.<your-domain>/api/health` → `200`.
   - `console`: SSH-tunnel to `127.0.0.1:9443`, log in.
   - `qedge` (only if started for testing): see `stacks/qedge/README.md` § Switchover.
   Then re-run the matching `docs/verification.md` check (1 for
   ingress/cloud, 5 for qedge, etc.).
5. **Rollback** if anything misbehaves: revert the tag-bump commit,
   `./scripts/deploy.sh`, `docker compose up -d` again. Persistent state
   (`letsencrypt/`, `data/`, `config/`, `secrets/`) lives outside the
   image and survives between versions, so rollback is graceful.
   Authelia's sqlite under `data/db.sqlite3` is migrated forward by
   Authelia on first run — if a major bump applied a schema migration,
   rollback may require restoring the pre-bump sqlite from a snapshot.
   Take a copy of `stacks/authelia/data/` on the VPS before any
   Authelia bump that crosses a major version.

For the **AmneziaWG kernel package + DKMS module**, see § Kernel updates
below — the procedure is different (kernel ABI considerations, no
container churn).

---

## Cert renewal verification

NPM auto-renews the wildcard cert via OVH DNS-01 ~30 days before expiry
(certbot runs inside the NPM container). Renewals create a new
`letsencrypt/live/npm-N+1/` directory rather than overwriting in place.

The host-side `qedge-cert-watcher.{path,service}` units
(`host/systemd/qedge-cert-watcher.*` → `scripts/cert-renewal-hook.sh`)
fire on any change under `/opt/stacks/ingress/letsencrypt/live/` and
re-point `qedge`'s symlinks at the newest matching `npm-N/` dir. This
runs unattended; verify quarterly that it actually works:

```sh
# 1. qedge symlinks resolve and point at the live NPM cert dir
ssh <admin>@<your-domain> \
    'sudo ls -la /opt/stacks/qedge/tls/ && \
     sudo ls -la /opt/stacks/ingress/letsencrypt/live/'
# fullchain.pem and privkey.pem under qedge/tls/ should symlink into
# the newest npm-N/ dir.

# 2. cert NotAfter > 30 days out
ssh <admin>@<your-domain> \
    'sudo openssl x509 -noout -dates \
         -in /opt/stacks/ingress/letsencrypt/live/npm-*/fullchain.pem'

# 3. watcher journal — should show "no change needed" most of the time,
#    "repointed ..." after each NPM renewal.
ssh <admin>@<your-domain> \
    'sudo journalctl -u qedge-cert-watcher.service --since "90 days ago"'
```

If the symlinks are stale, run the hook by hand and investigate the
path unit:

```sh
sudo /root/scripts/cert-renewal-hook.sh
sudo systemctl status qedge-cert-watcher.path
```

If NPM itself is failing to renew, look at NPM's own logs (`docker logs
ingress`) and verify the OVH application token is still valid (rotate
yearly per `stacks/ingress/README.md` § 2).

---

## Kernel updates

Ubuntu 24.04 LTS GA is kernel 6.8.0-X. The `amneziawg-dkms` module
rebuilds against new kernels via the DKMS apt hook. Kernel upgrades are
the highest-risk routine maintenance on this box because a failed DKMS
build on the new kernel means `gw0` doesn't come back after the reboot.

Procedure:

1. **Inventory.** What kernel are we on, what's the upgrade?
   ```sh
   uname -r
   sudo apt update && sudo apt list --upgradable | grep -E '^linux-(image|headers|generic)'
   ```
2. **Pull updates.** `unattended-upgrades` is configured to apply
   `*-security` only and `Automatic-Reboot "false"` — kernel updates
   land but don't reboot. Apply manually:
   ```sh
   sudo apt full-upgrade
   ```
   The DKMS rebuild for `amneziawg` fires automatically as part of the
   apt hook chain. Watch for the "DKMS: install completed" line.
3. **Verify the build BEFORE rebooting.** The new kernel ABI is what
   the new module is being built against; if the build silently failed,
   reboot will leave you without `gw0`.
   ```sh
   sudo dkms status
   # expect: amneziawg/<ver>: installed (kernel <new-ver>)
   modinfo amneziawg | head -3
   ```
   If `dkms status` shows "WAITING FOR REBOOT", that's expected —
   confirm the new-kernel line says `installed`, not `failed`.
4. **Reboot.**
   ```sh
   sudo reboot
   ```
5. **Post-reboot verification.** After SSH comes back:
   ```sh
   uname -r                                          # confirm new kernel booted
   sudo systemctl status awg-quick@gw0               # active (running)
   sudo awg show gw0                                 # peers listed
   sudo dmesg | grep -i amnezia                      # no errors
   sudo /root/scripts/mount-stores.sh                # re-unlock LUKS volumes
   ```
   From a peer client, run a quick handshake check (`awg show` on the
   peer side, or just open `cloud.<your-domain>` over the
   tunnel). If a peer can't handshake, that's the symptom row in
   § Triage / log locations.
6. **If DKMS failed for the new kernel**: the `gw0` interface won't
   come up. See `docs/perf-tuning.md` § Decision flow for the
   remediation tree (install matching headers → HWE kernel → userspace
   `amneziawg-go` fallback). The host-bootstrap script's reinstall path
   (`scripts/install-gw0.sh`) is also re-runnable safely — it detects
   prebuilt-kmod availability first.

Stack containers come back automatically (`live-restore: true` in
`/etc/docker/daemon.json` and `restart: unless-stopped` on most
services); `qedge` stays stopped (`restart: "no"`) which is intended.
LUKS volumes do **not** auto-mount — `mount-stores.sh` is a manual,
intentional step every reboot (passphrases never on disk).

---

## Monthly chnroutes2 refresh

The regional split lives entirely in client peer configs' `AllowedIPs`
line. Drift in the chnroutes2 CIDR set is small (~weeks) but real.

`scripts/update-route-tables.sh` runs from a `/etc/cron.monthly` hook on
the laptop (not the VPS — the regenerated `peers/` live in the laptop
repo). Force a refresh + re-emit peer configs by hand:

```sh
cd ~/Projects/raph-server-installer
./scripts/update-route-tables.sh --refresh
./scripts/update-route-tables.sh --regenerate-peers
```

The script rewrites the `AllowedIPs` line in every `peers/*.conf` in
place using the canonical marker comment
(`# AllowedIPs managed by update-route-tables.sh`). After a refresh:

- Spot-check that the byte count moved by something plausible. The
  full IPv4 complement is on the order of ~600 KiB; large
  swings (±50%) suggest a bad upstream fetch.
- Reimport the regenerated `.conf` on each affected peer device (or
  scan the QR again from `peers/<name>.qr.png`). The server-side
  `gw0.conf` does not need to change — the server is unaware of the
  client-side split.
- If a peer is connected at the time, the new `AllowedIPs` only takes
  effect after the client reloads the tunnel.

---

## Authelia user / TOTP / password rotation

User lifecycle operations flow through the `enrol` UI at
`https://enrol.<your-domain>/` — TOTP enrolment, password rotation,
user add/remove, LUKS volume create/delete. The CLI invocations below
are the **fallback** path used only when `enrol` is itself unavailable
(rare: the service is down, or the operator needs to recover from a
state the UI cannot represent). See `stacks/enrol/DESIGN.md` for the
canonical UI flow.

### Add user (CLI fallback)

Via UI: Users → Add. The wizard collects username, email, initial
password, optional volume size; on submit the UI provisions everything
atomically.

CLI fallback:

1. Generate the argon2id digest for the user's bootstrap password using
   the recipe in `stacks/authelia/README.md` § 3.
2. Append a user block to `/opt/stacks/authelia/users_database.yml`
   (file watch reloads Authelia within a few seconds — `watch: true` is
   set; if reload misfires, `docker exec authelia kill -HUP 1`).
3. If the user gets a LUKS volume:
   `sudo /root/scripts/create-store-volume.sh <user> 50` then ensure
   the new `<user>.img` is on disk under `/srv/store/data/`. The
   `mount-stores.sh` script auto-discovers all `*.img` files there
   (other than `_shared.img`).
4. The user logs into `https://auth.<your-domain>/` and is prompted for
   TOTP enrolment on first login (Authelia's built-in flow), if your
   policy requires `two_factor`.

### Rotate password (CLI fallback)

Via UI: Users → `<user>` → Change password.

CLI fallback:

```sh
HASH="$(docker run --rm authelia/authelia:4.39.19 \
    authelia crypto hash generate argon2 \
    --variant argon2id --iterations 3 --memory 65536 --parallelism 4 \
    --key-length 32 --salt-length 16 --password '<NEW-PASSWORD>' \
    | awk '/Digest:/ {print $2}')"
sudo sed -i "s|password: '\\\$argon2id\\\$.*'|password: '$HASH'|" \
    /opt/stacks/authelia/users_database.yml
# watch:true picks it up; verify by tailing logs
sudo docker logs --tail 20 authelia
```

(The argon2id parameters above must match the live config in
`stacks/authelia/configuration.yml` so Authelia doesn't trigger a
rehash-on-next-login. See `stacks/enrol/DESIGN.md` § 0.1.)

### Regenerate TOTP (CLI fallback)

Via UI: Users → `<user>` → Reset 2FA.

CLI fallback (Authelia CLI):

```sh
# blow away existing secret + force-regen, gives the operator the
# otpauth:// URI on stdout. Show the URI as a QR via qrencode for the
# user to scan into Aegis / 1Password / etc.
sudo docker exec -it authelia authelia storage user totp generate \
    <user> --force
# render QR locally:
echo '<otpauth-uri-from-above>' | qrencode -t ansiutf8
```

### Delete user (CLI fallback)

Via UI: Users → `<user>` → Delete (confirmation + LUKS-blob
disposition prompt).

CLI fallback:

```sh
# 1. remove from users_database.yml (sed/edit)
# 2. remove TOTP record:
sudo docker exec authelia authelia storage user totp delete <user>
# 3. handle the LUKS blob: backup ${user}.img off-host (restic),
#    then sudo umount /srv/store/mnt/<user> && sudo cryptsetup close
#    store_<user> && sudo rm /srv/store/data/<user>.img
# 4. remove the OS user (optional — only matters for SSH access):
sudo userdel -r <user>
```

---

## LUKS rotation + recovery

Both LUKS-blob lifecycle operations route through `cryptsetup` on the
VPS. The plan keeps passphrases off disk and out of git — they live
only in the operators' offline password managers.

### Passphrase rotation

```sh
# Add a new key slot first, THEN remove the old one. Never the reverse —
# a typo in the new passphrase between those two steps locks you out.
sudo cryptsetup luksAddKey /srv/store/data/<user>.img
# (prompts for an existing passphrase, then the new one, twice)
sudo cryptsetup luksDump /srv/store/data/<user>.img | grep -A1 'Keyslot:'
# verify both slots present, then remove the old one:
sudo cryptsetup luksRemoveKey /srv/store/data/<user>.img
# (prompts for the OLD passphrase you want to retire)
```

Record the rotation date + retire the old passphrase from the offline
note **only after** verifying the new one unlocks (`store-mount@<user>`
restarts cleanly). Keep the **old** passphrase archived offline for as
long as any restic snapshot taken under it is still retained — see
`docs/backups.md` § Operational rules.

### Emergency recovery from a backup snapshot

If the live blob is corrupt / accidentally rm'd / a deploy gone wrong:
restore from the last good restic snapshot. The full procedure (restic
restore → loop attach → cryptsetup open → mount → verify) is in
`docs/backups.md` § Recovery procedure.

Operationally, recovery into the live VPS path looks like:

1. Run the laptop-side recovery procedure from `docs/backups.md` to get
   a verified `.img` mounted at `/mnt/recover` on the laptop.
2. Stop the live mount on the VPS:
   ```sh
   sudo systemctl stop store-mount@<user>.service
   sudo mv /srv/store/data/<user>.img /srv/store/data/<user>.img.broken
   ```
3. Copy the verified `.img` back over SSH (re-using the dedicated
   restic SSH key would NOT work here — that key is forced-command
   read-only; you need an admin key with sudo).
4. Restart the mount:
   ```sh
   sudo systemctl start store-mount@<user>.service
   ```

If the mount works, delete the `.broken` file. If not, archive it for
forensics; the user's passphrase against the live blob is what you'll
need to triage further.

---

## Tailscale (mesh) operations

Tailscale is **not installed by the wizard.** Operators who want an
out-of-band path to the `console` (Portainer) port `:9443` and the
`ingress` (NPM) admin port `:81` install it manually post-setup. The
admin UIs are otherwise reachable only by SSH-tunnel from an admin's
laptop.

### Install (one-time)

On the VPS (as the admin OS user):

```sh
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --ssh --hostname=<neutral-name>
# follow the printed URL; approve in https://login.tailscale.com/admin
```

Pick a hostname that does not advertise the box's role
(no "vpn" / "gateway" / etc.) — it is visible to every other tailnet
node.

### Day-2: keep membership tight

### Add a node (admin's new device)

From the new device:

```sh
# install tailscale (per https://tailscale.com/install)
sudo tailscale up --ssh
# follow the printed URL; approve in https://login.tailscale.com/admin
```

Approve under the same tailnet account that owns the VPS node. Set a
neutral hostname per `scripts/install-mesh.sh` (no
"vpn"/"gateway"/etc. — the tailnet hostname is visible to every other
node).

### Revoke a node

Tailnet admin → Machines → expire / remove the key for the device.
Effective immediately on the next Tailscale heartbeat. If revoking a
device with admin reach to `console`/`ingress`, also rotate Portainer
admin password and any in-flight Authelia sessions
(`stacks/authelia/README.md` § Secret rotation, rotate
`session-secret`).

### Rotate the VPS's mesh identity

```sh
sudo tailscale logout
sudo tailscale up --ssh --hostname=<neutral-name>
# re-approve in tailnet admin
```

This **changes the VPS's tailnet IP** — anything peer-side that has the
old tailnet IP hardcoded breaks. Sweep:

- Both admins' laptops: `tailscale status | grep <vps-name>` — the new
  IP is auto-discovered, but any local SSH config (`~/.ssh/config`
  `Host vps-mesh ...`) needs the IP / magic-DNS name updated.
- Bookmarks pointing at `https://<old-tailnet-ip>:9443` (Portainer) or
  `:81` (NPM admin).
- Anything in `peers/` referencing it (`grep -r '100\.' peers/` — should
  return nothing; tunnel peers don't transit the mesh, but check).

Magic DNS hostnames don't change across re-auth, so prefer those over
raw tailnet IPs everywhere.

---

## ufw + ufw-docker integration (deferred)

**Current state:** `ufw` is **configured but not active** on the VPS
(`scripts/bootstrap-host.sh` stages the rules then explicitly does not
`ufw enable`). The reason is the well-known interaction between vanilla
ufw and Docker: Docker writes its own iptables rules into the `DOCKER`
chain that bypass ufw's `INPUT` chain entirely, so naïve `ufw enable`
on a Docker host is silent — published container ports stay open even
when ufw says they're blocked. The fix is the
[`ufw-docker`](https://github.com/chaifeng/ufw-docker) shim, which
inserts a `DOCKER-USER` chain that ufw rules apply to.

Until ufw-docker is integrated, NAT MASQUERADE for the `gw0` subnet
(10.99.0.0/24) is kept alive by the **interim**
`host/systemd/gw0-nat.service` oneshot — see the unit's header comment
for the explicit retirement instructions.

### When the deferred work is picked up

The ufw-docker integration is its own task; it is **not** done in
passing. A future session should:

1. Read the upstream ufw-docker README and the issue tracker for the
   current Ubuntu 24.04 / Docker-CE compatibility state.
2. Splice the ufw-docker rules into `host/ufw/before.rules.fragment`
   (or a new file under `host/ufw/`) alongside the existing gw0 NAT
   block.
3. Adjust `scripts/bootstrap-host.sh` to install ufw-docker and run
   `ufw enable` as the final step — **but only after** the rule set is
   verified against a known-good list of expected open ports
   (`22/tcp`, `80/tcp`, `443/tcp`, `443/udp`, `51820/udp`).
4. Retire `gw0-nat.service`:
   ```sh
   sudo systemctl disable --now gw0-nat.service
   sudo rm /etc/systemd/system/gw0-nat.service
   sudo systemctl daemon-reload
   ```
   And remove the unit + its rsync entry from `host/systemd/`.
5. Re-run the verification pass from `docs/verification.md` end-to-end —
   especially Check 3 (gw0 throughput) and Check 4 (regional split
   correctness), because a misconfigured firewall here breaks egress
   silently.

Don't pick up the deferred work mid-session unless the user explicitly
asks for it. The current state is functional; the deferred state is
just incrementally more correct.

---

## Retrofitting forward-auth secret on existing deployments

The four production NPM proxy hosts (`auth`/`enrol`/`cloud`/`console`)
must inject `X-Forward-Auth-Secret` on every request — recent `enrol`
revisions fail closed without it (any container on the docker bridge
could otherwise forge `Remote-User` headers and become root). Boxes
provisioned before the secret was introduced still wire those hosts
without the header; pulling the upgrade flips `enrol` to fail-closed and
every protected UI starts returning 401.

One-shot retrofit (idempotent, never deletes existing hosts/certs):

```sh
sudo /opt/raph-server-installer/scripts/retrofit-enrol-forward-auth.sh
```

The script generates the secret if missing, logs into NPM with the
bootstrap admin creds at `/etc/raph-installer/npm-bootstrap.pass` (or
`NPM_EMAIL` / `NPM_PASS` if you've rotated past the bootstrap admin),
and PUTs each of the four hosts with the updated `advanced_config`. Run
it once per upgraded VPS; a second run is a no-op. New installs do this
automatically via the wizard's finalize step.

---

## Triage / log locations

| Symptom                              | First place to look                                                                  |
|--------------------------------------|--------------------------------------------------------------------------------------|
| HTTPS 502 from `cloud`               | `sudo docker logs ingress`, `sudo docker logs cloud`; check `mountpoint -q /srv/store/mnt/<user>` |
| HTTPS 502 from `auth`                | `sudo docker logs authelia`; check Authelia container `healthy`                      |
| SSO redirect loop (cloud / enrol)    | `sudo docker logs authelia`, NPM proxy host Advanced config (forward-auth snippets), browser devtools network panel |
| `console` unreachable                | mesh up? (`tailscale status`); `sudo docker logs console`; loopback bind only        |
| `gw0` peer not handshaking           | `sudo awg show gw0`, `sudo journalctl -u awg-quick@gw0`, `sudo dmesg \| grep -i amnezia` |
| `gw0` interface down post-reboot     | DKMS rebuild failed — see `docs/perf-tuning.md` § Decision flow                       |
| `cloud` shows empty user volume      | `mountpoint -q /srv/store/mnt/<user>` — LUKS unlocked? Run `sudo /root/scripts/mount-stores.sh` |
| copyparty sees stale view post-unlock| bind-mount propagation regression — see `stacks/cloud/README.md` § Bind-mount semantics |
| restic backup failed                 | client-side journal: `journalctl --user -u restic-backup-<user>.service -e`; verify forced-command auth in `~<user>/.ssh/authorized_keys` on VPS |
| `enrol` page 502                     | `sudo docker logs enrol`; post-transformation: also check Authelia `Remote-User` headers and host-side LUKS daemon socket |
| `qedge` won't start                  | `cd /opt/stacks/qedge && sudo docker compose logs`; check TLS symlinks (`§ Cert renewal verification`); `:443/udp` bound? |
| fail2ban not banning                 | `sudo fail2ban-client status sshd`; `sudo journalctl -u fail2ban -e`                 |
| sysctl drift (perf regression)       | `sysctl net.core.rmem_max` etc. vs `host/sysctl/99-host.conf`; redeploy if drift     |
| Disk full                            | `df -h /srv /opt /var`; `sudo du -sh /opt/stacks/*/data 2>/dev/null \| sort -h`; `sudo journalctl --vacuum-time=14d` if /var/log/journal is the offender |
| OOM kills in `dmesg`                 | `sudo dmesg \| grep -i 'killed process'`; `docker stats`; per-container `mem_limit` review (`docs/verification.md` Check 7) |

For deep gateway perf issues see `docs/perf-tuning.md`. For backup
issues see `docs/backups.md`. For DNS issues (cert renewal failures
upstream of NPM) see `docs/dns-records.md`.

---

## Disaster recovery

Outline only — full procedures live in `docs/backups.md` and
`docs/design.md`.

### VPS lost entirely (re-provision from scratch)

1. Provision a fresh Ubuntu 24.04 LTS VPS from any provider that
   supports cloud-init / user-data (LayerStack, Hetzner, Vultr, etc.).
   Note the new public IPv4.
2. Update DNS A records (`docs/dns-records.md` § 1) if the IP
   changed; drop TTL to 300 first per § 3.
3. Re-paste the bootstrap one-liner into the new VPS's user-data box
   with the same `DOMAIN` / `ADMIN_EMAIL`. The bootstrap rebuilds the
   host, brings up enrol in setup mode, and serves the wizard at
   `http://setup.<your-domain>`. Walk through the wizard exactly as on
   first install.
4. **Restore each user's `<user>.img`** from their respective laptop's
   restic repo into `/srv/store/data/` **before** that user's first
   login. Per `docs/backups.md` § Recovery procedure: each admin
   restores their **own** blob from their **own** laptop's repo.
5. Re-import peer `.conf` files from each admin's laptop `peers/` (or
   regenerate via the `enrol` UI's peer-management screen — note that
   regen invalidates all old peer configs because the server's
   AmneziaWG key pair was regenerated by phase 1).
6. Run the full `docs/verification.md` pass before declaring the
   replacement box "in service."

### Single user blob corrupted

```sh
# laptop side — recover into a temp dir using docs/backups.md §
# Recovery procedure (steps 1–6). Verify the test file is present.
# Then per § LUKS rotation + recovery > Emergency recovery, copy the
# verified .img back over to /srv/store/data/<user>.img on the VPS,
# stop/start store-mount@<user>.service.
```

If the blob is recoverable but the filesystem inside is corrupt rather
than missing, mount it read-only first
(`mount -o ro /dev/mapper/store_<user>_recover ...`), copy data into a
fresh blob created by `create-store-volume.sh`, then rotate the user's
LUKS passphrase since the recovered blob's slot is the old one.

---

## What NOT to do

The "out of scope" list in `claude-readme.md` § Out of scope is the
canonical list. Briefly:

- Don't add public DNS for any admin UI (`console`, NPM admin).
- Don't put a CDN / DNS proxy in front of `<your-domain>`.
- Don't run `qedge` as the daily driver — it stays stopped.
- Don't introduce client-side E2EE (Cryptomator etc.) — the threat
  model is encrypted-at-rest only.
- Don't put a backup tool on the VPS — backups are pulled from clients.
- Don't store LUKS or restic passphrases on the VPS or in this repo.
- Don't enable `ufw` without ufw-docker in place — see § ufw + ufw-docker.
- Don't auto-mount LUKS volumes on boot — manual unlock every reboot is
  intentional.

If a maintenance request seems to require violating any of the above,
that's a design-level decision — re-open `docs/design.md` first, don't
quietly flip the switch.
