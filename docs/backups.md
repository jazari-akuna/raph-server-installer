# Backups — operator runbook

Operational runbook for the bind-mount + `pg_dump` backup workflow.
Implements ADR-001 (`docs/architecture-decisions.md`): every stack's
persistent state lives on `/srv/store/<stack>-*` host bind-mounts, so
backup is a single `rsync` per box plus a `pg_dump` per Postgres-backed
stack.

The hard rules from the plan, restated up front:

- **Pull, not push.** The backup tool runs on the **admin's laptop**.
  The VPS holds **no** backup credentials.
- **Per-admin repos.** Each admin maintains their own restic
  repository. The repo password lives on the laptop only — never on
  the VPS, never in this repo, never in `peers/`.
- **`pg_dump` first, rsync second.** A live Postgres data dir is
  fragile to copy raw; always dump the database to SQL first and rsync
  the dump alongside the bind-mount tree. See § Cloud (Nextcloud)
  below.

## Architecture

```
        Admin laptop                         VPS (<your-domain>)
   ┌────────────────────────┐           ┌────────────────────────────────┐
   │ ~/snapshots/<box>/     │           │ /srv/store/cloud-data/         │
   │   cloud-data/          │  rsync    │ /srv/store/cloud-config/       │
   │   cloud-config/        │ ◀────ssh──│ /srv/store/cloud-apps/         │
   │   cloud-apps/          │           │ (plus a pg_dump streamed via   │
   │   <date>-cloud-db.sql  │           │  `docker exec cloud-db pg_dump`)│
   │                        │           │                                │
   │ (optional) restic repo │           │ /home/<admin>/.ssh/            │
   │ wraps the snapshot dir │           │   authorized_keys              │
   │ for de-dup + history   │           │   (forced-command lock)        │
   │                        │           │                                │
   │ ~/.ssh/id_restic_<u>   │           │ NO restic, NO repo password.   │
   └────────────────────────┘           └────────────────────────────────┘
```

The dataflow per backup run:

1. **`pg_dump` over SSH** streams a SQL dump of every Postgres-backed
   stack's database into a timestamped file in the local snapshot dir.
2. **`rsync` over SSH** mirrors each `/srv/store/<stack>-*` bind-mount
   into the corresponding subdir of the local snapshot dir.
3. **(optional)** `restic backup` wraps the snapshot dir for content-
   addressed dedup + retention. Skip this if you're happy with a flat
   `<date>-snapshot/` dir tree and your laptop's own backup tool.

`rsync -aAXH --delete` preserves ACLs and xattrs (Nextcloud relies on
neither, but Postgres-data inside `cloud-db/` does — bring it for
correctness).

## Cloud (Nextcloud)

Recipe for a single backup run. Substitute `vps` with your SSH alias
for the VPS and `snapshots/` with wherever you stage backups locally.

### Backup

```sh
mkdir -p snapshots
date_tag=$(date -u +%F)

# 1. Pre-backup: pg_dump streams the Nextcloud database to a local SQL
#    file. Use --clean so the restore drops + recreates objects.
ssh vps "sudo docker exec cloud-db pg_dump -U nextcloud --clean nextcloud" \
    > "snapshots/${date_tag}-cloud-db.sql"

# 2. Mirror the persistent bind-mounts. We do NOT rsync /srv/store/cloud-db
#    raw — the pg_dump above is the authoritative DB snapshot, and
#    raw-copying a live PG data dir is the standard "your backup is
#    inconsistent, surprise!" trap.
sudo rsync -aAXH --delete \
    vps:/srv/store/cloud-data/   snapshots/cloud-data/
sudo rsync -aAXH --delete \
    vps:/srv/store/cloud-config/ snapshots/cloud-config/
sudo rsync -aAXH --delete \
    vps:/srv/store/cloud-apps/   snapshots/cloud-apps/
```

### Restore

Restore order matters: database first, then file trees, then bring the
stack up so Nextcloud sees a consistent world from boot.

```sh
# On the (possibly fresh) VPS, after bootstrap.sh has brought cloud-db up:

# 1. Restore the database. --clean dump above means objects are dropped
#    and recreated; the target db has to exist.
cat "snapshots/${date_tag}-cloud-db.sql" \
    | ssh vps "sudo docker exec -i cloud-db psql -U nextcloud nextcloud"

# 2. Stop the Nextcloud containers (DB stays up — psql restore already done):
ssh vps "sudo docker compose -f /opt/stacks/cloud/docker-compose.yml stop cloud cloud-web"

# 3. Mirror the bind-mounts back. Same flag set as backup; rsync is
#    idempotent so repeating is safe.
sudo rsync -aAXH --delete \
    snapshots/cloud-data/   vps:/srv/store/cloud-data/
sudo rsync -aAXH --delete \
    snapshots/cloud-config/ vps:/srv/store/cloud-config/
sudo rsync -aAXH --delete \
    snapshots/cloud-apps/   vps:/srv/store/cloud-apps/

# 4. Bring the stack back up:
ssh vps "sudo docker compose -f /opt/stacks/cloud/docker-compose.yml up -d"

# 5. Sanity-check from the laptop:
curl -sk https://cloud.<your-domain>/ | grep -qi nextcloud && echo OK
```

If users have logged in between the snapshot and the restore, those
post-snapshot files (and DB rows) are gone — there is no merge. For
more granular recovery use Nextcloud's per-user Trash + Versions apps;
the snapshot is the disaster-recovery floor, not the per-file undo.

### Wrapping with restic (optional, recommended)

Restic on top of the snapshot dir gives you content-addressed dedup
across daily snapshots and a retention policy. The dataflow is the
same as for any "directory of files" backup target:

```sh
restic init -r ~/.local/share/restic-raph/<box>
restic -r ~/.local/share/restic-raph/<box> backup snapshots/
restic -r ~/.local/share/restic-raph/<box> forget --prune \
    --keep-daily 14 --keep-weekly 8 --keep-monthly 12
```

Pin the repo password into a 0600 file (`~/.config/restic-raph/<box>.passwd`)
or your laptop's keyring; the restic snapshot of `snapshots/` is
unrecoverable without it.

## One-time setup per admin laptop

Do this once on each admin's laptop, replacing `<admin>` with the
admin's VPS SSH login and `<vps-host>` with the connection target.

### 1. Install restic (only if you want the optional wrap step)

```sh
# Debian / Ubuntu
sudo apt install restic
# macOS
brew install restic
# verify
restic version    # 0.16+ recommended
```

### 2. Generate a dedicated SSH keypair for the backup pull

This key is **separate** from the admin's normal login key. It exists
solely to authorize the `pg_dump` + `rsync` pull and is locked down
server-side via a forced-command + `restrict` directive. The exact
forced-command string depends on which subset of the dataflow you want
this key to authorize (just `rsync`, just `docker exec cloud-db pg_dump`,
or the union of both); the simplest setup is one key per dataflow path.

```sh
ssh-keygen -t ed25519 \
    -f ~/.ssh/id_backup_<admin> \
    -C "backup-<admin>"
chmod 0600 ~/.ssh/id_backup_<admin>
chmod 0644 ~/.ssh/id_backup_<admin>.pub
```

Set a passphrase on the key only if your laptop has an SSH agent
running for the timer service to talk to; otherwise leave it empty
(the file is laptop-local and the laptop's full-disk encryption is
the real boundary).

### 3. Authorize the key server-side

Append the public key to the admin's `~/.ssh/authorized_keys` on the
VPS with a forced command and the `restrict` option set, so this key
can **only** run the specific commands the backup script issues.

For the simplest case (one key, allow the union of `pg_dump` + the
three rsync paths), wrap a small server-side script that branches on
`$SSH_ORIGINAL_COMMAND`:

```
command="/usr/local/sbin/backup-shim",restrict ssh-ed25519 AAAA…<paste pubkey here>… backup-<admin>
```

Where `/usr/local/sbin/backup-shim` (`mode 0755`, root-owned) inspects
`$SSH_ORIGINAL_COMMAND` and only invokes the whitelisted command set.
Keep the shim tight — anything beyond `sudo docker exec cloud-db pg_dump …`
and `rsync --server --sender …` to the four bind-mount paths is a
privilege-escalation surface.

References:

- restic docs: <https://restic.readthedocs.io/en/stable/040_backup.html>
- `pg_dump` docs: <https://www.postgresql.org/docs/16/app-pgdump.html>
- sshd authorized_keys directives: <https://man.openbsd.org/sshd.8#AUTHORIZED_KEYS_FILE_FORMAT>

## Test cadence

Once a month, on the first weekend, **one** admin runs the full restore
procedure end-to-end into a throwaway VM (or a fresh-bootstrapped
sibling VPS). Log the outcome (date, snapshot date, time-to-restore,
any anomalies). Alternate which admin runs the drill so both sets of
laptops/repos get exercised.

A drill is "passed" if and only if:

1. The restored Nextcloud serves at `cloud.<your-domain>/` (or the
   throwaway box's equivalent) returning 200 with the login HTML.
2. A login as one of the snapshot-era users lands in their dashboard
   with the snapshot's file tree visible.
3. The most recent file from the snapshot opens / downloads cleanly.

Anything else — schema mismatch after `psql` restore, missing files,
"Nextcloud is in maintenance mode" loop after `docker compose up` — is
a backup failure that has to be triaged before the next snapshot is
taken.

## Operational rules (recap)

- **Repo password (if you wrap with restic) and dedicated SSH key live
  on the laptop ONLY.** Never copy either to the VPS, to this repo, or
  to a shared cloud drive.
- **The dedicated backup SSH key is single-purpose** — only authorizes
  the `pg_dump` + rsync paths via the server-side shim. Don't reuse
  for shell login.
- **`pg_dump` BEFORE the rsync of `cloud-data/`, not after.** A
  large `cloud-data/` rsync takes minutes; if a user uploads during
  that window, the SQL dump is older than the file tree and a restore
  shows orphan rows. The rule is "snapshot the DB first, then mirror
  the files; on restore, restore the DB first, then the files."
- **Don't rsync `/srv/store/cloud-db/` raw.** `pg_dump` is the
  authoritative DB snapshot; the data dir is a moving target while
  Postgres is up. Use the file-tree rsync only for `cloud-data`,
  `cloud-config`, `cloud-apps`.
- **Per-user trash + versions are the per-file undo.** Snapshot
  restore is the disaster-recovery floor. Don't burn snapshots on
  "user deleted a file" — point them at Files → Deleted files in
  Nextcloud's UI first.

## Files referenced

- `docs/architecture-decisions.md` ADR-001 — why every stack's state
  lives on `/srv/store/<stack>-*` bind-mounts.
- `stacks/cloud/README.md` — Nextcloud stack details (image tag,
  bind-mount layout, `occ` usage).
- `docs/maintenance.md` § Cloud (Nextcloud) maintenance — runbook for
  `occ upgrade`, redis-lock recovery, db-connect troubleshooting.
