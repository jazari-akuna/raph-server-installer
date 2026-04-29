// authelia_bans.go — admin UI hook into Authelia's regulator.
//
// Authelia stores rate-limit bans (after N failed 1FA attempts) in its
// `banned_user` table. The CLI (`authelia storage user`) does not expose
// a list/revoke verb as of 4.39, and the bans persist across restarts
// because the regulator queries this table on every login attempt. We
// reach into the same sqlite file enrol's compose already binds RW
// (/opt/stacks/authelia/data/db.sqlite3) and run the two queries we
// need: list active bans, revoke a single user's active bans.
//
// Bans are not destructive — `revoked=1` is a soft delete that keeps
// the audit trail. We never delete rows.

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// userBan is one active ban row from authelia.banned_user, normalised
// for display in /users.
type userBan struct {
	Username string
	Expires  time.Time // when the regulator would auto-clear it
	Source   string    // "regulation" (rate limit) — Authelia 4.39 uses only this source today
	Reason   string    // free-form, e.g. "Exceeding Maximum Retries"
}

// activeUserBans returns the set of currently-banned usernames →
// userBan record. Empty map (not nil) on success-but-no-bans. Returns
// (nil, err) only on I/O / sqlite invocation failure; an inaccessible
// db file or a missing sqlite binary degrades to "no bans visible"
// from the caller's perspective rather than 500-ing the /users page.
//
// "Active" means: not revoked, and (expires IS NULL OR expires > now).
// expired column is set by Authelia itself when the time-window passes;
// the lookup index banned_user_lookup_idx exists for this exact filter.
func activeUserBans(ctx context.Context, dbPath string) (map[string]userBan, error) {
	if dbPath == "" {
		return map[string]userBan{}, nil
	}
	// .mode line is the standard sqlite trick for unambiguous column
	// separation — we use a literal "|" since none of the fields can
	// legitimately contain it (username is [a-z0-9_], reason is a
	// fixed Authelia string, source is one of a handful of constants).
	const q = `.mode list
.separator |
SELECT username, COALESCE(expires, ''), source, COALESCE(reason, '')
FROM banned_user
WHERE revoked = 0
  AND expired IS NULL
  AND (expires IS NULL OR expires > datetime('now'));`
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", dbPath)
	cmd.Stdin = strings.NewReader(q)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sqlite3 banned_user: %w", err)
	}
	bans := map[string]userBan{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		exp, _ := parseAutheliaTime(parts[1])
		bans[parts[0]] = userBan{
			Username: parts[0],
			Expires:  exp,
			Source:   parts[2],
			Reason:   parts[3],
		}
	}
	return bans, nil
}

// revokeUserBans soft-deletes every active ban for one user. Used by
// the admin "Unban" button. Mirrors the manual SQL: revoked=1 and
// expired=now so the row drops out of the regulator's "active" filter
// immediately. Authelia's regulator re-queries on every login attempt
// (no in-process cache), so the unban takes effect on the user's next
// /api/firstfactor POST without an authelia restart. Idempotent: a
// no-op when nothing's active.
func revokeUserBans(ctx context.Context, dbPath, username string) error {
	if dbPath == "" {
		return fmt.Errorf("authelia storage db path not configured")
	}
	if username == "" {
		return fmt.Errorf("username required")
	}
	// Guardrails on the username — defensive even though the caller
	// passes the URL segment which the mux already constrained, since
	// this string flows into a SQL statement built as text.
	for _, c := range username {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
			return fmt.Errorf("invalid username for unban: %q", username)
		}
	}
	q := fmt.Sprintf(`UPDATE banned_user
SET revoked = 1, expired = CURRENT_TIMESTAMP
WHERE username = '%s' AND revoked = 0 AND expired IS NULL;`, username)
	cmd := exec.CommandContext(ctx, "sqlite3", dbPath)
	cmd.Stdin = strings.NewReader(q)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sqlite3 unban: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parseAutheliaTime tolerates the two timestamp shapes Authelia writes
// into banned_user.expires across versions: "2026-04-29 09:18:19" and
// "2026-04-29 09:18:19.773002823+00:00". Falls back to zero time on
// parse failure so the template renders "—" instead of a misleading
// 0001-01-01.
func parseAutheliaTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		time.RFC3339,
		time.RFC3339Nano,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised authelia timestamp: %q", s)
}
