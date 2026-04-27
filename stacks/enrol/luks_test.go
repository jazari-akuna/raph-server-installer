// luks_test.go — unit tests for the size-resolution helpers in luks.go.
//
// We don't exercise the full storageSnapshot here because it shells out
// to syscall.Statfs (host filesystem) and stat (per-user .img) — the
// behaviour we want to lock down is the priority order in
// defaultPersonalNominalBytes, which is just file IO + JSON parsing.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPersonalNominalBytes_PrefersStateJSON(t *testing.T) {
	dir := t.TempDir()
	state := []byte(`{"personal_luks_size":16106127360,"shared_luks_size":107374182400}`)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), state, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	cfg := config{setupStateDir: dir, luksSizeGB: 50}
	got := defaultPersonalNominalBytes(cfg)
	want := int64(16106127360)
	if got != want {
		t.Fatalf("nominal = %d, want %d (15 GiB from state.json, not 50 GiB env fallback)", got, want)
	}
}

func TestDefaultPersonalNominalBytes_FallsBackToEnvWhenStateMissing(t *testing.T) {
	dir := t.TempDir() // empty — no state.json
	cfg := config{setupStateDir: dir, luksSizeGB: 50}
	got := defaultPersonalNominalBytes(cfg)
	want := int64(50) << 30
	if got != want {
		t.Fatalf("nominal = %d, want %d (50 GiB env fallback)", got, want)
	}
}

func TestDefaultPersonalNominalBytes_FallsBackToEnvWhenStateZero(t *testing.T) {
	dir := t.TempDir()
	// state.json with personal_luks_size omitted ⇒ zero ⇒ env fallback applies.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"step":"welcome"}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	cfg := config{setupStateDir: dir, luksSizeGB: 50}
	got := defaultPersonalNominalBytes(cfg)
	want := int64(50) << 30
	if got != want {
		t.Fatalf("nominal = %d, want %d (env fallback when personal_luks_size unset)", got, want)
	}
}

func TestDefaultPersonalNominalBytes_FallsBackOnUnparseableJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	cfg := config{setupStateDir: dir, luksSizeGB: 7}
	got := defaultPersonalNominalBytes(cfg)
	want := int64(7) << 30
	if got != want {
		t.Fatalf("nominal = %d, want %d (env fallback on bad JSON)", got, want)
	}
}

func TestDefaultPersonalNominalBytes_NoStateDirReturnsEnv(t *testing.T) {
	cfg := config{setupStateDir: "", luksSizeGB: 50}
	got := defaultPersonalNominalBytes(cfg)
	want := int64(50) << 30
	if got != want {
		t.Fatalf("nominal = %d, want %d (env fallback when setupStateDir empty)", got, want)
	}
}
