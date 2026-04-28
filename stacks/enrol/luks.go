// luks.go — host-side LUKS lifecycle operations.
//
// All functions assume the container is running PRIVILEGED with
// /srv/store bind-mounted rshared and the kernel module loaded on
// the host (always true on the VPS). Failures are surfaced to the
// caller; the caller writes the audit entry.
//
// Parameter set matches scripts/create-store-volume.sh:
//   luksFormat: type=luks2, cipher=aes-xts-plain64, key-size=512,
//               hash=sha512, pbkdf=argon2id, --use-urandom
//   filesystem: ext4 with label store_<u>
//   mountpoint: /srv/store/mnt/<u>, mode 0700, owned by host user.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// VolumeInfo is rendered into the user-detail page.
type VolumeInfo struct {
	User       string
	ImagePath  string
	Mountpoint string
	Mapper     string
	Exists     bool
	Mounted    bool
	SizeBytes  int64
}

// StorageInfo aggregates host-disk + per-user volume metrics for the
// /users overview panel.
type StorageInfo struct {
	Root          string // filesystem root we Statfs'd (e.g. /srv/store)
	TotalBytes    int64  // total capacity of the underlying filesystem
	FreeBytes     int64  // bytes free to non-root
	UserOnDisk    int64  // sum of per-user .img on-disk allocation
	UserUsedInner int64  // sum of bytes actually used inside unlocked volumes
	NominalBytes  int64  // sum of per-user nominal allocation (luksSizeGB each)
	SystemBytes   int64  // total - free - userOnDisk (everything else)
	Users         []UserStorage
	Shared        SharedStorage // shared volume (/srv/store/mnt/_shared)
}

// SharedStorage is the read-only status row for the system-managed
// shared LUKS volume (created by scripts/create-shared-volume.sh,
// auto-unlocked at boot by shared-store.service).
//
// Status values:
//
//	"mounted"         — blob exists, keyfile present, currently mounted
//	"not mounted"     — blob exists but mapper isn't open / fs isn't mounted
//	"not provisioned" — no blob (create-shared-volume.sh hasn't run)
type SharedStorage struct {
	Status        string // see comment above
	Provisioned   bool
	Mounted       bool
	OnDiskBytes   int64 // real on-disk consumption of the sparse .img
	UsedInner     int64 // bytes used inside the mounted ext4, 0 when not mounted
	InnerCapacity int64 // ext4 size when mounted, 0 when not mounted
	FreeInner     int64 // bytes free inside the mounted ext4, 0 when not mounted
}

// UserStorage is the per-user row in the storage panel.
type UserStorage struct {
	Name          string
	Exists        bool
	Mounted       bool
	NominalBytes  int64 // luksSizeGB advertised envelope
	OnDiskBytes   int64 // real on-disk consumption of the sparse blob
	UsedInner     int64 // bytes used inside the mounted ext4, 0 when locked
	InnerCapacity int64 // ext4 size when mounted, 0 when locked
}

// storageSnapshot collects the data the /users storage panel renders.
// Read-only; tolerates Stat/Statfs errors per-user (zeroes that field).
//
// Per-user `NominalBytes` resolution priority (the dashboard's "nominal"
// column reflects the operator's actual choice, never the env-var fallback
// when something better is known):
//
//  1. Apparent size of the user's `.img` on disk — the truth for any volume
//     that exists. This is the LUKS blob's declared envelope (sparse files
//     report the requested size via `Stat()`, not the on-disk allocation).
//  2. PersonalLUKSSize from /srv/store/setup/state.json — what the operator
//     picked on the wizard's storage step. Applies to users whose volume
//     hasn't been provisioned yet (e.g. the admin pre-finalize, or a
//     queued user awaiting LUKS create after a failed first attempt).
//  3. cfg.luksSizeGB — the legacy `ENROL_LUKS_SIZE_GB` env-var fallback,
//     used only when neither of the above is available (e.g. fresh
//     /users/new on a host where the wizard pre-dates the storage step).
func storageSnapshot(cfg config, users []string) StorageInfo {
	si := StorageInfo{Root: "/srv/store"}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(si.Root, &fs); err == nil {
		si.TotalBytes = int64(fs.Blocks) * int64(fs.Bsize)
		si.FreeBytes = int64(fs.Bavail) * int64(fs.Bsize)
	}
	defaultNominal := defaultPersonalNominalBytes(cfg)
	for _, name := range users {
		us := UserStorage{Name: name, NominalBytes: defaultNominal}
		img, mnt, _ := volumePaths(cfg, name)
		if st, err := os.Stat(img); err == nil {
			us.Exists = true
			// Apparent size = LUKS envelope chosen at create time. This is
			// the per-user "nominal" the dashboard advertises; it's
			// authoritative once the volume exists, regardless of what the
			// wizard or env var would suggest.
			us.NominalBytes = st.Size()
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				us.OnDiskBytes = int64(sys.Blocks) * 512
			}
		}
		if isMounted(mnt) {
			us.Mounted = true
			var mfs syscall.Statfs_t
			if err := syscall.Statfs(mnt, &mfs); err == nil {
				us.InnerCapacity = int64(mfs.Blocks) * int64(mfs.Bsize)
				us.UsedInner = int64(mfs.Blocks-mfs.Bfree) * int64(mfs.Bsize)
			}
		}
		si.UserOnDisk += us.OnDiskBytes
		si.UserUsedInner += us.UsedInner
		si.NominalBytes += us.NominalBytes
		si.Users = append(si.Users, us)
	}
	si.Shared = sharedSnapshot(cfg)
	// Account for the shared volume's on-disk footprint in the
	// "system" residual so the storage bar adds up cleanly.
	si.SystemBytes = si.TotalBytes - si.FreeBytes - si.UserOnDisk - si.Shared.OnDiskBytes
	if si.SystemBytes < 0 {
		si.SystemBytes = 0
	}
	return si
}

// defaultPersonalNominalBytes returns the per-user nominal size the
// dashboard should attribute to a user whose `.img` doesn't (yet) exist
// on disk. Reads /srv/store/setup/state.json's PersonalLUKSSize first
// (the wizard-captured operator choice); falls back to cfg.luksSizeGB
// (`ENROL_LUKS_SIZE_GB`) when state.json is missing/unparseable/zero.
//
// Only the two fields needed are unmarshalled — we deliberately avoid
// importing setupState here to keep luks.go free of the wizard's
// concerns (and so the unit tests don't drag setup.go's transitive
// deps in). Best-effort: any read/parse error silently falls back to
// the env default rather than failing the dashboard render.
func defaultPersonalNominalBytes(cfg config) int64 {
	envFallback := int64(cfg.luksSizeGB) << 30
	if cfg.setupStateDir == "" {
		return envFallback
	}
	b, err := os.ReadFile(filepath.Join(cfg.setupStateDir, "state.json"))
	if err != nil {
		return envFallback
	}
	var partial struct {
		PersonalLUKSSize int64 `json:"personal_luks_size"`
	}
	if err := json.Unmarshal(b, &partial); err != nil {
		return envFallback
	}
	if partial.PersonalLUKSSize > 0 {
		return partial.PersonalLUKSSize
	}
	return envFallback
}

// sharedSnapshot reads the shared volume's on-disk + mount state.
// Read-only; tolerates Stat/Statfs errors by zeroing the relevant fields.
// Path is fixed (matches scripts/create-shared-volume.sh +
// host/systemd/shared-store.service): the shared volume is system-
// managed, not a per-user volume, so it doesn't go through volumePaths.
func sharedSnapshot(cfg config) SharedStorage {
	const sharedName = "_shared"
	img := filepath.Join(cfg.storeDataDir, sharedName+".img")
	mnt := filepath.Join(cfg.storeMntDir, sharedName)

	ss := SharedStorage{Status: "not provisioned"}
	if st, err := os.Stat(img); err == nil {
		ss.Provisioned = true
		ss.Status = "not mounted"
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			ss.OnDiskBytes = int64(sys.Blocks) * 512
		}
		if isMounted(mnt) {
			ss.Mounted = true
			ss.Status = "mounted"
			var mfs syscall.Statfs_t
			if err := syscall.Statfs(mnt, &mfs); err == nil {
				ss.InnerCapacity = int64(mfs.Blocks) * int64(mfs.Bsize)
				ss.UsedInner = int64(mfs.Blocks-mfs.Bfree) * int64(mfs.Bsize)
				ss.FreeInner = int64(mfs.Bavail) * int64(mfs.Bsize)
			}
		}
	}
	return ss
}

func volumePaths(cfg config, user string) (img, mountpoint, mapper string) {
	return filepath.Join(cfg.storeDataDir, user+".img"),
		filepath.Join(cfg.storeMntDir, user),
		"store_" + user
}

// describeVolume returns a VolumeInfo describing the on-disk state.
func describeVolume(cfg config, user string) VolumeInfo {
	img, mnt, mapper := volumePaths(cfg, user)
	v := VolumeInfo{User: user, ImagePath: img, Mountpoint: mnt, Mapper: mapper}
	if st, err := os.Stat(img); err == nil {
		v.Exists = true
		v.SizeBytes = st.Size()
	}
	v.Mounted = isMounted(mnt)
	return v
}

// isMounted returns true iff `path` is a current mountpoint.
func isMounted(path string) bool {
	// Use /proc/self/mountinfo so we don't need the `mountpoint` binary.
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		// Field 5 of mountinfo is the mountpoint.
		fields := strings.Fields(ln)
		if len(fields) >= 5 && fields[4] == path {
			return true
		}
	}
	return false
}

// luksCreate creates a fresh LUKS2 blob + ext4 fs at /srv/store/data/<u>.img,
// initializes the mountpoint, leaves the volume CLOSED on return. Mirrors
// scripts/create-store-volume.sh. Resolves the envelope via the same
// precedence the dashboard uses (setupState.PersonalLUKSSize first, then
// the ENROL_LUKS_SIZE_GB env fallback) so the size shown for prospective
// users in the storage panel matches what their .img will actually be at
// create time. Use luksCreateWithSize when a specific size is known
// (e.g. finalize provisioning the admin's volume from a literal byte
// count).
func luksCreate(cfg config, user, passphrase string) error {
	return luksCreateWithSize(cfg, user, passphrase, defaultPersonalNominalBytes(cfg))
}

// luksCreateWithSize is luksCreate parametrised by an explicit envelope in
// raw bytes. The dd seek= takes a byte count, so any positive value works
// (we don't require an exact GiB multiple). Internal helper; callers must
// have validated `sizeBytes > 0` upstream.
func luksCreateWithSize(cfg config, user, passphrase string, sizeBytes int64) error {
	if !validUsername(user) {
		return fmt.Errorf("invalid username %q", user)
	}
	if len(passphrase) < 1 {
		return errors.New("LUKS passphrase is empty")
	}
	if sizeBytes <= 0 {
		return fmt.Errorf("LUKS size must be positive, got %d bytes", sizeBytes)
	}
	img, mnt, mapper := volumePaths(cfg, user)

	if _, err := os.Stat(img); err == nil {
		return fmt.Errorf("%s already exists — refusing to clobber", img)
	}
	if err := os.MkdirAll(cfg.storeDataDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.storeMntDir, 0o755); err != nil {
		return err
	}

	// Sparse blob. Pass byte-precise seek= so wizard-chosen non-GiB
	// multiples (rare but allowed) land at exactly the requested size.
	size := fmt.Sprintf("%d", sizeBytes)
	if err := runCommand("dd", "if=/dev/zero", "of="+img,
		"bs=1", "count=0", "seek="+size); err != nil {
		return fmt.Errorf("dd create %s: %w", img, err)
	}
	if err := os.Chmod(img, 0o600); err != nil {
		_ = os.Remove(img)
		return err
	}

	// luksFormat — passphrase via stdin.
	cmd := exec.Command("cryptsetup", "luksFormat",
		"--type", "luks2",
		"--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--hash", "sha512",
		"--pbkdf", "argon2id",
		"--use-urandom",
		"--batch-mode", // skip the YES prompt
		"--key-file=-", // read passphrase from stdin
		img)
	cmd.Stdin = strings.NewReader(passphrase)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(img)
		return fmt.Errorf("luksFormat: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Open, mkfs, mount, chown, unmount, close.
	if err := luksOpenWithPassphrase(img, mapper, passphrase); err != nil {
		_ = os.Remove(img)
		return err
	}
	defer func() {
		_ = exec.Command("cryptsetup", "close", mapper).Run()
	}()

	if err := runCommand("mkfs.ext4", "-q", "-L", mapper, "/dev/mapper/"+mapper); err != nil {
		return fmt.Errorf("mkfs.ext4: %w", err)
	}

	uid, gid, err := lookupHostUser(user)
	if err != nil {
		return fmt.Errorf("user %q not on host (run useradd first): %w", user, err)
	}

	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	if err := os.Chown(mnt, uid, gid); err != nil {
		return err
	}
	if err := runCommand("mount", "/dev/mapper/"+mapper, mnt); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	if err := os.Chown(mnt, uid, gid); err != nil {
		_ = exec.Command("umount", mnt).Run()
		return err
	}
	if err := os.Chmod(mnt, 0o700); err != nil {
		_ = exec.Command("umount", mnt).Run()
		return err
	}
	if err := runCommand("umount", mnt); err != nil {
		return fmt.Errorf("umount: %w", err)
	}
	// Re-assert mountpoint owner+mode after unmount (now showing the
	// underlying directory again).
	_ = os.Chown(mnt, uid, gid)
	_ = os.Chmod(mnt, 0o700)
	return nil
}

// luksUnlock opens the blob and mounts it at the standard mountpoint.
// Idempotent: if already mounted, returns nil. If the mapper exists
// but the mount doesn't, attempts to mount.
func luksUnlock(cfg config, user, passphrase string) error {
	img, mnt, mapper := volumePaths(cfg, user)
	if _, err := os.Stat(img); err != nil {
		return fmt.Errorf("no LUKS blob at %s", img)
	}
	if isMounted(mnt) {
		return nil
	}
	if _, err := os.Stat("/dev/mapper/" + mapper); err != nil {
		if err := luksOpenWithPassphrase(img, mapper, passphrase); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	if err := runCommand("mount", "/dev/mapper/"+mapper, mnt); err != nil {
		// If mount fails, close the mapper to leave a clean state.
		_ = exec.Command("cryptsetup", "close", mapper).Run()
		return fmt.Errorf("mount: %w", err)
	}
	return nil
}

// luksLock unmounts and closes the mapper. Idempotent.
func luksLock(cfg config, user string) error {
	_, mnt, mapper := volumePaths(cfg, user)
	if isMounted(mnt) {
		if err := runCommand("umount", mnt); err != nil {
			return fmt.Errorf("umount: %w", err)
		}
	}
	if _, err := os.Stat("/dev/mapper/" + mapper); err == nil {
		if err := runCommand("cryptsetup", "close", mapper); err != nil {
			return fmt.Errorf("cryptsetup close: %w", err)
		}
	}
	return nil
}

// luksChangePassphrase adds a new keyslot, then removes the old. Order
// matters: add-then-remove leaves a working keyslot at all times even
// on partial failure.
func luksChangePassphrase(cfg config, user, oldPass, newPass string) error {
	img, _, _ := volumePaths(cfg, user)
	if _, err := os.Stat(img); err != nil {
		return fmt.Errorf("no LUKS blob at %s", img)
	}

	// luksAddKey: --key-file=- reads the *existing* passphrase from
	// stdin, --new-keyfile=/dev/stdin reads the new one. Older
	// cryptsetup versions also accept passing both via stdin lines —
	// safer: write the old to a memfd, pass --key-file pointing at
	// /dev/fd/N, and pipe the new to stdin. cryptsetup 2.7.x supports
	// "key-file=-" for the existing key + the new key as a positional
	// arg via stdin only when --batch-mode is used.
	//
	// The reliable pattern on cryptsetup 2.7.5 (verified on debian:
	// trixie-slim): write old to a memfd, point --key-file at it,
	// pass new on stdin.
	oldFD, err := writeMemfd("luks-oldkey", []byte(oldPass))
	if err != nil {
		return err
	}
	defer oldFD.Close()
	newFD, err := writeMemfd("luks-newkey", []byte(newPass))
	if err != nil {
		return err
	}
	defer newFD.Close()

	add := exec.Command("cryptsetup", "luksAddKey",
		"--batch-mode",
		"--key-file", "/dev/fd/3",
		img, "/dev/fd/4")
	add.ExtraFiles = []*os.File{oldFD, newFD}
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("luksAddKey: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// Re-seek both files (luksAddKey consumed them).
	_, _ = oldFD.Seek(0, 0)
	_, _ = newFD.Seek(0, 0)

	rm := exec.Command("cryptsetup", "luksRemoveKey",
		"--batch-mode",
		"--key-file", "/dev/fd/3",
		img)
	rm.ExtraFiles = []*os.File{oldFD}
	if out, err := rm.CombinedOutput(); err != nil {
		// New key is added but old key is still present — partial
		// success but acceptable; surface the error so the caller
		// audits "fail" with the detail.
		return fmt.Errorf("luksRemoveKey: %w (%s); new key was added, old key still works",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// luksDelete: lock the volume if open, then unlink the .img and remove
// the mountpoint dir. Best-effort; logs and continues on per-step failure.
//
// Why a plain unlink rather than `shred`: the .img is a LUKS2 ciphertext
// blob — every block is encrypted under a master key wrapped in the
// LUKS header at the start of the file. Once the file is unlinked the
// header (and therefore the wrapped master key) is gone; the underlying
// extents may linger on the disk for some time, but they're statistically
// unrecoverable without that key. `shred -z` on a 50 GB sparse image
// writes 4 passes (random + zero) over EVERY block, which (a) takes
// 5–15 minutes per volume on commodity hardware and (b) inflates the
// sparse file to its full envelope size BEFORE deleting it — so a
// "delete" can briefly need 50 GB of free disk per volume. For this
// installer's threat model (encrypted at rest, host-level wipe is
// out-of-scope) plain unlink is the right choice.
func luksDelete(cfg config, user string) error {
	if err := luksLock(cfg, user); err != nil {
		// Continue — the blob may not be open.
	}
	img, mnt, _ := volumePaths(cfg, user)
	if _, err := os.Stat(img); err == nil {
		if err := os.Remove(img); err != nil {
			return fmt.Errorf("rm img: %w", err)
		}
	}
	if err := os.RemoveAll(mnt); err != nil {
		return fmt.Errorf("rm mountpoint: %w", err)
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

// luksOpenWithPassphrase opens the LUKS blob at img with the given
// passphrase, mapping it as `name`.
func luksOpenWithPassphrase(img, name, passphrase string) error {
	cmd := exec.Command("cryptsetup", "open",
		"--type", "luks2",
		"--key-file=-",
		img, name)
	cmd.Stdin = strings.NewReader(passphrase)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cryptsetup open %s: %w (%s)",
			img, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeMemfd creates an in-memory file (memfd_create), writes data to
// it, rewinds, and returns the open *os.File. Useful for passing
// secrets to subprocess via /dev/fd/N without leaving them on disk.
//
// On platforms without memfd we'd need to fall back to /tmp; but
// debian:trixie-slim runs Linux 5.14+ where memfd is unconditional.
func writeMemfd(name string, data []byte) (*os.File, error) {
	fd, err := unixMemfdCreate(name)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// unixMemfdCreate is the syscall wrapper. We avoid pulling
// golang.org/x/sys/unix here; a raw syscall keeps the dep set lean.
// MFD_CLOEXEC=1, no other flags. amd64 syscall number 319.
func unixMemfdCreate(name string) (int, error) {
	const SYS_MEMFD_CREATE = 319
	const MFD_CLOEXEC = 0x0001
	bp, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	r1, _, e1 := syscall.Syscall(SYS_MEMFD_CREATE,
		uintptr(unsafe.Pointer(bp)), MFD_CLOEXEC, 0)
	if e1 != 0 {
		return 0, e1
	}
	return int(r1), nil
}

// runCommand runs `name args...` and returns nil on exit code 0. On
// failure it includes the combined output in the error.
func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lookupHostUser returns the host uid/gid for `name`. We must read this
// from the host's /etc/passwd, NOT the container's. nsenter into PID 1's
// mount namespace gives us that. The container's own /etc/passwd is the
// image's snapshot from build-time and would not see users added at
// runtime.
//
// (We considered bind-mounting /etc/passwd into the container — that
// fails because useradd uses the rename-onto-/etc/passwd pattern which
// invalidates a single-file bind on each write. So we route both reads
// and writes through nsenter, keeping the container's image-time passwd
// untouched as a stable fallback.)
func lookupHostUser(name string) (int, int, error) {
	out, err := exec.Command("nsenter", "--target", "1", "--mount", "--",
		"getent", "passwd", name).Output()
	if err != nil || len(out) == 0 {
		return 0, 0, fmt.Errorf("user %q not found", name)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(parts) < 4 {
		return 0, 0, fmt.Errorf("malformed passwd entry for %q", name)
	}
	uid, err := atoiSafe(parts[2])
	if err != nil {
		return 0, 0, err
	}
	gid, err := atoiSafe(parts[3])
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// hostUserAdd creates a host system user (no home dir, no login shell)
// idempotently. Required before chowning the LUKS mountpoint to that
// user's uid. If the user already exists we return nil.
//
// Implementation: useradd writes /etc/passwd via the
// "write-to-/etc/passwd+ then rename" pattern. Inside our container,
// /etc/passwd is a single-file bind-mount, and rename onto a single-
// file mount is EBUSY. We therefore run useradd in the host's mount
// namespace via nsenter, which uses the host's /etc directly.
//
// Bind-mounting all of /etc was considered and rejected — too coarse
// a privilege grant; the nsenter path is only used by useradd/userdel
// and getent.
func hostUserAdd(name string) error {
	if _, _, err := lookupHostUser(name); err == nil {
		return nil
	}
	if err := runHostCommand("useradd", "-M", "-s", "/usr/sbin/nologin", "-U", name); err != nil {
		return fmt.Errorf("useradd %s: %w", name, err)
	}
	return nil
}

// hostUserDel removes the host user. Best-effort — userdel can fail if
// the user has live processes; we don't care because the LUKS volume
// is already gone by this point.
func hostUserDel(name string) error {
	if _, _, err := lookupHostUser(name); err != nil {
		return nil // not present
	}
	return runHostCommand("userdel", "-f", name)
}

// runHostCommand wraps the command in `nsenter --target 1 --mount --
// <name> <args...>` so writes to /etc land in the host's filesystem
// rather than the container's overlay. Required for useradd / userdel.
// Other paths (cryptsetup / mount / shred) operate on /srv/store which
// is bind-mounted rshared, so they don't need the namespace switch.
func runHostCommand(name string, args ...string) error {
	full := append([]string{"--target", "1", "--mount", "--", name}, args...)
	out, err := exec.Command("nsenter", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nsenter %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// atoiSafe — small int parsing without importing strconv here (it's
// already in peers.go but we keep this module-local for clarity).
func atoiSafe(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not an integer: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
