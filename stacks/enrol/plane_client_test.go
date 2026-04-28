// plane_client_test.go — unit tests for the Vikunja-backed PlaneClient.
//
// The live integration shells out `docker exec plane-db psql ...`, which
// can't be mocked without a docker-in-docker setup. These tests therefore
// cover the fast, mockable surface only:
//
//   - constructor + ready()
//   - silent fallback when the client is unconfigured
//   - .env password parsing (quoted/unquoted/missing)
//
// The actual psql shell-out is exercised by the live deploy smoke test
// in scripts/smoke-test.sh (TODO: add an assertion there for the admin
// /users page rendering plane columns once Vikunja has at least one user).

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ----- constructor + ready() -----

func TestNewPlaneClient(t *testing.T) {
	cases := []struct {
		name      string
		container string
		dbName    string
		dbUser    string
		envFile   string
		wantReady bool
	}{
		{
			name:      "fully configured",
			container: "plane-db",
			dbName:    "vikunja",
			dbUser:    "vikunja",
			envFile:   "/opt/stacks/plane/.env",
			wantReady: true,
		},
		{
			name:      "missing container → not ready",
			container: "",
			dbName:    "vikunja",
			dbUser:    "vikunja",
			envFile:   "/opt/stacks/plane/.env",
			wantReady: false,
		},
		{
			name:      "missing env file → not ready",
			container: "plane-db",
			dbName:    "vikunja",
			dbUser:    "vikunja",
			envFile:   "",
			wantReady: false,
		},
		{
			name:      "whitespace stripped",
			container: "  plane-db  ",
			dbName:    "vikunja",
			dbUser:    "vikunja",
			envFile:   "/opt/stacks/plane/.env",
			wantReady: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewPlaneClient(tc.container, tc.dbName, tc.dbUser, tc.envFile)
			if got := c.ready(); got != tc.wantReady {
				t.Errorf("ready() = %v, want %v", got, tc.wantReady)
			}
		})
	}
}

// Nil receiver must not panic — admin page may call methods on a partial
// startup config.
func TestPlaneClientNilReady(t *testing.T) {
	var c *PlaneClient
	if c.ready() {
		t.Errorf("nil client should not be ready")
	}
}

// ----- silent fallback when unconfigured -----

// UserStats with empty container → (zero, nil) without ever shelling out.
// (We can't observe the absence of a shell-out directly, but we verify
// the function returns instantly with no error and zero values.)
func TestUserStatsUnconfigured(t *testing.T) {
	c := NewPlaneClient("", "", "", "")
	stats, err := c.UserStats(context.Background(), "alice@example.com")
	if err != nil {
		t.Errorf("expected nil error on unconfigured client, got %v", err)
	}
	if stats.Tasks != 0 || stats.AttBytes != 0 || stats.UserExists {
		t.Errorf("expected zero stats on unconfigured client, got %+v", stats)
	}
}

// Empty email is a no-op (admin page may pass "" for users with no email
// claim from Authelia).
func TestUserStatsEmptyEmail(t *testing.T) {
	c := NewPlaneClient("plane-db", "vikunja", "vikunja", "/dev/null")
	stats, err := c.UserStats(context.Background(), "")
	if err != nil {
		t.Errorf("expected nil error on empty email, got %v", err)
	}
	if stats.UserExists || stats.Tasks != 0 {
		t.Errorf("expected zero stats on empty email, got %+v", stats)
	}
}

// ----- .env password parsing -----

func TestPasswordReadsEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	cases := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name:     "plain value",
			contents: "POSTGRES_PASSWORD=secret123\nOTHER=x\n",
			want:     "secret123",
		},
		{
			name:     "single-quoted value",
			contents: "POSTGRES_PASSWORD='quoted-secret'\n",
			want:     "quoted-secret",
		},
		{
			name:     "double-quoted value",
			contents: "POSTGRES_PASSWORD=\"another secret\"\n",
			want:     "another secret",
		},
		{
			name:     "leading whitespace tolerated",
			contents: "   POSTGRES_PASSWORD=trimmed\n",
			want:     "trimmed",
		},
		{
			name:     "absent key → empty",
			contents: "OTHER=x\nFOO=bar\n",
			want:     "",
		},
		{
			name:     "first-match wins",
			contents: "POSTGRES_PASSWORD=first\nPOSTGRES_PASSWORD=second\n",
			want:     "first",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(envPath, []byte(tc.contents), 0o600); err != nil {
				t.Fatalf("write env: %v", err)
			}
			c := NewPlaneClient("plane-db", "vikunja", "vikunja", envPath)
			// Force a fresh read by zeroing the cache.
			c.cachedPwd, c.cachedPwdAt = "", time.Time{}
			got := c.password()
			if got != tc.want {
				t.Errorf("password() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Missing env file → empty password (silent fallback). Must not panic
// or surface the os.Open error.
func TestPasswordMissingFile(t *testing.T) {
	c := NewPlaneClient("plane-db", "vikunja", "vikunja", "/nonexistent/.env.noway")
	if got := c.password(); got != "" {
		t.Errorf("password() on missing file = %q, want empty", got)
	}
}

// requireConfigured returns a sentinel-wrapped error when partial; nil
// when fully configured. Used by potential future tests asserting
// "deployed mode".
func TestRequireConfigured(t *testing.T) {
	cfg := NewPlaneClient("plane-db", "vikunja", "vikunja", "/opt/stacks/plane/.env")
	if err := cfg.requireConfigured(); err != nil {
		t.Errorf("fully-configured client should not error, got %v", err)
	}
	empty := NewPlaneClient("", "", "", "")
	if err := empty.requireConfigured(); err == nil {
		t.Errorf("empty client should error")
	}
}

// ----- shim methods preserving the old call surface -----
//
// storage.go still uses ListWorkspaces / UserByEmail / IssueCount /
// FileAssetBytes — make sure they don't panic on a nil/empty client.

func TestShimsSafeOnUnconfigured(t *testing.T) {
	c := NewPlaneClient("", "", "", "")
	ctx := context.Background()
	if ws, _ := c.ListWorkspaces(ctx); ws != nil {
		t.Errorf("ListWorkspaces should return nil on unconfigured client")
	}
	if u, _ := c.UserByEmail(ctx, "x@y"); u != nil {
		t.Errorf("UserByEmail should return nil on unconfigured client")
	}
	if n, _ := c.IssueCount(ctx, "any", "x@y"); n != 0 {
		t.Errorf("IssueCount should return 0 on unconfigured client")
	}
	if b, _ := c.FileAssetBytes(ctx, "any", "x@y"); b != 0 {
		t.Errorf("FileAssetBytes should return 0 on unconfigured client")
	}
	if t0, _ := c.LastActivity(ctx, "x@y"); !t0.IsZero() {
		t.Errorf("LastActivity should return zero time on unconfigured client")
	}
}
