// task_client.go — read-only "client" for the task stack (Vikunja).
//
// Vikunja's REST API is per-user authenticated and lacks an admin
// "list-all-users / show-tasks-of-user-X" endpoint suitable for the admin
// /users page. Rather than provisioning a privileged service-account JWT
// inside Vikunja and rewriting the storage page around per-call auth,
// we read the same data straight from Vikunja's Postgres via
// `docker exec task-db psql`. This mirrors the cloud integration which
// uses `docker exec cloud occ` for the same reason: enrol already has the
// docker socket and the task stack already exposes its DB to admin
// inspection by virtue of being on the same host.
//
// Pattern:
//   - TaskClient holds the docker container name + db credentials path.
//   - UserStats(email) returns (taskCount, attachmentBytes, lastActivity).
//   - Silent-fallback on every failure mode (no container, no DB, no user,
//     password missing, query timeout) → zero values + nil error. The admin
//     /users page renders "—" for any zero value.
//
// Why "task" everywhere despite the upstream being Vikunja: ADR-002
// camouflage. External + internal naming is unified at "task".
//
// Threat model: enrol already has docker socket access (root-equivalent
// on host, see stacks/enrol/docker-compose.yml banner). Reading
// /opt/stacks/task/.env for the DB password adds zero new attack
// surface. The psql invocation is parameterised via PG environment
// variables (no shell-quoted SQL), so user-controlled email values
// cannot escape into the query string.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// types

// TaskClient queries Vikunja's Postgres via docker exec. Zero value is
// not usable — call NewTaskClient.
type TaskClient struct {
	dbContainer string // e.g. "task-db"
	dbName      string // e.g. "vikunja"
	dbUser      string // e.g. "vikunja"
	envFile     string // path to /opt/stacks/task/.env (POSTGRES_PASSWORD lives here)
	timeout     time.Duration

	// password is loaded lazily on first call and cached; the .env file
	// is read every time but the file IO is cheap and avoids stale-cache
	// surprises if the operator rotates the secret + restarts the stack
	// without restarting enrol.
	mu          sync.Mutex
	cachedPwd   string
	cachedPwdAt time.Time
}

// NewTaskClient constructs a client targeting the named container +
// DB. envFile must contain `POSTGRES_PASSWORD=...` (the same file
// docker-compose loads). Pass empty values to disable the client; every
// method then short-circuits to zero values.
//
// Note the call signature has changed from Wave B (which took baseURL +
// bearer-token) — Vikunja replaced Plane and the integration moved to
// docker-exec + psql. Callers in main.go need updating accordingly.
func NewTaskClient(dbContainer, dbName, dbUser, envFile string) *TaskClient {
	envFile = strings.TrimSpace(envFile)
	if envFile != "" {
		// filepath.Clean("") returns "." — surface empty as empty so
		// ready() can treat it correctly.
		envFile = filepath.Clean(envFile)
	}
	return &TaskClient{
		dbContainer: strings.TrimSpace(dbContainer),
		dbName:      strings.TrimSpace(dbName),
		dbUser:      strings.TrimSpace(dbUser),
		envFile:     envFile,
		timeout:     6 * time.Second,
	}
}

// ready reports whether enough is configured to issue a query.
func (c *TaskClient) ready() bool {
	if c == nil {
		return false
	}
	return c.dbContainer != "" && c.dbName != "" && c.dbUser != "" && c.envFile != ""
}

// ---------------------------------------------------------------------------
// password loader

// passwordTTL is how long we cache the .env password before re-reading.
// The file is small (<1 KB) and on local disk; 5 minutes balances "operator
// rotated and restarted plane" against avoiding pointless re-reads under
// dashboard refresh-spam.
const passwordTTL = 5 * time.Minute

// password returns POSTGRES_PASSWORD from the env file, "" on any error.
// Errors are deliberately swallowed to preserve the silent-fallback
// contract — the admin page degrades to zeros rather than 500-ing when
// the env file is missing or the key isn't present yet (e.g. the plane
// stack hasn't been deployed on this host).
func (c *TaskClient) password() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedPwd != "" && time.Since(c.cachedPwdAt) < passwordTTL {
		return c.cachedPwd
	}
	f, err := os.Open(c.envFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "POSTGRES_PASSWORD=") {
			continue
		}
		val := strings.TrimPrefix(line, "POSTGRES_PASSWORD=")
		// .env files in this repo sometimes single- or double-quote
		// values; strip a single matched pair if present.
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '\'' && val[len(val)-1] == '\'') ||
				(val[0] == '"' && val[len(val)-1] == '"') {
				val = val[1 : len(val)-1]
			}
		}
		c.cachedPwd = val
		c.cachedPwdAt = time.Now()
		return val
	}
	return ""
}

// ---------------------------------------------------------------------------
// API

// TaskUserStats is the per-user roll-up the admin /users page renders
// for the task (Vikunja) columns. Zero values are surface-level: caller
// already knows the user from Authelia, this just decorates them.
type TaskUserStats struct {
	Tasks      int       // tasks created by the user
	AttBytes   int64     // sum of file sizes for attachments owned by user
	LastSeen   time.Time // max(users.updated, max(tasks.updated)) — best-effort
	UserExists bool      // false if the email isn't in Vikunja's users table yet
}

// UserStats runs one psql query joining users → tasks → attachments → files
// and returns the per-user roll-up. Zero values and nil error on any
// shell-out / parse / DB failure (silent fallback — admin page renders "—").
//
// Why psql and not the REST API: Vikunja's /users endpoint is search-only
// and the /tasks endpoint requires the calling user's JWT. There is no
// "admin: list this user's tasks" endpoint in v2.3.0. Querying the DB
// directly is the path enrol already takes for cloud (`docker exec cloud
// occ`); applying it here keeps the integration uniform and avoids
// minting a service-account JWT in Vikunja that we'd have to rotate.
func (c *TaskClient) UserStats(ctx context.Context, email string) (TaskUserStats, error) {
	if !c.ready() || strings.TrimSpace(email) == "" {
		return TaskUserStats{}, nil
	}
	pwd := c.password()
	if pwd == "" {
		return TaskUserStats{}, nil
	}

	// Local timeout if caller didn't bound the context; psql can hang if
	// the container is mid-restart.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	// Single query, four columns. The email comes in via a psql variable
	// (-v) and is interpolated as a quoted SQL literal (:'email_arg'),
	// so the user-controlled value never reaches the shell. Reduces
	// SQL-injection surface to "psql variable handling", which is well-
	// audited.
	const sqlQuery = `
SELECT COALESCE(t.cnt, 0) AS task_count,
       COALESCE(a.bytes, 0) AS att_bytes,
       COALESCE(GREATEST(u.updated, t.last_task), u.updated) AS last_seen,
       1 AS user_exists
FROM users u
LEFT JOIN (
  SELECT created_by_id, COUNT(*) AS cnt, MAX(updated) AS last_task
  FROM tasks
  GROUP BY created_by_id
) t ON t.created_by_id = u.id
LEFT JOIN (
  SELECT ta.created_by_id, COALESCE(SUM(f.size), 0) AS bytes
  FROM task_attachments ta
  JOIN files f ON f.id = ta.file_id
  GROUP BY ta.created_by_id
) a ON a.created_by_id = u.id
WHERE LOWER(u.email) = LOWER(:'email_arg')
LIMIT 1;
`
	cmd := exec.CommandContext(ctx,
		"docker", "exec",
		"-e", "PGPASSWORD="+pwd,
		"-e", "PGOPTIONS=-c statement_timeout=5000",
		c.dbContainer,
		"psql",
		"-U", c.dbUser,
		"-d", c.dbName,
		"-A", "-t", "-F", "|",
		"--no-psqlrc",
		"-v", "email_arg="+email,
		"-c", sqlQuery,
	)
	// Inherit nothing — explicit empty env keeps surprise PATH lookups
	// out of the picture; docker is absolute-resolved by exec.LookPath
	// because cmd.Path is set on construction.
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		// Common transient: container not running, password rotated,
		// network blip. Silent fallback per banner.
		return TaskUserStats{}, nil
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		// Email isn't in Vikunja's users table — user has never logged
		// in via OIDC. Distinct from "we got a row of zeros".
		return TaskUserStats{UserExists: false}, nil
	}
	fields := strings.Split(line, "|")
	if len(fields) < 4 {
		return TaskUserStats{}, nil
	}
	stats := TaskUserStats{UserExists: true}
	if v, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
		stats.Tasks = v
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64); err == nil {
		stats.AttBytes = v
	}
	// Postgres timestamp default format from psql -A -t: "2006-01-02 15:04:05.999999"
	tsStr := strings.TrimSpace(fields[2])
	if tsStr != "" {
		// Try the most common Postgres timestamp formats.
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999",
			"2006-01-02 15:04:05",
			time.RFC3339,
		} {
			if t, err := time.Parse(layout, tsStr); err == nil {
				stats.LastSeen = t
				break
			}
		}
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// shims preserving the old call surface
//
// storage.go's `taskUserInfo` helper expects the old method names. To
// keep this PR's diff small and avoid coupling the rewrite to a storage.go
// rewrite in the same commit, the old names below now thin-shim onto
// UserStats. They will be removed in a follow-up once storage.go drops
// the per-workspace fan-out.

// errTaskUnconfigured is kept as a sentinel for tests that previously
// matched on error type. Plane integration now silently fails, so this is
// only ever returned from explicitly-invalidated test cases.
var errTaskUnconfigured = errors.New("task: not configured (Vikunja stack down or DB unreachable)")

// stub vestige of the old REST-shaped client — kept so tests that wired
// "plane is not deployed" survive without touching every call site.
type Workspace struct {
	ID   string
	Name string
	Slug string
}

// TaskUser likewise, for the old `UserByEmail` shape. Only Email is
// load-bearing and the taskUserInfo helper short-circuits when this is
// returned with empty ID.
type TaskUser struct {
	ID         string
	Email      string
	LastActive time.Time
}

func (c *TaskClient) ListWorkspaces(_ context.Context) ([]Workspace, error) {
	// Vikunja has no notion of workspaces. Return a single synthetic
	// "all" workspace so storage.go's per-workspace loop runs once. The
	// Slug "_all" is a private sentinel used by IssueCount/FileAssetBytes
	// below to mean "ignore the workspace dimension".
	if !c.ready() {
		return nil, nil
	}
	return []Workspace{{Slug: "_all", Name: "All", ID: "_all"}}, nil
}

func (c *TaskClient) UserByEmail(ctx context.Context, email string) (*TaskUser, error) {
	if !c.ready() || strings.TrimSpace(email) == "" {
		return nil, nil
	}
	stats, err := c.UserStats(ctx, email)
	if err != nil || !stats.UserExists {
		return nil, nil
	}
	// We return the email itself as the ID so the per-workspace IssueCount
	// / FileAssetBytes calls below can re-derive the user. The real user-id
	// (bigint) lives only in the DB and isn't surfaced; that's fine because
	// the only caller is taskUserInfo which threads ID into IssueCount,
	// which we override below to ignore it.
	return &TaskUser{ID: email, Email: email, LastActive: stats.LastSeen}, nil
}

func (c *TaskClient) IssueCount(ctx context.Context, _wsSlug, userID string) (int, error) {
	if !c.ready() {
		return 0, nil
	}
	stats, err := c.UserStats(ctx, userID) // userID is actually the email — see UserByEmail above.
	if err != nil {
		return 0, nil
	}
	return stats.Tasks, nil
}

func (c *TaskClient) FileAssetBytes(ctx context.Context, _wsSlug, userID string) (int64, error) {
	if !c.ready() {
		return 0, nil
	}
	stats, err := c.UserStats(ctx, userID)
	if err != nil {
		return 0, nil
	}
	return stats.AttBytes, nil
}

func (c *TaskClient) LastActivity(ctx context.Context, userID string) (time.Time, error) {
	if !c.ready() {
		return time.Time{}, nil
	}
	stats, err := c.UserStats(ctx, userID)
	if err != nil {
		return time.Time{}, nil
	}
	return stats.LastSeen, nil
}

// ---------------------------------------------------------------------------
// helpers used by tests

// requireConfigured is exposed for test scaffolding that wants to assert
// the client is in "deployed mode" before issuing live queries.
func (c *TaskClient) requireConfigured() error {
	if c.ready() {
		return nil
	}
	return fmt.Errorf("%w: container=%q db=%q user=%q env=%q",
		errTaskUnconfigured, c.dbContainer, c.dbName, c.dbUser, c.envFile)
}
