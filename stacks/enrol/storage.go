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
	"context"
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

// UserStorage is the per-user row in the storage panel. Name + OnDiskBytes
// + NominalBytes are the original cloud-era fields; the NC* + Plane*
// fields are added by the unified admin page (see ADR-005). Any of the
// new fields may be zero/sentinel when the upstream data source is
// unavailable — the template renders "—" rather than "0" so an empty
// Plane state isn't conflated with a real zero.
type UserStorage struct {
	Name         string
	Exists       bool  // true iff the user has any files on disk yet
	OnDiskBytes  int64 // `du -sb /srv/store/cloud-data/<u>/files`, 0 when missing
	NominalBytes int64 // configured per-user quota; 0 = unlimited

	// Nextcloud `occ user:info` cross-checks (per ADR-005). NCQuotaUsed
	// is what Nextcloud thinks the user is using (may differ from the du
	// number if external storage / shares are mounted in); -1 signals
	// "occ failed" so the template can render "—".
	NCQuotaUsed int64
	NCFileCount int

	// Plane API per-user counts (per ADR-005). Both default to 0 when
	// Plane is not deployed yet OR the API token is missing OR the user
	// hasn't logged into Plane yet (graceful fallback).
	TaskCount   int
	TaskAttBytes int64

	// LastSeen is max(NC last_login, Plane last_active) — best-effort.
	// Zero when both upstreams are unavailable.
	LastSeen time.Time
}

// NCInfo is the parsed result of `occ user:info <name> --output=json`.
// Fields beyond the three the admin page uses are intentionally dropped
// — adding them later is non-breaking, removing them is a UI churn.
type NCInfo struct {
	QuotaUsed int64
	FileCount int
	LastLogin time.Time
}

// TaskInfo aggregates the per-user Plane numbers the admin page renders.
// Sourced from one or more TaskClient API calls.
type TaskInfo struct {
	Issues   int
	AttBytes int64
	LastSeen time.Time
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

// storageUserSpec is the per-user input to storageSnapshot. Name is the
// Authelia username (used for du + occ); Email is the Authelia email
// claim (used to look the user up in Plane via UserByEmail). Email may
// be empty — callers can pass nil/zero specs and the Plane columns
// will simply remain zero.
type storageUserSpec struct {
	Name  string
	Email string
}

// storageSnapshot collects the data the /users admin page renders.
// Read-only; tolerates Statfs / du / occ / Plane failures per-user
// (zeroes that field, sometimes signalling unknown via -1 or zero time).
//
// The default per-user quota comes from setupState.CloudUserQuotaGB (raw
// GB; 0 = unlimited which renders as "—" in the template). Falls back to
// 50 GB when state.json is missing/unparseable.
//
// taskClient may be nil — in that case the Plane columns silently stay
// at their zero values (rendered as "—" by the template). This is the
// expected state during Wave B (before Plane itself is deployed).
func storageSnapshot(cfg config, users []storageUserSpec, taskClient *TaskClient) StorageInfo {
	si := StorageInfo{Root: "/srv/store"}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(si.Root, &fs); err == nil {
		si.TotalBytes = int64(fs.Blocks) * int64(fs.Bsize)
		si.FreeBytes = int64(fs.Bavail) * int64(fs.Bsize)
	}
	defaultNominal := defaultCloudQuotaBytes(cfg)
	for _, u := range users {
		name := u.Name
		us := UserStorage{Name: name, NominalBytes: defaultNominal, NCQuotaUsed: -1}
		bytes, exists := userOnDiskBytes(name)
		us.Exists = exists
		us.OnDiskBytes = bytes

		// Nextcloud occ user:info (cached). Best-effort — on shell-out
		// failure we leave NCQuotaUsed at the -1 sentinel so the
		// template renders "—" rather than "0 B" (which would be a lie).
		if nc, err := nextcloudUserInfo(name); err == nil {
			us.NCQuotaUsed = nc.QuotaUsed
			us.NCFileCount = nc.FileCount
			if !nc.LastLogin.IsZero() && nc.LastLogin.After(us.LastSeen) {
				us.LastSeen = nc.LastLogin
			}
		}

		// Plane API. Silent fallback on every failure mode (no client,
		// no token, user not in Plane, API down, …) — see TaskInfo
		// helper docs.
		if pi, err := taskUserInfo(taskClient, u.Email); err == nil {
			us.TaskCount = pi.Issues
			us.TaskAttBytes = pi.AttBytes
			if !pi.LastSeen.IsZero() && pi.LastSeen.After(us.LastSeen) {
				us.LastSeen = pi.LastSeen
			}
		}

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

// ---------------------------------------------------------------------------
// Nextcloud per-user info (occ user:info)
//
// occ shell-out is ~50 ms per user; the dashboard's 60 s in-memory
// cache (mirror of duCache) keeps the click-around responsive without
// hammering Nextcloud on every reload.

type ncCacheEntry struct {
	info NCInfo
	err  error
	at   time.Time
}

var (
	ncCacheMu sync.Mutex
	ncCache   = map[string]ncCacheEntry{}
)

// nextcloudUserInfo runs `docker exec -u www-data cloud php occ
// user:info <name> --output=json` and parses the result. The cache TTL
// matches duCacheTTL so an admin refreshing the page doesn't re-shell
// every reload. On any error, returns a zero-valued NCInfo + the
// underlying error — caller decides whether to surface zeros or skip
// the row.
//
// Output shape (Nextcloud 28+):
//
//	{
//	  "user_id": "raph",
//	  "display_name": "Raphael",
//	  "email": "raph@example.com",
//	  "last_seen": "2026-04-25T12:34:56+00:00",
//	  "quota": {
//	    "free": 12345, "used": 67890, "total": 100000, "relative": 67.89,
//	    "quota": -3
//	  }
//	}
//
// We extract only the three fields the admin page renders. Fields
// Nextcloud renames in future versions degrade gracefully to zero.
func nextcloudUserInfo(name string) (NCInfo, error) {
	ncCacheMu.Lock()
	defer ncCacheMu.Unlock()
	if e, ok := ncCache[name]; ok && time.Since(e.at) < duCacheTTL {
		return e.info, e.err
	}
	cacheAndReturn := func(info NCInfo, err error) (NCInfo, error) {
		ncCache[name] = ncCacheEntry{info: info, err: err, at: time.Now()}
		return info, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", "-u", "www-data",
		"cloud", "php", "occ", "user:info", name, "--output=json").Output()
	if err != nil {
		return cacheAndReturn(NCInfo{}, fmt.Errorf("occ user:info %s: %w", name, err))
	}

	// Tolerant decode: occ wraps the payload differently across versions
	// (sometimes top-level, sometimes under "user_id"). We use a
	// permissive struct shape that catches both common layouts.
	var raw struct {
		Quota struct {
			Used  json.Number `json:"used"`
			Files json.Number `json:"files"`
		} `json:"quota"`
		Files    json.Number `json:"files"`
		LastSeen string      `json:"last_seen"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return cacheAndReturn(NCInfo{}, fmt.Errorf("occ user:info %s: parse json: %w", name, err))
	}

	info := NCInfo{}
	if v, err := raw.Quota.Used.Int64(); err == nil {
		info.QuotaUsed = v
	}
	// File count: prefer top-level "files", fall back to nested.
	if v, err := raw.Files.Int64(); err == nil {
		info.FileCount = int(v)
	} else if v, err := raw.Quota.Files.Int64(); err == nil {
		info.FileCount = int(v)
	}
	if raw.LastSeen != "" {
		// occ emits ISO-8601 with tz offset; tolerate both RFC3339 and
		// the bare YYYY-MM-DD form.
		if t, err := time.Parse(time.RFC3339, raw.LastSeen); err == nil {
			info.LastLogin = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", raw.LastSeen); err == nil {
			info.LastLogin = t
		}
	}
	return cacheAndReturn(info, nil)
}

// ---------------------------------------------------------------------------
// Plane per-user info
//
// Aggregates ListWorkspaces + UserByEmail + per-workspace IssueCount /
// FileAssetBytes into a single TaskInfo row. Silent-fallback on every
// edge: nil client, empty token, user-not-in-Plane, API down. The admin
// page's columns degrade to "—" rather than 500-ing.

func taskUserInfo(client *TaskClient, email string) (TaskInfo, error) {
	if client == nil || !client.ready() || email == "" {
		return TaskInfo{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	user, err := client.UserByEmail(ctx, email)
	if err != nil || user == nil || user.ID == "" {
		// User isn't in Plane yet, or API is down — render zeros.
		return TaskInfo{}, nil
	}
	workspaces, err := client.ListWorkspaces(ctx)
	if err != nil {
		return TaskInfo{}, nil
	}

	var info TaskInfo
	for _, ws := range workspaces {
		if n, err := client.IssueCount(ctx, ws.Slug, user.ID); err == nil {
			info.Issues += n
		}
		if b, err := client.FileAssetBytes(ctx, ws.Slug, user.ID); err == nil {
			info.AttBytes += b
		}
	}
	if t, err := client.LastActivity(ctx, user.ID); err == nil {
		info.LastSeen = t
	}
	return info, nil
}
