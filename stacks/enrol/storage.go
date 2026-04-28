// storage.go — host-side disk + per-user Nextcloud usage snapshot.
//
// Replaces the prior LUKS-era implementation in luks.go. Nextcloud stores
// each user's data under /srv/store/cloud-data/<u>/files; a `du -sb` over
// that path is the per-user "on-disk" reading the dashboard renders. Free
// / total bytes still come from syscall.Statfs of /srv/store. There is no
// "shared volume" anymore — Nextcloud's group-folders app provides shared
// space inside the same datadir.
//
// `du` shells out, so we cache results for 60 s in-process to keep the
// /users render cheap for the operator clicking around.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// StorageInfo aggregates host-disk + per-user Nextcloud metrics for the
// /users overview panel. The shape mirrors the prior LUKS-era version
// minus the SharedStorage row, with NominalBytes now meaning "configured
// per-user quota" rather than "LUKS envelope".
type StorageInfo struct {
	Root         string // filesystem root we Statfs'd (e.g. /srv/store)
	TotalBytes   int64  // total capacity of the underlying filesystem
	FreeBytes    int64  // bytes free to non-root
	UserOnDisk   int64  // sum of per-user data dir bytes
	NominalBytes int64  // sum of per-user configured quotas
	SystemBytes  int64  // total - free - userOnDisk (everything else)
	Users        []UserStorage
}

// UserStorage is the per-user row in the storage panel. Only Name,
// OnDiskBytes, NominalBytes are meaningful in the cloud era.
type UserStorage struct {
	Name         string
	Exists       bool  // true iff the user has any files on disk yet
	OnDiskBytes  int64 // `du -sb /srv/store/cloud-data/<u>/files`, 0 when missing
	NominalBytes int64 // configured per-user quota; 0 = unlimited
}

// cloudDataRoot is the on-disk root Nextcloud bind-mounts as its datadir.
// Per-user files live under <root>/<username>/files. Matches Wave 1A's
// stacks/cloud/docker-compose.yml.
const cloudDataRoot = "/srv/store/cloud-data"

// duCacheTTL is how long a `du -sb` result is reused before a re-shell.
// 60 s is the operator click-around horizon; longer would feel stale,
// shorter would wedge the dashboard behind serialised du runs.
const duCacheTTL = 60 * time.Second

type duCacheEntry struct {
	bytes  int64
	exists bool
	at     time.Time
}

var (
	duCacheMu sync.Mutex
	duCache   = map[string]duCacheEntry{}
)

// storageSnapshot collects the data the /users storage panel renders.
// Read-only; tolerates Statfs / du failures per-user (zeroes that field).
//
// The default per-user quota comes from setupState.CloudUserQuotaGB (raw
// GB; 0 = unlimited which renders as "—" in the template). Falls back to
// 50 GB when state.json is missing/unparseable.
func storageSnapshot(cfg config, users []string) StorageInfo {
	si := StorageInfo{Root: "/srv/store"}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(si.Root, &fs); err == nil {
		si.TotalBytes = int64(fs.Blocks) * int64(fs.Bsize)
		si.FreeBytes = int64(fs.Bavail) * int64(fs.Bsize)
	}
	defaultNominal := defaultCloudQuotaBytes(cfg)
	for _, name := range users {
		us := UserStorage{Name: name, NominalBytes: defaultNominal}
		bytes, exists := userOnDiskBytes(name)
		us.Exists = exists
		us.OnDiskBytes = bytes
		si.UserOnDisk += us.OnDiskBytes
		si.NominalBytes += us.NominalBytes
		si.Users = append(si.Users, us)
	}
	si.SystemBytes = si.TotalBytes - si.FreeBytes - si.UserOnDisk
	if si.SystemBytes < 0 {
		si.SystemBytes = 0
	}
	return si
}

// defaultCloudQuotaBytes returns the per-user Nextcloud quota in bytes
// from setupState.CloudUserQuotaGB, falling back to 50 GiB (matching the
// wizard default) when state.json is missing/unparseable. A zero value in
// state.json means "unlimited" → returns 0 so the template can render
// "—" rather than "0 B".
func defaultCloudQuotaBytes(cfg config) int64 {
	const fallback = int64(50) << 30
	if cfg.setupStateDir == "" {
		return fallback
	}
	b, err := os.ReadFile(filepath.Join(cfg.setupStateDir, "state.json"))
	if err != nil {
		return fallback
	}
	var partial struct {
		CloudUserQuotaGB int `json:"cloud_user_quota_gb"`
	}
	if err := json.Unmarshal(b, &partial); err != nil {
		return fallback
	}
	if partial.CloudUserQuotaGB < 0 {
		return fallback
	}
	if partial.CloudUserQuotaGB == 0 {
		// 0 = unlimited — surface as 0 so callers can render "—".
		return 0
	}
	return int64(partial.CloudUserQuotaGB) << 30
}

// userOnDiskBytes returns the cached or freshly-derived size of the user's
// Nextcloud datadir. Returns (bytes, exists). Missing dir → (0, false).
// Errors from `du` → (0, true) (best-effort; dashboard renders zero).
func userOnDiskBytes(user string) (int64, bool) {
	duCacheMu.Lock()
	defer duCacheMu.Unlock()
	if e, ok := duCache[user]; ok && time.Since(e.at) < duCacheTTL {
		return e.bytes, e.exists
	}
	path := filepath.Join(cloudDataRoot, user, "files")
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		duCache[user] = duCacheEntry{bytes: 0, exists: false, at: time.Now()}
		return 0, false
	}
	out, err := exec.Command("du", "-sb", path).Output()
	if err != nil {
		// `du` can fail on permission errors inside subdirs. Cache zero
		// so we don't re-shell on every dashboard hit; the next TTL
		// expiry retries.
		duCache[user] = duCacheEntry{bytes: 0, exists: true, at: time.Now()}
		return 0, true
	}
	fields := strings.Fields(string(out))
	if len(fields) < 1 {
		duCache[user] = duCacheEntry{bytes: 0, exists: true, at: time.Now()}
		return 0, true
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		duCache[user] = duCacheEntry{bytes: 0, exists: true, at: time.Now()}
		return 0, true
	}
	duCache[user] = duCacheEntry{bytes: n, exists: true, at: time.Now()}
	return n, true
}

// runCommand runs `name args...` and returns nil on exit code 0. On
// failure it includes the combined output in the error. Kept here (vs
// inlined into each caller) so future shell-outs (e.g. occ) get a
// uniform error format.
func runCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
