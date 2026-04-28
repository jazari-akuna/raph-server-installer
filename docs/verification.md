# Verification Runbook

Operator-actionable mirror of `docs/design.md` Verification section. **Canonical sign-off doc** — all 7 checks must be PASS before declaring the box ready.

## Status

| Check | Topic | Status |
|---|---|---|
| 1 | Public HTTPS reachability | _PASS / FAIL / WIP_ |
| 2 | Encrypted volume round-trip | _PASS / FAIL / WIP_ |
| 3 | Gateway throughput (primary path) | _PASS / FAIL / WIP_ |
| 4 | Regional split correctness | _PASS / FAIL / WIP_ |
| 5 | Alternate-ingress drill | _PASS / FAIL / WIP_ |
| 6 | Backup recoverability | _PASS / FAIL / WIP_ |
| 7 | Resource budget sanity | _PASS / FAIL / WIP_ |

Tick all 7 before flipping the box from "bring-up" to "in service." Append a dated row to the **Sign-off log** at the bottom when running through.

---

### Check 1 — Public HTTPS reachability

- **Where to run:** laptop, off-VPS network (not via `mesh`).
- **Commands:**
  ```
  curl -sI https://cloud.<your-domain>
  curl -sI https://console.<your-domain> 2>&1 | head -5
  ```
- **Expected:**
  - `cloud`: `HTTP/2 200`, `Strict-Transport-Security` header present, valid Let's Encrypt cert covering `*.<your-domain>`.
  - `console`: connection refused or DNS NXDOMAIN. Admin UI MUST NOT be public.
- **If this fails:**
  - Re-check NPM proxy host config in `ingress` admin UI (over `mesh`).
  - Re-check DNS A record for `cloud` points to the VPS public IP (`<vps-ip>`).
  - Re-check NPM cert state (DNS-01 token still valid?).
  - **DO NOT** add a public proxy host for `console` to "fix" the second curl — it is supposed to fail.

---

### Check 2 — Cloud (Nextcloud) round-trip

- **Where to run:** laptop browser + (optional) VPS via SSH.
- **Steps:**
  1. Open `https://cloud.<your-domain>/` in a private window. The page returns 200 with Nextcloud's login HTML (no Authelia 302 — Nextcloud owns its session, see ADR-003).
  2. Click "Log in with OpenID Connect" → bounces to Authelia → log in as `<u>` → land in Nextcloud as `<u>`. Admin role is inherited from the `admins` claim if `<u>` is in that group.
  3. Drag a file row onto a folder row → the file moves.
  4. Right-click a file → "Share link" → open the URL in incognito → file downloads.
  5. Talk → New conversation → start a call in two browsers → audio + video work (basic mode is P2P up to ~4 participants).
  6. Upload a 100 MB+ test file → completes (proves the 50 GB upload chain end-to-end).
- **Expected:** every step succeeds; the user's data is visible only to that user (and admins) under `/srv/store/cloud-data/<u>/`.
- **If this fails:**
  ```
  sudo docker logs cloud 2>&1 | tail -50
  sudo docker logs cloud-web 2>&1 | tail -50
  sudo docker exec -u www-data cloud php occ status
  ```
  For OIDC issues: `sudo docker logs authelia 2>&1 | grep -i oidc` and verify the `cloud` client + `redirect_uris` in `stacks/authelia/configuration.yml`.
  For upload-413 issues: confirm `client_max_body_size 50G;` is present in both the nginx-sidecar config and the NPM proxy host advanced_config.

---

### Check 3 — Gateway throughput (primary path)

- **Where to run:** one client outside heavy filtering (sanity ceiling), then a CN-resident client (real target).
- **Commands:**
  ```
  # On VPS (one-shot iperf3 server, exits after first client):
  sudo apt install -y iperf3
  iperf3 -s -1

  # On client (with gw0 active):
  iperf3 -c gw.<your-domain> -t 30
  ```
- **Expected:**
  - Low-filter client: ≥800 Mbps.
  - CN client over CN2-GIA: ≥50 Mbps sustained.
- **If this fails:**
  - Sanity-check peer config: `Endpoint`, `PublicKey`, `PresharedKey`, AmneziaWG params (`Jc/Jmin/Jmax/S1/S2/H1/H2/H3/H4`).
  - On VPS: `sudo awg show gw0` → verify recent handshake from the peer's IP.
  - Apply / re-verify sysctl tuning per `docs/perf-tuning.md` (`rmem_max`, `wmem_max`, `tcp_mtu_probing`, `bbr` + `fq`).
  - If kernel module not loaded: see `docs/perf-tuning.md` decision flow (DKMS → prebuilt kmod → HWE → userspace fallback).

---

### Check 4 — Regional split correctness (the important one)

- **Where to run:** client connected via `gw0`, physically in mainland China.
- **Commands:**
  ```
  curl -s ifconfig.me
  mtr -rwc 10 baidu.com
  mtr -rwc 10 github.com
  ```
- **Expected:**
  - `ifconfig.me` → `<vps-ip>` (foreign traffic exits VPS).
  - `mtr baidu.com` → first hop is the **local ISP gateway**, NOT the VPS.
  - `mtr github.com` → first hop is the **VPS**.
- **If this fails:**
  - `AllowedIPs` in the peer `.conf` is stale. On laptop:
    ```
    ./scripts/update-route-tables.sh --refresh
    ./scripts/update-route-tables.sh --regenerate-peers
    ```
  - Verify chnroutes2 cache is fresh (mtime within last ~30 days).
  - Re-import the regenerated peer `.conf` on the client.

---

### Check 5 — Alternate-ingress drill

- **Where to run:** VPS (admin via SSH) + client with sing-box profile pre-installed.
- **Steps:**
  1. Stop primary gateway on VPS:
     ```
     sudo systemctl stop awg-quick@gw0
     ```
  2. Bring up `qedge` (NPM cert symlinks must be in place — see `stacks/qedge/README.md`):
     ```
     cd /opt/stacks/qedge && sudo docker compose up -d
     ```
  3. Switch client to the sing-box `qedge` profile.
  4. Re-run **Check 3** and **Check 4** through the alternate path.
- **Expected:** all checks pass; throughput delta vs `gw0` is **<20%**.
- **Cleanup:**
  ```
  cd /opt/stacks/qedge && sudo docker compose down
  sudo systemctl start awg-quick@gw0
  ```
- **If this fails:** check `qedge` cert symlinks (wildcard cert from `ingress` shared into the `qedge` container), `cdn.<your-domain>` DNS, `:443/udp` open in `ufw`.

---

### Check 6 — Backup recoverability

- **Where to run:** laptop.
- **Steps:** Run the full Cloud (Nextcloud) backup recipe in `docs/backups.md` against a recent snapshot, then restore into a throwaway VM (or fresh-bootstrapped sibling VPS) following the restore section of the same doc.
- **Expected:** the restored Nextcloud serves at the throwaway box's URL with the snapshot-era file tree visible to the snapshot-era users.
- **If this fails:** see `docs/backups.md` § Test cadence and § Operational rules — most issues are a `pg_dump` ordering problem (snapshot the DB first, then mirror the files).

---

### Check 7 — Resource budget sanity

- **Where to run:** VPS (or from laptop via SSH).
- **Commands:**
  ```
  ssh <admin>@<your-domain> '
    free -m
    sudo docker stats --no-stream --format "table {{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}"
  '
  ```
- **Expected:** with `ingress` + `console` + `cloud` + `gw0` + `mesh` running and `qedge` stopped, total used RAM ≤ **2.5 GB**, leaving ~1.5 GB headroom.
- **If this fails:**
  - Identify the offender from `docker stats`.
  - Drop unnecessary user-deployed stacks.
  - Tighten per-container `mem_limit` via `console` (Portainer). Note: current `2g` per-service caps are **caps, not guarantees**.
  - If still tight after cleanup, the answer is a VPS upgrade — do not tune around RAM-heavy stacks (databases, AI inference) on a 4 GB box.

---

## Sign-off log

Append one row per full pass. Format:

```
| Date       | Operator | C1 | C2 | C3 | C4 | C5 | C6 | C7 | Notes |
|------------|----------|----|----|----|----|----|----|----|-------|
| YYYY-MM-DD | <admin>  |    |    |    |    |    |    |    |       |
```

A run with any FAIL is not a sign-off; fix and re-run the affected checks (and any downstream).
