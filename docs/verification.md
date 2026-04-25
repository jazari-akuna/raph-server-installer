# Verification Runbook

Operator-actionable mirror of `docs/plan.md` Verification section. **Canonical sign-off doc** — all 7 checks must be PASS before declaring the box ready.

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
  curl -sI https://cloud.antarctica-engineering.com
  curl -sI https://console.antarctica-engineering.com 2>&1 | head -5
  ```
- **Expected:**
  - `cloud`: `HTTP/2 200`, `Strict-Transport-Security` header present, valid Let's Encrypt cert covering `*.antarctica-engineering.com`.
  - `console`: connection refused or DNS NXDOMAIN. Admin UI MUST NOT be public.
- **If this fails:**
  - Re-check NPM proxy host config in `ingress` admin UI (over `mesh`).
  - Re-check OVH DNS A record for `cloud` points to `43.228.124.145`.
  - Re-check NPM cert state (DNS-01 token still valid?).
  - **DO NOT** add a public proxy host for `console` to "fix" the second curl — it is supposed to fail.

---

### Check 2 — Encrypted volume round-trip

- **Where to run:** VPS (sagan via SSH) + laptop browser.
- **Steps:**
  1. Unlock store (sagan, on VPS):
     ```
     sudo systemctl start store-mount@sagan.service
     ```
     Operator types passphrase at the `systemd-ask-password` prompt.
  2. From laptop browser, upload `test.bin` via `https://cloud.antarctica-engineering.com` into sagan's volume.
  3. On VPS, lock the volume:
     ```
     sudo umount /srv/store/mnt/sagan
     sudo cryptsetup close store_sagan
     ```
  4. Reload `cloud` in browser → sagan's volume appears empty / inaccessible (fail-closed).
  5. Re-run step 1 → `test.bin` reappears.
- **Expected:** file is visible only when the LUKS volume is unlocked; locked volume = empty directory served by copyparty.
- **If this fails:**
  ```
  sudo journalctl -u store-mount@sagan -n 50
  sudo dmesg | grep -i luks
  sudo docker logs cloud
  ```
  Confirm bind-mount path `/srv/store/mnt/sagan` matches the compose volume mapping in `stacks/cloud/docker-compose.yml`.

---

### Check 3 — Gateway throughput (primary path)

- **Where to run:** one client outside heavy filtering (sanity ceiling), then a CN-resident client (real target).
- **Commands:**
  ```
  # On VPS (one-shot iperf3 server, exits after first client):
  sudo apt install -y iperf3
  iperf3 -s -1

  # On client (with gw0 active):
  iperf3 -c gw.antarctica-engineering.com -t 30
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
  - `ifconfig.me` → `43.228.124.145` (foreign traffic exits VPS).
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

- **Where to run:** VPS (sagan or marcus via SSH) + client with sing-box profile pre-installed.
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
- **If this fails:** check `qedge` cert symlinks (wildcard cert from `ingress` shared into the `qedge` container), `cdn.antarctica-engineering.com` DNS, `:443/udp` open in `ufw`.

---

### Check 6 — Backup recoverability

- **Where to run:** laptop.
- **Commands:**
  ```
  restic snapshots -r ~/.local/share/restic-rarcus/sagan
  restic restore latest --target /tmp/recover -r ~/.local/share/restic-rarcus/sagan
  ```
  Then mount-and-verify per `docs/backups.md` recovery procedure: attach restored `sagan.img` as loop device, `cryptsetup open` with the passphrase, mount, confirm the `test.bin` from Check 2 is present.
- **Expected:** recent snapshot exists; restore completes; recovered `.img` unlocks and contains expected files.
- **If this fails:** see `docs/backups.md` (repo password, SSH key, sftp endpoint, cron / systemd-timer health on laptop).

---

### Check 7 — Resource budget sanity

- **Where to run:** VPS (or from laptop via SSH).
- **Commands:**
  ```
  ssh sagan@antarctica-engineering.com '
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
| YYYY-MM-DD | sagan    |    |    |    |    |    |    |    |       |
| YYYY-MM-DD | marcus   |    |    |    |    |    |    |    |       |
```

A run with any FAIL is not a sign-off; fix and re-run the affected checks (and any downstream).
