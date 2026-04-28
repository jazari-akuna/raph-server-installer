// backup.go — admin /backup page + restic engine + CLI subcommand.
//
// One file because everything in here is the same feature surface: the
// recipe table, the restic shell-out helpers, the in-flight op tracker,
// the four HTTP handlers, the SSE stream, and the `enrol backup` CLI
// subcommand all reference the same private types. Splitting them
// across files would mostly trade one large file for one large diff
// across five files; the tests in backup_test.go exercise the entire
// pure surface (recipe lookup, retention math, path-traversal guards,
// envfile parsing, JSON parse) without needing to fan out either.
//
// Architecture (see ADR-010, plans/hazy-hugging-wave.md):
//
//   • One restic repo for all stacks at cfg.backupRepoDir, password at
//     cfg.backupPasswordFile. Each backup is tagged with the stack id
//     ("cloud", "task", ...) AND the run kind ("daily", "manual",
//     "pre_restore"). `restic snapshots --tag <id>` filters per-stack;
//     retention math is per-tag too so manual snapshots aren't culled
//     by a daily-run forget pass.
//
//   • Five recipes (cloud, task, authelia, ingress, enrol) drive both
//     UI rows and CLI iteration. Each recipe knows: its display label,
//     the docker compose services that need stopping, an optional
//     pg_dump target, and the host paths to snapshot. Adding a stack
//     is one slice append.
//
//   • restoresHave a typed-confirmation guard (subtle.ConstantTimeCompare
//     against the recipe id) AND auto-take a `--tag pre_restore`
//     snapshot first so a botched restore is itself recoverable.
//
//   • Every UI op holds backupOpMu — the package-level mutex — so
//     parallel "Backup all" + "Restore task" can't tangle. Restic's
//     own `--lock` would also reject the second invocation but
//     producing a 409 in the HTTP layer is friendlier than letting
//     restic spit a lock-file error mid-stream.
//
//   • Two execution paths share the same engine:
//       - HTTP handler → spawn goroutine → register in opTracker →
//         SSE handler streams the buffered events to the browser.
//       - CLI subcommand (`enrol backup --scheduled --tag=daily`) →
//         run synchronously, write progress lines to stdout + audit
//         log, exit non-zero on any per-stack failure (but every
//         stack still gets a turn — no early-bail).

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// shared DTO (consumed by templates/backup.html via viewerData.Backup)

// BackupPageView is the page-data for /backup. The field names + JSON
// shape are the load-bearing contract between this file (Agent A) and
// templates/backup.html (Agent B); rename or retype with care, and update
// the template in lockstep.
type BackupPageView struct {
	RepoSize      string // human-readable (e.g. "412 MB"); "—" if repo not initialised yet.
	RepoSnapCount int    // total snapshot count across all tags.
	NextScheduled string // "2026-04-29 03:00 UTC" (read from systemctl list-timers); "—" if absent.
	Recipes       []BackupRecipeView
	OffHostHelp   OffHostCommandsView
	InProgress    string // op id of the currently-running op ("cloud:create:abcd1234"), empty when idle.
	CSRF          string
}

// BackupRecipeView is one row in the per-stack table.
type BackupRecipeView struct {
	ID, Display      string
	LastSnapshotAt   string // "2026-04-28 03:00 (daily)" or "—".
	LastSnapshotSize string // "312 MB" or "—".
	Restorable       bool
	Snapshots        []BackupSnapshotView // populated only when ?expand=<id> matches this row.
}

// BackupSnapshotView is one expanded snapshot under a recipe row.
type BackupSnapshotView struct {
	ID        string // restic short id (8 chars).
	CreatedAt string // "2026-04-28 03:00 UTC".
	Size      string // "312 MB" — derived from snapshot.Summary.TotalBytesProcessed.
	Tags      []string
}

// OffHostCommandsView are pre-rendered ready-to-paste command strings the
// operator copies into a remote shell to pull or remote-restore. Filled
// in server-side with the actual VPS hostname so there's no copy-paste
// error from a hand-edited example.
type OffHostCommandsView struct {
	RsyncCmd         string
	ResticCopyCmd    string
	ResticRestoreCmd string
}

// ---------------------------------------------------------------------------
// recipes

// pgDump describes a Postgres dump source for a recipe. The .env file is
// parsed via readPostgresPassword (envfile.go) to recover POSTGRES_PASSWORD;
// we shell out via `docker exec -e PGPASSWORD=... <Container> pg_dump --clean
// -U <User> <DB>` and stream the output straight into restic.
type pgDump struct {
	Container string // "cloud-db"
	DB        string // "nextcloud"
	User      string // "nextcloud"
	EnvFile   string // "/opt/stacks/cloud/.env"
}

// backupRecipe is the per-stack snapshot definition. New stack →
// append one entry to backupRecipes; the UI / CLI / retention logic all
// iterate the slice without further plumbing.
type backupRecipe struct {
	ID           string
	Display      string
	StopServices []string // compose service names to `docker compose stop` during snapshot.
	PGDump       *pgDump  // nil for non-Postgres stacks.
	Paths        []string // host paths fed to `restic backup`.
	Restorable   bool     // false for "enrol" — can't safely restore self mid-stream.
}

// backupRecipes is the canonical list driving the UI table, the CLI
// iteration, and the recipe-id whitelist for restore. Add a new stack
// here. Display strings camouflage the upstream image identity (ADR-002).
var backupRecipes = []backupRecipe{
	{
		ID:           "cloud",
		Display:      "Cloud (Nextcloud)",
		Restorable:   true,
		StopServices: nil,
		PGDump:       &pgDump{Container: "cloud-db", DB: "nextcloud", User: "nextcloud", EnvFile: "/opt/stacks/cloud/.env"},
		Paths:        []string{"/srv/store/cloud-data", "/srv/store/cloud-config", "/srv/store/cloud-apps"},
	},
	{
		ID:           "task",
		Display:      "Tasks (Vikunja)",
		Restorable:   true,
		StopServices: nil,
		PGDump:       &pgDump{Container: "task-db", DB: "vikunja", User: "vikunja", EnvFile: "/opt/stacks/task/.env"},
		Paths:        []string{"/srv/store/task-files"},
	},
	{
		// Authelia is NOT stopped during backup. Sessions live in-memory
		// (no Redis); a restart would force-logout every active user on
		// every nightly + on-demand snapshot. The users_database.yml file
		// is rewritten under enrol's mutex via single truncate+write, so a
		// mid-write capture is unlikely; if it ever happens, the next
		// nightly captures a clean copy. See ADR-010.
		ID:         "authelia",
		Display:    "Authelia (SSO)",
		Restorable: true,
		Paths:      []string{"/opt/stacks/authelia/data", "/opt/stacks/authelia/users_db", "/opt/stacks/authelia/secrets"},
	},
	{
		ID:         "ingress",
		Display:    "Ingress (NPM)",
		Restorable: true,
		Paths:      []string{"/opt/stacks/ingress/data", "/opt/stacks/ingress/letsencrypt"},
	},
	{
		ID:         "enrol",
		Display:    "Enrol (config)",
		Restorable: false,
		Paths:      []string{"/srv/store/enrol-launcher", "/srv/store/enrol-peers-archive", "/etc/raph-installer", "/opt/stacks/.env"},
	},
}

// recipeByID returns the matching recipe by id, or nil if unknown.
// Used by the restore handler's whitelist check + tests.
func recipeByID(id string) *backupRecipe {
	for i := range backupRecipes {
		if backupRecipes[i].ID == id {
			return &backupRecipes[i]
		}
	}
	return nil
}

// safeRestoreTargets validates that every path in `paths` is rooted under
// one of the recipe's whitelisted Paths prefixes. Used when the operator
// opts to restore an explicit subset of files (UI only restores all paths
// today; this guard exists so a future "restore single file" form cannot
// accidentally write outside the recipe's footprint, e.g. `/etc/shadow`).
//
// "Rooted under" is filepath.Clean-equality of the prefix or a strict
// child of the prefix (next char must be `/`). Symlinks are NOT
// resolved — the caller is responsible for ensuring restic doesn't
// follow user-controlled symlinks during restore (restic does not
// follow symlinks during restore, so this is safe in practice).
func safeRestoreTargets(paths []string, recipe *backupRecipe) error {
	if recipe == nil {
		return errors.New("safeRestoreTargets: nil recipe")
	}
	cleanedPrefixes := make([]string, 0, len(recipe.Paths))
	for _, p := range recipe.Paths {
		cleanedPrefixes = append(cleanedPrefixes, filepath.Clean(p))
	}
	for _, p := range paths {
		got := filepath.Clean(p)
		ok := false
		for _, prefix := range cleanedPrefixes {
			if got == prefix {
				ok = true
				break
			}
			if strings.HasPrefix(got, prefix+string(filepath.Separator)) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("path %q is not in recipe %q whitelist", p, recipe.ID)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// in-flight op tracking

// backupOp holds the live state of one in-progress backup or restore
// operation. The Logs slice is the buffered SSE history a late-connecting
// browser replays before tailing live events from Sub.
type backupOp struct {
	ID        string         // "<stack>:<verb>:<random8>" (e.g. "cloud:create:7f3a9b21")
	Stack     string         // recipe id ("cloud", "task", ...) or "all"
	Verb      string         // "create" | "restore"
	StartedAt time.Time
	Done      bool
	DoneAt    time.Time
	Failed    bool

	mu   sync.Mutex
	logs []sseFrame      // buffered events for late-connecting clients
	subs []chan sseFrame // live subscribers
}

// sseFrame is one buffered SSE event; replayed to late subscribers.
type sseFrame struct {
	Event   string // "status" | "log" | "error" | "done"
	Payload string // JSON object, single line (already escaped)
}

// append records one event. Safe for concurrent calls.
func (o *backupOp) append(event, payload string) {
	o.mu.Lock()
	frame := sseFrame{Event: event, Payload: payload}
	o.logs = append(o.logs, frame)
	subs := append([]chan sseFrame(nil), o.subs...)
	o.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- frame:
		default:
			// Slow consumer; drop. The full history is still in o.logs
			// for any subsequent reconnection.
		}
	}
}

// snapshot returns the current buffered logs + a fresh subscription
// channel. Caller must close the channel when done.
func (o *backupOp) snapshot() ([]sseFrame, chan sseFrame) {
	o.mu.Lock()
	defer o.mu.Unlock()
	hist := append([]sseFrame(nil), o.logs...)
	ch := make(chan sseFrame, 32)
	o.subs = append(o.subs, ch)
	return hist, ch
}

// unsubscribe removes ch from the subs slice. Best-effort; idempotent.
func (o *backupOp) unsubscribe(ch chan sseFrame) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.subs[:0]
	for _, c := range o.subs {
		if c != ch {
			out = append(out, c)
		}
	}
	o.subs = out
}

// finish marks the op as done (success or failure). Triggers GC eligibility
// after a grace window (see opTracker GC goroutine).
func (o *backupOp) finish(failed bool) {
	o.mu.Lock()
	o.Done = true
	o.Failed = failed
	o.DoneAt = time.Now().UTC()
	o.mu.Unlock()
}

// opTracker is the package-level registry of in-flight + recently-done ops.
// Map key is op.ID. A janitor goroutine evicts done ops after 30s so a
// browser that reconnects late still sees the terminal "done"/"error"
// frame.
type opTracker struct {
	mu  sync.RWMutex
	ops map[string]*backupOp

	janitorOnce sync.Once
}

var globalOpTracker = &opTracker{ops: map[string]*backupOp{}}

func (t *opTracker) register(op *backupOp) {
	t.mu.Lock()
	t.ops[op.ID] = op
	t.mu.Unlock()
	t.janitorOnce.Do(func() {
		go t.janitor()
	})
}

func (t *opTracker) get(id string) *backupOp {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ops[id]
}

// current returns the first non-done op (any), or nil if idle. Used by
// the page render to surface "in progress" state without taking a name.
func (t *opTracker) current() *backupOp {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, op := range t.ops {
		if !op.Done {
			return op
		}
	}
	return nil
}

func (t *opTracker) janitor() {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for range tick.C {
		cutoff := time.Now().UTC().Add(-30 * time.Second)
		t.mu.Lock()
		for id, op := range t.ops {
			op.mu.Lock()
			expire := op.Done && op.DoneAt.Before(cutoff)
			op.mu.Unlock()
			if expire {
				delete(t.ops, id)
			}
		}
		t.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// concurrency: at-most-one create/restore at a time

// backupOpMu serialises every create/restore operation (UI or CLI).
// restic itself locks the repo, but failing late hurts UX; the mutex
// produces a clean 409 at the HTTP boundary instead.
var backupOpMu sync.Mutex
var backupOpInFlight bool

var errBackupBusy = errors.New("another backup or restore operation is already in progress")

// tryAcquire attempts to claim the global lock. Returns a release closure
// (always safe to call exactly once) and nil on success; (nil, errBackupBusy)
// if the lock was already held.
func tryAcquire() (func(), error) {
	backupOpMu.Lock()
	if backupOpInFlight {
		backupOpMu.Unlock()
		return nil, errBackupBusy
	}
	backupOpInFlight = true
	backupOpMu.Unlock()
	return func() {
		backupOpMu.Lock()
		backupOpInFlight = false
		backupOpMu.Unlock()
	}, nil
}

// ---------------------------------------------------------------------------
// restic JSON: snapshots --json output parser

// resticSnapshot is the subset of restic's JSON snapshot record we read.
// Restic emits more fields (parent, paths, excludes, ...) but we only
// surface id+time+tags+summary in the UI.
type resticSnapshot struct {
	ID       string    `json:"id"`         // long id (64 chars); truncate to 8 for display.
	Time     time.Time `json:"time"`       // RFC3339.
	Tags     []string  `json:"tags"`       // includes both the recipe id and the kind ("daily"/"manual"/"pre_restore").
	Paths    []string  `json:"paths"`      // host paths captured.
	Hostname string    `json:"hostname"`   // we always pass --host=vps so this is constant.
	Summary  *struct {
		TotalBytesProcessed int64 `json:"total_bytes_processed"`
	} `json:"summary,omitempty"`
}

// shortID returns the first 8 chars of the long restic id, matching what
// `restic snapshots` shows in its tabular default output.
func (s resticSnapshot) shortID() string {
	if len(s.ID) <= 8 {
		return s.ID
	}
	return s.ID[:8]
}

// parseResticSnapshots decodes `restic snapshots --json` output into our
// view shape. Tolerates an empty body (no snapshots yet → nil slice).
func parseResticSnapshots(jsonBody []byte) ([]BackupSnapshotView, error) {
	body := strings.TrimSpace(string(jsonBody))
	if body == "" || body == "null" {
		return nil, nil
	}
	var raw []resticSnapshot
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("decode restic snapshots json: %w", err)
	}
	out := make([]BackupSnapshotView, 0, len(raw))
	for _, s := range raw {
		size := "—"
		if s.Summary != nil {
			size = formatBytes(s.Summary.TotalBytesProcessed)
		}
		out = append(out, BackupSnapshotView{
			ID:        s.shortID(),
			CreatedAt: s.Time.UTC().Format("2006-01-02 15:04 UTC"),
			Size:      size,
			Tags:      s.Tags,
		})
	}
	return out, nil
}

// formatBytes pretty-prints a byte count using base-10 SI units (matches
// how restic itself reports sizes in its `snapshots` output).
func formatBytes(n int64) string {
	const (
		kB int64 = 1000
		mB       = 1000 * kB
		gB       = 1000 * mB
		tB       = 1000 * gB
	)
	switch {
	case n >= tB:
		return fmt.Sprintf("%.1f TB", float64(n)/float64(tB))
	case n >= gB:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gB))
	case n >= mB:
		return fmt.Sprintf("%d MB", n/mB)
	case n >= kB:
		return fmt.Sprintf("%d KB", n/kB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---------------------------------------------------------------------------
// retention math (pure, unit-tested)

// snapshotsToForget returns the long IDs of snapshots that should be
// dropped to keep at most `keepDaily` of `all`. Oldest are forgotten
// first. `keepDaily <= 0` is treated as "keep none → forget all".
//
// The caller is responsible for filtering `all` to a single tag scope
// before passing in: the retention rule is per-tag (daily / manual /
// pre_restore each have their own count). This keeps the math obviously
// correct + trivially testable.
func snapshotsToForget(all []resticSnapshot, keepDaily int) []string {
	if len(all) == 0 {
		return nil
	}
	// Sort ascending by Time so forget-oldest-first is a head slice.
	sorted := append([]resticSnapshot(nil), all...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Time.Before(sorted[j].Time)
	})
	if keepDaily < 0 {
		keepDaily = 0
	}
	if len(sorted) <= keepDaily {
		return nil
	}
	dropCount := len(sorted) - keepDaily
	out := make([]string, 0, dropCount)
	for i := 0; i < dropCount; i++ {
		out = append(out, sorted[i].ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// restic shell-out helpers

// resticEnv is the env passed to every restic invocation. PATH is
// minimal so an attacker who manages to set $PATH in the parent process
// can't redirect us to a fake `restic`. RESTIC_PASSWORD_FILE is set
// rather than the password itself so the secret never appears in
// /proc/<pid>/environ.
func resticEnv(cfg config) []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/root",
		"RESTIC_PASSWORD_FILE=" + cfg.backupPasswordFile,
	}
}

// resticCmd builds an exec.Cmd with the standard arg prefix
// (-r <repo> --password-file <file>) followed by the supplied subcommand
// args. Caller fills in stdin/stdout/stderr.
func resticCmd(ctx context.Context, cfg config, args ...string) *exec.Cmd {
	full := append([]string{
		"-r", cfg.backupRepoDir,
		"--password-file", cfg.backupPasswordFile,
	}, args...)
	cmd := exec.CommandContext(ctx, cfg.backupResticBin, full...)
	cmd.Env = resticEnv(cfg)
	return cmd
}

// listSnapshotsForTag runs `restic snapshots --tag <id> --json` and
// returns the parsed view list. Empty list on any error (best-effort:
// the page should render even when restic isn't installed yet).
func listSnapshotsForTag(ctx context.Context, cfg config, tag string) ([]resticSnapshot, error) {
	cmd := resticCmd(ctx, cfg, "snapshots", "--tag", tag, "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(string(out))
	if body == "" || body == "null" {
		return nil, nil
	}
	var snaps []resticSnapshot
	if err := json.Unmarshal([]byte(body), &snaps); err != nil {
		return nil, fmt.Errorf("decode snapshots json: %w", err)
	}
	return snaps, nil
}

// repoStats returns (sizeHuman, snapCount). Best-effort: any error returns
// ("—", 0) so the page renders during bootstrap before the repo exists.
func repoStats(ctx context.Context, cfg config) (string, int) {
	cmd := resticCmd(ctx, cfg, "stats", "--json")
	out, err := cmd.Output()
	if err != nil {
		return "—", 0
	}
	var stats struct {
		TotalSize          int64 `json:"total_size"`
		SnapshotsCount     int   `json:"snapshots_count"`
		TotalFileCount     int64 `json:"total_file_count"`
		TotalBlobCount     int64 `json:"total_blob_count"`
	}
	if err := json.Unmarshal(out, &stats); err != nil {
		return "—", 0
	}
	return formatBytes(stats.TotalSize), stats.SnapshotsCount
}

// ---------------------------------------------------------------------------
// backup pipeline

// backupHostTag is the value passed to `restic --host` so snapshots are
// filterable by the conventional `--host=vps`. Restic defaults to the
// container's hostname (a docker-generated hex blob) which is useless
// for filtering.
const backupHostTag = "vps"

// runBackup snapshots one recipe with the given run-kind tag ("daily" /
// "manual" / "pre_restore"). Emits status + log frames into emit. Returns
// the captured snapshot id (long form) on success, non-nil error on any
// step failure.
//
// Pipeline:
//  1. (optional) docker compose stop <recipe.StopServices>
//  2. restic backup --tag <kind> --tag <recipe.id> --host vps <paths...>
//  3. (optional) restic backup --stdin via docker exec ... pg_dump
//  4. (optional) docker compose start <recipe.StopServices>
//
// Step 3 uses --stdin-from-command on restic versions that support it
// (>= 0.16); on older restic we fall back to a Go-side io.Pipe wiring
// pg_dump's stdout to restic's stdin. The probe is one-shot at startup
// (`resticSupportsStdinFromCommand`) so we don't pay for it per-call.
func runBackup(ctx context.Context, cfg config, recipe *backupRecipe, kind string, em sseEmitter) error {
	if recipe == nil {
		return errors.New("runBackup: nil recipe")
	}
	step := recipe.ID + ":" + kind
	em.Status(step, fmt.Sprintf("starting %s backup of %s", kind, recipe.Display))

	// 1. Stop services if required (Authelia is the only one today).
	if len(recipe.StopServices) > 0 {
		if err := composeServiceCmd(ctx, recipe, "stop", em); err != nil {
			return fmt.Errorf("stop services: %w", err)
		}
		// Restart on any subsequent error AND on success.
		defer func() {
			if startErr := composeServiceCmd(context.Background(), recipe, "start", em); startErr != nil {
				em.Log(fmt.Sprintf("warning: failed to restart %v: %v", recipe.StopServices, startErr))
			}
		}()
	}

	// 2. File-system snapshot via `restic backup`.
	if len(recipe.Paths) > 0 {
		em.Status(step, "snapshotting paths via restic")
		args := []string{
			"backup",
			"--tag", kind,
			"--tag", recipe.ID,
			"--host", backupHostTag,
			"--quiet",
		}
		args = append(args, recipe.Paths...)
		cmd := resticCmd(ctx, cfg, args...)
		if err := streamCmd(cmd, em); err != nil {
			return fmt.Errorf("restic backup: %w", err)
		}
	}

	// 3. Optional pg_dump streamed straight into restic.
	if recipe.PGDump != nil {
		em.Status(step, "pg_dump → restic")
		if err := runPGDumpBackup(ctx, cfg, recipe, kind, em); err != nil {
			return fmt.Errorf("pg_dump: %w", err)
		}
	}

	em.Status(step, "backup complete")
	return nil
}

// runPGDumpBackup wires `docker exec ... pg_dump` to `restic backup --stdin`
// without ever writing the .sql to disk. Falls back from
// --stdin-from-command (restic >= 0.16) to a Go-side io.Pipe on older
// versions; both produce identical snapshots.
func runPGDumpBackup(ctx context.Context, cfg config, recipe *backupRecipe, kind string, em sseEmitter) error {
	pwd, err := readPostgresPassword(recipe.PGDump.EnvFile)
	if err != nil {
		return fmt.Errorf("read pg password from %s: %w", recipe.PGDump.EnvFile, err)
	}
	if pwd == "" {
		return fmt.Errorf("POSTGRES_PASSWORD not found in %s", recipe.PGDump.EnvFile)
	}
	dumpFilename := recipe.ID + "-db.sql"

	// Try --stdin-from-command first; the operand spec is:
	//   restic backup --tag <kind> --tag <id> --host vps --stdin-from-command \
	//     --stdin-filename <id>-db.sql -- docker exec -e PGPASSWORD=... \
	//     <container> pg_dump --clean -U <user> <db>
	// On restic >= 0.16 this is one process; the dump never touches disk
	// AND restic captures the pg_dump exit code (so a half-dump fails
	// the snapshot rather than silently committing a truncated file).
	if resticSupportsStdinFromCommand(ctx, cfg) {
		args := []string{
			"backup",
			"--tag", kind,
			"--tag", recipe.ID,
			"--host", backupHostTag,
			"--quiet",
			"--stdin-from-command",
			"--stdin-filename", dumpFilename,
			"--",
			"docker", "exec",
			"-e", "PGPASSWORD=" + pwd,
			recipe.PGDump.Container,
			"pg_dump", "--clean", "-U", recipe.PGDump.User, recipe.PGDump.DB,
		}
		cmd := resticCmd(ctx, cfg, args...)
		return streamCmd(cmd, em)
	}

	// Fallback for older restic: pipe pg_dump → restic backup --stdin.
	dumpCmd := exec.CommandContext(ctx, "docker", "exec",
		"-e", "PGPASSWORD="+pwd,
		recipe.PGDump.Container,
		"pg_dump", "--clean", "-U", recipe.PGDump.User, recipe.PGDump.DB)
	dumpCmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}

	resticArgs := []string{
		"backup",
		"--tag", kind,
		"--tag", recipe.ID,
		"--host", backupHostTag,
		"--quiet",
		"--stdin",
		"--stdin-filename", dumpFilename,
	}
	resticBackup := resticCmd(ctx, cfg, resticArgs...)

	stdout, err := dumpCmd.StdoutPipe()
	if err != nil {
		return err
	}
	resticBackup.Stdin = stdout
	if err := resticBackup.Start(); err != nil {
		return err
	}
	if err := dumpCmd.Run(); err != nil {
		_ = resticBackup.Process.Kill()
		_, _ = resticBackup.Process.Wait()
		return fmt.Errorf("pg_dump exited: %w", err)
	}
	return resticBackup.Wait()
}

// resticSupportsStdinFromCommand probes restic at most once and caches
// the result. Strategy: run `restic backup --help` and look for the
// "--stdin-from-command" string. Fast, no network, no docker.
var (
	resticStdinFromCommandOnce sync.Once
	resticStdinFromCommandOK   bool
)

func resticSupportsStdinFromCommand(ctx context.Context, cfg config) bool {
	resticStdinFromCommandOnce.Do(func() {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(probeCtx, cfg.backupResticBin, "backup", "--help")
		cmd.Env = resticEnv(cfg)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// Tolerate failure: fall back to --stdin path below.
			return
		}
		resticStdinFromCommandOK = strings.Contains(string(out), "--stdin-from-command")
	})
	return resticStdinFromCommandOK
}

// composeServiceCmd runs `docker compose -f <stack>/docker-compose.yml
// <verb> <services...>`. Used for the brief stop/start window during
// authelia backups + during restores.
func composeServiceCmd(ctx context.Context, recipe *backupRecipe, verb string, em sseEmitter) error {
	if len(recipe.StopServices) == 0 {
		return nil
	}
	composeFile := filepath.Join("/opt/stacks", recipe.ID, "docker-compose.yml")
	args := []string{"compose", "-f", composeFile, verb}
	args = append(args, recipe.StopServices...)
	em.Log(fmt.Sprintf("$ docker %s", strings.Join(args, " ")))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/root"}
	return streamCmd(cmd, em)
}

// streamCmd starts a command, fan-outing stdout+stderr to em.Log, and
// blocks on cmd.Wait(). Mirrors runStreaming in setup.go but talks to
// our sseEmitter instead of a raw logLine func.
func streamCmd(cmd *exec.Cmd, em sseEmitter) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	pump := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		sc := bufio.NewScanner(r)
		// Bigger buffer than the default 64 KiB; restic's status lines
		// are short but pg_dump can emit very long DDL on COPY blocks.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			em.Log(sc.Text())
		}
	}
	go pump(stdout)
	go pump(stderr)
	<-done
	<-done
	return cmd.Wait()
}

// ---------------------------------------------------------------------------
// restore pipeline

// runRestore restores recipe to the given snapshot short id. Pipeline:
//
//  1. take a `pre_restore` snapshot (via runBackup — same code path).
//  2. docker compose stop <recipe.StopServices>
//  3. restic restore <snap> --target / --include <path1> --include ...
//  4. (optional) restic dump <snap> /<id>-db.sql | docker exec -i -e
//     PGPASSWORD=... <container> psql -U <user> <db>
//  5. docker compose start <recipe.StopServices>
//
// Recipes with Restorable=false (today: enrol) refuse before step 1.
func runRestore(ctx context.Context, cfg config, recipe *backupRecipe, snapshotID string, em sseEmitter, auditPath, actor string) error {
	if recipe == nil {
		return errors.New("runRestore: nil recipe")
	}
	if !recipe.Restorable {
		return fmt.Errorf("recipe %s is not restorable (see ADR-010)", recipe.ID)
	}
	step := recipe.ID + ":restore"

	// 1. Pre-restore safety snapshot (same engine, different tag).
	em.Status(step, "taking pre-restore safety snapshot")
	if err := runBackup(ctx, cfg, recipe, "pre_restore", em); err != nil {
		return fmt.Errorf("pre-restore snapshot: %w", err)
	}
	writeAudit(auditPath, auditEntry{
		Action: "backup.restore.rollback", Actor: actor, Target: recipe.ID,
		Result: "ok", Note: "auto pre-restore snapshot before restore of " + snapshotID,
	})

	// 2. Stop services (best-effort restart in defer).
	if len(recipe.StopServices) > 0 {
		em.Status(step, "stopping services")
		if err := composeServiceCmd(ctx, recipe, "stop", em); err != nil {
			return fmt.Errorf("stop services: %w", err)
		}
		defer func() {
			if startErr := composeServiceCmd(context.Background(), recipe, "start", em); startErr != nil {
				em.Log(fmt.Sprintf("warning: failed to restart %v: %v", recipe.StopServices, startErr))
			}
		}()
	}

	// 3. Restore files. We pass --include <path> for every recipe path
	// rather than --target <some-staging-dir> so the restore overwrites
	// the live mount in place — same shape the backup captured.
	if len(recipe.Paths) > 0 {
		em.Status(step, "restic restore (files)")
		args := []string{"restore", snapshotID, "--target", "/"}
		for _, p := range recipe.Paths {
			args = append(args, "--include", p)
		}
		cmd := resticCmd(ctx, cfg, args...)
		if err := streamCmd(cmd, em); err != nil {
			return fmt.Errorf("restic restore: %w", err)
		}
	}

	// 4. Optional pg_dump restore: stream `restic dump <snap> /<id>-db.sql`
	// straight into `psql`. The dump was created with --clean so DROP+CREATE
	// happens before the data load → idempotent against a non-empty DB.
	if recipe.PGDump != nil {
		em.Status(step, "restic dump | psql (Postgres)")
		if err := runPGDumpRestore(ctx, cfg, recipe, snapshotID, em); err != nil {
			return fmt.Errorf("psql restore: %w", err)
		}
	}

	em.Status(step, "restore complete")
	return nil
}

// runPGDumpRestore pipes `restic dump <snap> /<id>-db.sql` into
// `docker exec -i ... psql`. No tempfile.
func runPGDumpRestore(ctx context.Context, cfg config, recipe *backupRecipe, snapshotID string, em sseEmitter) error {
	pwd, err := readPostgresPassword(recipe.PGDump.EnvFile)
	if err != nil {
		return fmt.Errorf("read pg password: %w", err)
	}
	if pwd == "" {
		return fmt.Errorf("POSTGRES_PASSWORD not found in %s", recipe.PGDump.EnvFile)
	}
	dumpPath := "/" + recipe.ID + "-db.sql"

	dumpCmd := resticCmd(ctx, cfg, "dump", snapshotID, dumpPath)
	psqlCmd := exec.CommandContext(ctx, "docker", "exec", "-i",
		"-e", "PGPASSWORD="+pwd,
		recipe.PGDump.Container,
		"psql", "-U", recipe.PGDump.User, "-d", recipe.PGDump.DB,
		"--quiet", "--single-transaction",
		"--set", "ON_ERROR_STOP=1",
	)
	psqlCmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}

	stdout, err := dumpCmd.StdoutPipe()
	if err != nil {
		return err
	}
	psqlCmd.Stdin = stdout
	em.Log(fmt.Sprintf("$ restic dump %s %s | docker exec -i %s psql -U %s -d %s",
		snapshotID, dumpPath, recipe.PGDump.Container, recipe.PGDump.User, recipe.PGDump.DB))
	if err := psqlCmd.Start(); err != nil {
		return err
	}
	if err := dumpCmd.Run(); err != nil {
		_ = psqlCmd.Process.Kill()
		_, _ = psqlCmd.Process.Wait()
		return fmt.Errorf("restic dump exited: %w", err)
	}
	return psqlCmd.Wait()
}

// ---------------------------------------------------------------------------
// orchestrators (used by HTTP and CLI)

// runBackupAll snapshots every recipe sequentially; collects per-stack
// errors but does NOT bail at the first failure (so a single broken
// stack doesn't stop the others from being captured). After all stacks,
// runs `restic forget` per-tag to honour the retention rules. Returns
// a non-nil error if ANY per-stack op failed.
func runBackupAll(ctx context.Context, cfg config, kind string, em sseEmitter, auditPath, actor string) error {
	em.Status("all:"+kind, fmt.Sprintf("starting %s backup of all stacks", kind))
	var failed []string
	for i := range backupRecipes {
		recipe := &backupRecipes[i]
		if err := runBackup(ctx, cfg, recipe, kind, em); err != nil {
			em.Log(fmt.Sprintf("FAIL %s: %v", recipe.ID, err))
			writeAudit(auditPath, auditEntry{
				Action: auditActionForKind(kind), Actor: actor, Target: recipe.ID,
				Result: "fail", Note: err.Error(),
			})
			failed = append(failed, recipe.ID)
			continue
		}
		writeAudit(auditPath, auditEntry{
			Action: auditActionForKind(kind), Actor: actor, Target: recipe.ID,
			Result: "ok", Note: "tag=" + kind,
		})
	}

	// Retention pass after every batch (only meaningful for "daily" today;
	// manual retention is best-effort, pre_restore is owned by the restore
	// path itself).
	if kind == "daily" {
		if err := runRetention(ctx, cfg, kind, cfg.backupRetentionDaily, em, auditPath, actor); err != nil {
			em.Log(fmt.Sprintf("retention pass FAILED: %v", err))
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("backup completed with failures: %s", strings.Join(failed, ", "))
	}
	em.Status("all:"+kind, "all stacks backed up successfully")
	return nil
}

// runRetention forgets old snapshots beyond the keep count for the given
// tag. Iterates per recipe so each stack's history is retained
// independently (prevents a single noisy stack from bumping a quiet one
// out of the keep window).
func runRetention(ctx context.Context, cfg config, kind string, keep int, em sseEmitter, auditPath, actor string) error {
	for i := range backupRecipes {
		recipe := &backupRecipes[i]
		snaps, err := listSnapshotsForRecipeAndKind(ctx, cfg, recipe.ID, kind)
		if err != nil {
			em.Log(fmt.Sprintf("retention: list %s/%s: %v", recipe.ID, kind, err))
			continue
		}
		toDrop := snapshotsToForget(snaps, keep)
		if len(toDrop) == 0 {
			continue
		}
		em.Log(fmt.Sprintf("retention: forgetting %d %s snapshots for %s", len(toDrop), kind, recipe.ID))
		args := append([]string{"forget", "--prune"}, toDrop...)
		cmd := resticCmd(ctx, cfg, args...)
		if err := streamCmd(cmd, em); err != nil {
			em.Log(fmt.Sprintf("retention: forget failed: %v", err))
			continue
		}
		for _, id := range toDrop {
			writeAudit(auditPath, auditEntry{
				Action: "backup.forget", Actor: actor, Target: recipe.ID,
				Result: "ok", Note: "snapshot=" + shortenID(id) + " tag=" + kind,
			})
		}
	}
	return nil
}

// listSnapshotsForRecipeAndKind returns snapshots that carry BOTH the
// recipe id AND the kind tag. restic's --tag flag is OR within one --tag
// arg and AND across multiple --tag args; we want the AND.
func listSnapshotsForRecipeAndKind(ctx context.Context, cfg config, recipeID, kind string) ([]resticSnapshot, error) {
	cmd := resticCmd(ctx, cfg, "snapshots", "--tag", recipeID, "--tag", kind, "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(string(out))
	if body == "" || body == "null" {
		return nil, nil
	}
	var snaps []resticSnapshot
	if err := json.Unmarshal([]byte(body), &snaps); err != nil {
		return nil, err
	}
	return snaps, nil
}

func shortenID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// auditActionForKind maps a run-kind tag to the audit action name.
func auditActionForKind(kind string) string {
	if kind == "daily" {
		return "backup.scheduled"
	}
	return "backup.create"
}

// ---------------------------------------------------------------------------
// HTTP handlers

// backupTemplateData is the wrapper passed to backup.html. The template
// references the page-data via `{{.Backup.RepoSize}}` etc — matching the
// "viewerData has a *BackupPageView field" contract documented in
// plans/hazy-hugging-wave.md (no global viewerData type exists in this
// codebase; each handler defines its own wrapper, but the template's
// `.Backup.<field>` access is preserved by this local shim).
type backupTemplateData struct {
	Title         string
	User          string
	Flash         string
	CSRF          string
	ViewerIsAdmin bool
	Backup        *BackupPageView
}

func (s *server) handleBackupIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	csrf := ensureCSRF(w, r)
	expand := strings.TrimSpace(r.URL.Query().Get("expand"))

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	pv := s.buildBackupPageView(ctx, expand, csrf)

	data := backupTemplateData{
		Title:         "backups",
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		ViewerIsAdmin: true, // requireAdmin gates this route
		Backup:        &pv,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "backup.html", data); err != nil {
		log.Printf("template backup.html: %v", err)
	}
}

// buildBackupPageView assembles the page-data DTO for /backup. Every
// restic call is best-effort — a missing repo (pre-bootstrap) renders
// the page with empty rows rather than 500-ing.
func (s *server) buildBackupPageView(ctx context.Context, expandID, csrf string) BackupPageView {
	pv := BackupPageView{CSRF: csrf}

	// Per-recipe rows.
	rows := make([]BackupRecipeView, 0, len(backupRecipes))
	for i := range backupRecipes {
		recipe := &backupRecipes[i]
		row := BackupRecipeView{
			ID:               recipe.ID,
			Display:          recipe.Display,
			Restorable:       recipe.Restorable,
			LastSnapshotAt:   "—",
			LastSnapshotSize: "—",
		}
		snaps, err := listSnapshotsForTag(ctx, s.cfg, recipe.ID)
		if err == nil && len(snaps) > 0 {
			// Newest first.
			sort.Slice(snaps, func(i, j int) bool {
				return snaps[i].Time.After(snaps[j].Time)
			})
			latest := snaps[0]
			kind := pickKindTag(latest.Tags, recipe.ID)
			row.LastSnapshotAt = latest.Time.UTC().Format("2006-01-02 15:04") + " (" + kind + ")"
			if latest.Summary != nil {
				row.LastSnapshotSize = formatBytes(latest.Summary.TotalBytesProcessed)
			}
			if recipe.ID == expandID {
				view := make([]BackupSnapshotView, 0, len(snaps))
				for _, s := range snaps {
					size := "—"
					if s.Summary != nil {
						size = formatBytes(s.Summary.TotalBytesProcessed)
					}
					view = append(view, BackupSnapshotView{
						ID:        s.shortID(),
						CreatedAt: s.Time.UTC().Format("2006-01-02 15:04 UTC"),
						Size:      size,
						Tags:      s.Tags,
					})
				}
				row.Snapshots = view
			}
		}
		rows = append(rows, row)
	}
	pv.Recipes = rows

	pv.RepoSize, pv.RepoSnapCount = repoStats(ctx, s.cfg)
	pv.NextScheduled = nextScheduledRun(ctx)
	pv.OffHostHelp = renderOffHostHelp(s.cfg)
	if op := globalOpTracker.current(); op != nil {
		pv.InProgress = op.ID
	}
	return pv
}

// pickKindTag returns the first tag in `tags` that isn't the recipe id —
// i.e. the "kind" tag (daily/manual/pre_restore). Falls back to "—".
func pickKindTag(tags []string, recipeID string) string {
	for _, t := range tags {
		if t == recipeID {
			continue
		}
		return t
	}
	return "—"
}

// nextScheduledRun shells out to `systemctl list-timers raph-backup.timer
// --no-pager --output=json` and returns the next-fire time. Empty string
// on any error (timer not yet installed, systemctl absent inside the
// container, etc).
func nextScheduledRun(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "systemctl", "list-timers", "raph-backup.timer",
		"--no-pager", "--output=json")
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return "—"
	}
	var rows []struct {
		Next string `json:"next"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return "—"
	}
	if rows[0].Next == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, rows[0].Next)
	if err != nil {
		return rows[0].Next
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// renderOffHostHelp pre-renders the three off-host pull/restore commands
// the operator copies into a remote shell. Hostnames + repo paths are
// substituted so there's no copy-paste error from a hand-edited example.
func renderOffHostHelp(cfg config) OffHostCommandsView {
	host := "root@" + cfg.domain
	return OffHostCommandsView{
		RsyncCmd: fmt.Sprintf(
			"rsync -av --delete %s:%s/ ./local-backup-mirror/",
			host, cfg.backupRepoDir),
		ResticCopyCmd: fmt.Sprintf(
			"restic -r sftp:%s:%s copy --from-password-file ~/.raph-restic-password --to-repo /tmp/local-mirror",
			host, cfg.backupRepoDir),
		ResticRestoreCmd: "restic -r ./local-backup-mirror restore <snapshot-id> --target /tmp/restored --tag <stack>",
	}
}

// handleBackupCreate kicks off a backup of one stack (or all) and
// redirects to /backup with the in-flight op id so the SSE handler
// can resume tailing.
func (s *server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	stack := strings.TrimSpace(r.Form.Get("stack"))
	if stack == "" {
		http.Error(w, "missing stack", http.StatusBadRequest)
		return
	}
	if stack != "all" && recipeByID(stack) == nil {
		http.Error(w, "unknown stack: "+stack, http.StatusBadRequest)
		return
	}
	release, err := tryAcquire()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")
	op := newOp(stack, "create")
	globalOpTracker.register(op)

	go func() {
		defer release()
		// Detached context — the SSE consumer's context is per-connection,
		// but the backup outlives any single browser tab. Bound only by
		// runBackupAll's internal restic timeouts (none today; restic itself
		// is single-process and well-behaved).
		ctx := context.Background()
		em := opEmitter(op)
		var err error
		if stack == "all" {
			err = runBackupAll(ctx, s.cfg, "manual", em, auditPath, actor)
		} else {
			recipe := recipeByID(stack)
			err = runBackup(ctx, s.cfg, recipe, "manual", em)
			if err == nil {
				writeAudit(auditPath, auditEntry{
					Action: "backup.create", Actor: actor, Target: recipe.ID,
					Result: "ok", Note: "tag=manual",
				})
			} else {
				writeAudit(auditPath, auditEntry{
					Action: "backup.create", Actor: actor, Target: recipe.ID,
					Result: "fail", Note: err.Error(),
				})
			}
		}
		if err != nil {
			em.Error(op.Stack+":create", err.Error())
			op.finish(true)
		} else {
			em.Done(`{"ok":true}`)
			op.finish(false)
		}
	}()

	http.Redirect(w, r, "/backup?op="+op.ID, http.StatusSeeOther)
}

// handleBackupRestore restores a stack from a specific snapshot. Guarded
// by a typed-confirmation form field that must equal the stack id under
// constant-time compare; only restorable recipes are accepted.
func (s *server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	stack := strings.TrimSpace(r.Form.Get("stack"))
	snapshotID := strings.TrimSpace(r.Form.Get("snapshot"))
	confirm := r.Form.Get("confirm")
	if stack == "" || snapshotID == "" {
		http.Error(w, "missing stack or snapshot", http.StatusBadRequest)
		return
	}
	recipe := recipeByID(stack)
	if recipe == nil {
		http.Error(w, "unknown stack: "+stack, http.StatusBadRequest)
		return
	}
	if !recipe.Restorable {
		http.Error(w, "recipe is not restorable", http.StatusBadRequest)
		return
	}
	// Constant-time compare: even a 1-bit difference would log "wrong",
	// but we go through the formality so timing oracles can't probe the
	// recipe id char-by-char. (No attacker model that needs this exists,
	// but the cost is one library call.)
	if subtle.ConstantTimeCompare([]byte(confirm), []byte(stack)) != 1 {
		http.Error(w, "confirmation token mismatch", http.StatusBadRequest)
		return
	}
	release, err := tryAcquire()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")
	op := newOp(stack, "restore")
	globalOpTracker.register(op)

	go func() {
		defer release()
		ctx := context.Background()
		em := opEmitter(op)
		err := runRestore(ctx, s.cfg, recipe, snapshotID, em, auditPath, actor)
		if err != nil {
			writeAudit(auditPath, auditEntry{
				Action: "backup.restore", Actor: actor, Target: recipe.ID,
				Result: "fail", Note: "snapshot=" + snapshotID + " err=" + err.Error(),
			})
			em.Error(op.Stack+":restore", err.Error())
			op.finish(true)
			return
		}
		writeAudit(auditPath, auditEntry{
			Action: "backup.restore", Actor: actor, Target: recipe.ID,
			Result: "ok", Note: "snapshot=" + snapshotID,
		})
		em.Done(`{"ok":true}`)
		op.finish(false)
	}()

	http.Redirect(w, r, "/backup?op="+op.ID, http.StatusSeeOther)
}

// handleBackupForget removes one snapshot by short id. Useful for
// reclaiming space or evicting a known-bad snapshot.
func (s *server) handleBackupForget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	snapshotID := strings.TrimSpace(r.Form.Get("snapshot"))
	if snapshotID == "" {
		http.Error(w, "missing snapshot", http.StatusBadRequest)
		return
	}
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cmd := resticCmd(ctx, s.cfg, "forget", "--prune", snapshotID)
	if out, err := cmd.CombinedOutput(); err != nil {
		writeAudit(auditPath, auditEntry{
			Action: "backup.forget", Actor: actor, Target: snapshotID,
			Result: "fail", Note: err.Error() + ": " + string(out),
		})
		http.Error(w, "forget: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{
		Action: "backup.forget", Actor: actor, Target: snapshotID,
		Result: "ok",
	})
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// handleBackupEvents streams the buffered + live event log for one op.
// SSE format identical to the wizard's /setup/events (status / log /
// error / done frames; periodic keepalive comments).
func (s *server) handleBackupEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opID := strings.TrimSpace(r.URL.Query().Get("op"))
	if opID == "" {
		http.Error(w, "missing op", http.StatusBadRequest)
		return
	}
	op := globalOpTracker.get(opID)
	if op == nil {
		http.Error(w, "unknown or expired op", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}

	var writeMu sync.Mutex
	writeFrame := func(b []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = w.Write(b)
		flusher.Flush()
	}

	// Replay buffered history first.
	hist, ch := op.snapshot()
	defer op.unsubscribe(ch)
	for _, fr := range hist {
		writeFrame([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", fr.Event, fr.Payload)))
	}
	if op.Done {
		// History already includes the terminal frame; nothing to tail.
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			writeMu.Lock()
			_, err := io.WriteString(w, ": keepalive\n\n")
			if err == nil {
				flusher.Flush()
			}
			writeMu.Unlock()
			if err != nil {
				return
			}
		case fr, ok := <-ch:
			if !ok {
				return
			}
			writeFrame([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", fr.Event, fr.Payload)))
			if fr.Event == "done" || fr.Event == "error" {
				return
			}
		}
	}
}

// newOp builds an op with a random 8-hex suffix in the id.
func newOp(stack, verb string) *backupOp {
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	return &backupOp{
		ID:        stack + ":" + verb + ":" + suffix,
		Stack:     stack,
		Verb:      verb,
		StartedAt: time.Now().UTC(),
	}
}

// opEmitter returns an sseEmitter that writes into the op's buffer.
// The HTTP SSE handler then replays the buffer + tails new events.
func opEmitter(op *backupOp) sseEmitter {
	writeFrame := func(b []byte) {
		// Parse the event/payload out of the standard `event: ...\ndata: ...\n\n`
		// frame so we can store the structured pair (the same shape the
		// SSE handler then re-serialises). Cheap; the frame is one line of
		// each anyway.
		s := string(b)
		event, payload := splitFrame(s)
		op.append(event, payload)
	}
	return newSSEEmitter(writeFrame)
}

// splitFrame extracts (event, payload) from the canonical SSE frame
// produced by sseEmitter. Returns ("", payload) if no event line.
func splitFrame(frame string) (event, payload string) {
	frame = strings.TrimSuffix(frame, "\n\n")
	for _, line := range strings.Split(frame, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload = strings.TrimPrefix(line, "data: ")
		}
	}
	return event, payload
}

// ---------------------------------------------------------------------------
// CLI subcommand: `enrol backup [--scheduled] [--tag=...] [--stack=...]`

// runBackupCLI is the entrypoint when main() observes os.Args[1]=="backup".
// Loads config from env, parses flags, invokes runBackupAll (or
// runBackup for a single --stack) synchronously, and exits 0/1.
//
// Output is line-buffered to stdout with a `[backup]` prefix so journald
// and the operator's terminal both see structured progress. The same
// audit log entries written by the UI path are written here too, with
// Actor=`system` for scheduled runs.
func runBackupCLI(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	scheduled := fs.Bool("scheduled", false, "marks this as a scheduled (timer) run; sets Actor=system")
	tag := fs.String("tag", "manual", "snapshot kind tag: daily | manual | pre_restore")
	stack := fs.String("stack", "", "single stack id (cloud/task/...); empty = all")
	repo := fs.String("repo", "", "override $ENROL_BACKUP_REPO_DIR")
	pwFile := fs.String("password-file", "", "override $ENROL_BACKUP_PASSWORD_FILE")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"usage: enrol backup [--scheduled] [--tag=daily|manual|pre_restore] [--stack=<id>] [--repo=<path>] [--password-file=<path>]\n\n"+
				"Snapshots all stacks (or one --stack) into the restic repo at ENROL_BACKUP_REPO_DIR.\n"+
				"Invoked nightly by host/systemd/raph-backup.timer with --scheduled --tag=daily.\n")
	}
	if err := fs.Parse(args); err != nil {
		// flag.ExitOnError already exited; defensive return.
		return
	}

	cfg := loadConfig()
	if *repo != "" {
		cfg.backupRepoDir = *repo
	}
	if *pwFile != "" {
		cfg.backupPasswordFile = *pwFile
	}

	actor := "system"
	if !*scheduled {
		// Logged-in operator running the binary by hand inside the
		// container. We have no Authelia header here; record the OS
		// username so the audit row is at least attributable.
		if u := os.Getenv("USER"); u != "" {
			actor = "cli:" + u
		} else {
			actor = "cli"
		}
	}
	auditPath := filepath.Join(cfg.awgDir, "peers-audit.log")

	// Stdout-bound emitter: each SSE-shaped frame becomes a `[backup]`
	// line. Reuse the same sseEmitter struct so the engine code is
	// agnostic to "browser stream" vs "stdout".
	em := newSSEEmitter(func(b []byte) {
		event, payload := splitFrame(string(b))
		fmt.Printf("[backup] %s %s\n", event, payload)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runErr error
	if *stack != "" {
		recipe := recipeByID(*stack)
		if recipe == nil {
			fmt.Fprintf(os.Stderr, "unknown stack: %s\n", *stack)
			os.Exit(2)
		}
		runErr = runBackup(ctx, cfg, recipe, *tag, em)
		if runErr != nil {
			writeAudit(auditPath, auditEntry{
				Action: auditActionForKind(*tag), Actor: actor, Target: recipe.ID,
				Result: "fail", Note: runErr.Error(),
			})
		} else {
			writeAudit(auditPath, auditEntry{
				Action: auditActionForKind(*tag), Actor: actor, Target: recipe.ID,
				Result: "ok", Note: "tag=" + *tag,
			})
		}
		if *tag == "daily" {
			_ = runRetention(ctx, cfg, *tag, cfg.backupRetentionDaily, em, auditPath, actor)
		}
	} else {
		runErr = runBackupAll(ctx, cfg, *tag, em, auditPath, actor)
	}

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "backup failed: %v\n", runErr)
		os.Exit(1)
	}
}
