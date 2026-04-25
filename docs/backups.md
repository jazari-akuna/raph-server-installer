# Backups — operator runbook

Operational runbook for the encrypted-blob backup workflow. Implements
build-sequence step 12 (`docs/plan.md`) and is exercised by verification
check 6.

The hard rules from the plan, restated up front:

- **Pull, not push.** The backup tool runs on the **admin's laptop**.
  The VPS holds **no** backup credentials and **no** restic binary.
- **Per-admin repos.** sagan and marcus each maintain their own restic
  repository. They back up only their **own** `${user}.img`. There is
  no shared repo and no cross-admin access.
- **The repo password lives on the laptop only.** Never on the VPS,
  never in this repo, never in `peers/`.
- **Restic only ever sees ciphertext.** The `.img` is an opaque LUKS2
  blob; restic doesn't know or care that it's encrypted, and cannot
  decrypt it without the LUKS passphrase (which it never has access
  to). Compromising the restic repo gives an attacker only ciphertext.

## Architecture

```
        Admin laptop                         VPS (antarctica-engineering.com)
   ┌────────────────────────┐           ┌────────────────────────────────┐
   │ ~/.cache/restic-rarcus/│           │ /srv/store/data/<user>.img      │
   │   <user>/<user>.img    │  rsync    │   (LUKS2 sparse blob, opaque)   │
   │     (staging mirror)   │ ◀────ssh──│                                 │
   │                        │           │ /home/<user>/.ssh/             │
   │ ~/.local/share/        │           │   authorized_keys              │
   │   restic-rarcus/<user>/│           │   (forced-command lock)         │
   │     (restic repo)      │           │                                 │
   │                        │           │ NO restic, NO repo password.    │
   │ ~/.ssh/id_restic_<user>│           └────────────────────────────────┘
   │ ~/.config/restic-      │
   │   rarcus/<user>.passwd │
   └────────────────────────┘
```

The dataflow is two stages:

1. **rsync over SSH** pulls `/srv/store/data/<user>.img` from the VPS
   into a local staging directory on the laptop. This is the only
   network step; it uses a dedicated, restricted SSH key.
2. **`restic backup`** runs **locally** against the staged file and
   commits a snapshot to the local repo. Restic dedups against prior
   snapshots, so steady-state wire and disk cost on the laptop scales
   with the **changed** ciphertext bytes, not the full blob size.

### Why rsync-stage-then-restic, not `restic backup sftp:…`

`restic backup` takes a **path argument**, not a URL. The `sftp:` URL
form is supported only as the **repository** argument (`-r`/
`--repo`). The two relevant operating modes:

- **(a) Repo on laptop, source on VPS pulled via sftp.** This would be
  the natural fit, but `restic backup sftp:vps:/srv/store/data/<user>.img`
  is **not valid syntax** — restic cannot back up a remote path
  directly. The source has to exist as a path on the host running
  `restic backup`.
- **(b) Repo on VPS via sftp, source on laptop.** Would work, but
  inverts the trust model: the repo (and its metadata, including
  filenames and snapshot tree) lives on the VPS, and the VPS would
  need write access to it. Not what we want — the plan keeps backup
  state laptop-side.

The workable approach is therefore: stage the `.img` to the laptop
first (rsync, scp, or sshfs), then run `restic backup` on the staged
copy. We use **rsync** because it's incremental at the byte level (not
just whole-file), so subsequent runs only pull the changed extents of
the blob even though the file as a whole is multi-GB. Restic on the
local copy then dedups across snapshots within the repo.

`sshfs` would let restic read the remote file directly, but every
`restic backup` rescan re-reads the entire file over the network,
defeating the point. `rsync --inplace` is the fast path.

## One-time setup per admin laptop

Do this once on each admin's laptop, replacing `<user>` with `sagan`
or `marcus` and `<vps-host>` with the connection target.

### 1. Install restic

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
solely to authorize the rsync pull and is locked down server-side.

```sh
ssh-keygen -t ed25519 \
    -f ~/.ssh/id_restic_<user> \
    -C "restic-backup-<user>"
chmod 0600 ~/.ssh/id_restic_<user>
chmod 0644 ~/.ssh/id_restic_<user>.pub
```

Set a passphrase on the key only if your laptop has an SSH agent
running for the timer service to talk to; otherwise leave it empty
(the file is laptop-local and the laptop's full-disk encryption is
the real boundary).

### 3. Authorize the key server-side, with a forced-command restriction

On the VPS, append the public key to `/home/<user>/.ssh/authorized_keys`
with a forced command and the `restrict` option set, so this key can
**only** read the one `.img` file via rsync. Anything else — interactive
shell, port-forward, agent-forward, X11, sftp browse, scp of arbitrary
paths — is denied.

The line to append (single line; the `command=` value is the exact
rsync server invocation that the laptop's `rsync` will issue):

```
command="rsync --server --sender -e.LsfxC . /srv/store/data/<user>.img",restrict ssh-ed25519 AAAA…<paste pubkey here>… restic-backup-<user>
```

What that does:

- `command="…"` — when this key authenticates, sshd runs **only** the
  given command and ignores whatever the client requested. The
  `rsync --server --sender …` form is what the rsync-over-ssh transport
  actually executes on the far side; the leading `--server --sender`
  declares this end as read-only-source. The `-e.LsfxC` flag set
  matches what `rsync -e "ssh ..."` from the laptop will negotiate
  for our use; if the client invocation changes, this string has to
  change with it (run `rsync -vv ... 2>&1 | grep "Server command"`
  during a test pull to extract the exact string your rsync version
  produces — the canonical text varies slightly between rsync 3.1,
  3.2, and 3.3).
- `restrict` — the modern shorthand that disables all of:
  `agent-forwarding`, `port-forwarding`, `pty`, `user-rc`, and
  `X11-forwarding` in one option. It's also forward-compatible: any
  future restriction OpenSSH adds is enabled-by-default under
  `restrict`. See `sshd(8)`'s
  `AUTHORIZED_KEYS FILE FORMAT` section.

References:

- restic docs: <https://restic.readthedocs.io/en/stable/040_backup.html>
- sshd authorized_keys directives: <https://man.openbsd.org/sshd.8#AUTHORIZED_KEYS_FILE_FORMAT>

Verify the lockdown after install:

```sh
# from the laptop, should succeed:
rsync -e "ssh -i ~/.ssh/id_restic_<user>" \
      <user>@<vps-host>:/srv/store/data/<user>.img /tmp/probe.img

# from the laptop, should each FAIL with "command forbidden" or similar:
ssh -i ~/.ssh/id_restic_<user> <user>@<vps-host>            # no shell
ssh -i ~/.ssh/id_restic_<user> <user>@<vps-host> ls /       # no exec
scp -i ~/.ssh/id_restic_<user> /etc/hosts <user>@<vps-host>:/tmp/  # no scp
```

Delete `/tmp/probe.img` after the test.

### 4. Initialize the restic repo

Pick a strong, unique password for the repo. Either store it in your
laptop's keyring (macOS Keychain, GNOME Keyring, KeePassXC, etc.) and
have the timer service wrap it with `restic --password-command=…`, or
store it in a 0600 file:

```sh
mkdir -p ~/.config/restic-rarcus
umask 077
# either prompt for it now and save, or paste it in via your editor:
read -rsp "restic password for <user>: " p && echo "$p" > ~/.config/restic-rarcus/<user>.passwd
unset p
chmod 0600 ~/.config/restic-rarcus/<user>.passwd

mkdir -p ~/.local/share/restic-rarcus
restic init \
    -r ~/.local/share/restic-rarcus/<user> \
    --password-file ~/.config/restic-rarcus/<user>.passwd
```

If the laptop is lost, the repo is unrecoverable without that password.
Treat it like a master key. Cross-store it in the same place the LUKS
passphrase is recorded (offline password manager or paper).

### 5. Drop the templates into place

Copy and substitute placeholders:

```sh
mkdir -p ~/.local/bin ~/.config/systemd/user

# script
sed "s/<USER>/<user>/g; s/<VPS_HOST>/<vps-host>/g" \
    /path/to/repo/scripts/templates/restic-backup.sh \
    > ~/.local/bin/restic-backup-<user>.sh
chmod 0755 ~/.local/bin/restic-backup-<user>.sh

# systemd user units
sed "s/<USER>/<user>/g" \
    /path/to/repo/scripts/templates/restic-backup.service \
    > ~/.config/systemd/user/restic-backup-<user>.service
sed "s/<USER>/<user>/g" \
    /path/to/repo/scripts/templates/restic-backup.timer \
    > ~/.config/systemd/user/restic-backup-<user>.timer

systemctl --user daemon-reload
systemctl --user enable --now restic-backup-<user>.timer
```

On macOS, use `launchd` plists or a wrapper around `cron` instead;
the `.sh` script is portable, the systemd units are Linux-laptop only.

## Daily backup procedure

Two paths, depending on whether the timer is wired up.

### Automatic (timer-driven)

After the user-unit setup above, the timer fires daily near 03:00
laptop-local with up to 30 minutes of randomized delay. To inspect:

```sh
systemctl --user status  restic-backup-<user>.timer
systemctl --user list-timers --all | grep restic
journalctl --user -u restic-backup-<user>.service -e
```

To run an out-of-cycle backup:

```sh
systemctl --user start restic-backup-<user>.service
```

### Manual

```sh
~/.local/bin/restic-backup-<user>.sh
```

The script (see `scripts/templates/restic-backup.sh`):

1. `rsync`'s `/srv/store/data/<user>.img` from the VPS to
   `~/.cache/restic-rarcus/<user>/<user>.img` over the dedicated SSH
   key.
2. Runs `restic backup` against the staged file with the `daily` tag.
3. Runs `restic forget --prune` with the configured retention policy
   (default: 14 daily, 8 weekly, 12 monthly).

## Recovery procedure

This is the procedure for both monthly drills (verification check 6)
and real disasters. Run on a Linux box with `cryptsetup` and a free
loop device — the admin's laptop is fine; a clean throwaway VM is
better for drills because it forces you to verify all the steps work
from scratch.

```sh
# 1. inspect snapshots
restic snapshots \
    -r ~/.local/share/restic-rarcus/<user> \
    --password-file ~/.config/restic-rarcus/<user>.passwd

# 2. restore the latest snapshot (writes the .img tree under /tmp/recover)
mkdir -p /tmp/recover
restic restore latest \
    -r ~/.local/share/restic-rarcus/<user> \
    --password-file ~/.config/restic-rarcus/<user>.passwd \
    --target /tmp/recover

# locate the restored .img — restic preserves the source path:
img=$(find /tmp/recover -name "<user>.img" -type f -print -quit)
echo "restored image: $img"

# 3. attach the .img as a loop device (-fP returns first free, scans parts)
loopdev=$(sudo losetup -fP --show "$img")
echo "loop device: $loopdev"

# 4. unlock the LUKS volume — passphrase prompt
sudo cryptsetup open --type luks2 "$loopdev" store_<user>_recover

# 5. mount it
sudo mkdir -p /mnt/recover
sudo mount /dev/mapper/store_<user>_recover /mnt/recover

# 6. verify a known file is present (use whatever marker you placed
#    during verification check 2 — e.g. a `test.bin` you uploaded
#    via cloud)
ls -la /mnt/recover/
sha256sum /mnt/recover/<known-file>

# 7. cleanup
sudo umount /mnt/recover
sudo cryptsetup close store_<user>_recover
sudo losetup -d "$loopdev"
sudo rm -rf /tmp/recover
```

If step 4 prompts but rejects the passphrase, you've recovered the
wrong blob (or somebody changed the LUKS passphrase since the snapshot
was taken). The LUKS header is part of the blob; the passphrase is
**not**. Use the passphrase from the time the snapshot was created.

## Test cadence

Once a month, on the first weekend, **one** admin runs the full
recovery procedure end-to-end against their own latest snapshot. Log
the outcome (date, snapshot ID, time-to-mount, any anomalies) in a
local note. Alternate which admin runs the drill so both sets of
laptops/repos get exercised.

A drill is "passed" if and only if step 6 produced the exact known-good
file with the expected hash. Anything else — hash mismatch, missing
file, mount failure, restore complete-but-blob-corrupt — is a backup
failure that has to be triaged before the next snapshot is taken.

## Operational rules (recap)

- **Repo password and dedicated SSH key live on the laptop ONLY.**
  Never copy either to the VPS, to this repo, or to a shared cloud
  drive.
- **The dedicated backup SSH key is single-purpose.** It does not
  authorize anything beyond reading the one `.img` file. Do not reuse
  it for ssh login, peer provisioning, or anything else; if you need
  shell, use your normal admin key.
- **The `.img` is opaque ciphertext.** Restic deduplicates well within
  a single user's history (steady-state delta is small), but
  cross-user dedup is essentially worthless: each user's blob has its
  own LUKS master key, so the byte streams are uncorrelated. There is
  no benefit to sharing a repo between sagan and marcus, and there
  are real downsides (mutual access to each other's snapshot
  metadata).
- **Backups continue to work whether or not `mount-stores.sh` has
  been run.** The `.img` file exists on disk regardless of mount
  state — it's a plain sparse file. Mount state affects only the
  decrypted view at `/srv/store/mnt/<user>`, which is **not** what
  restic backs up.
- **Do not back up `/srv/store/mnt/<user>`.** Plaintext tree, would
  defeat the entire scheme.
- **If you change the LUKS passphrase**, every prior snapshot becomes
  unrecoverable without the **old** passphrase. Keep a record of
  passphrase rotations (date + old passphrase, archived offline) for
  long enough that any retained snapshot is still recoverable.

## Files referenced

- `scripts/templates/restic-backup.sh` — the laptop-side backup
  script. Substitute `<USER>` and `<VPS_HOST>`.
- `scripts/templates/restic-backup.service` — systemd user unit that
  runs the script.
- `scripts/templates/restic-backup.timer` — systemd user unit that
  schedules the service daily.
