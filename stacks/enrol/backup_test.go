// backup_test.go — unit coverage for the pure surface of backup.go.
//
// Scope: recipe lookup, confirm-token guard, path-traversal rejection,
// envfile parsing (port of TaskClient cases against the new shared
// readPostgresPassword helper), JSON parse of `restic snapshots --json`
// output, and the per-tag retention math.
//
// Out of scope: anything that shells out to restic / docker / systemctl
// — those are integration concerns covered by Wave 3's verifier.

package main

import (
	"crypto/subtle"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1. recipe lookup

func TestRecipeByID(t *testing.T) {
	cases := []struct {
		id      string
		wantNil bool
	}{
		{"cloud", false},
		{"task", false},
		{"authelia", false},
		{"ingress", false},
		{"enrol", false},
		{"", true},
		{"xxx", true},
		{"CLOUD", true}, // case-sensitive on purpose
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := recipeByID(tc.id)
			if tc.wantNil && got != nil {
				t.Errorf("recipeByID(%q) = %+v, want nil", tc.id, got)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("recipeByID(%q) returned nil, want match", tc.id)
			}
			if got != nil && got.ID != tc.id {
				t.Errorf("recipeByID(%q).ID = %q, want %q", tc.id, got.ID, tc.id)
			}
		})
	}
}

// Recipe IDs must be unique — the lookup-by-id contract assumes it.
func TestRecipeIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range backupRecipes {
		if seen[r.ID] {
			t.Errorf("duplicate recipe id: %q", r.ID)
		}
		seen[r.ID] = true
	}
}

// "enrol" recipe must declare itself non-restorable (per ADR-010 — can't
// restore self mid-stream). Defensive: an accidental Restorable=true on
// enrol would let an admin click "Restore" on the live process.
func TestEnrolNotRestorable(t *testing.T) {
	r := recipeByID("enrol")
	if r == nil {
		t.Fatal("enrol recipe missing")
	}
	if r.Restorable {
		t.Error("enrol recipe must NOT be Restorable")
	}
}

// ---------------------------------------------------------------------------
// 2. confirm-token guard (constant-time compare)

func TestConfirmTokenGuard(t *testing.T) {
	cases := []struct {
		name        string
		formConfirm string
		recipeID    string
		wantOK      bool
	}{
		{"exact match", "cloud", "cloud", true},
		{"case mismatch", "Cloud", "cloud", false},
		{"prefix only", "clo", "cloud", false},
		{"suffix only", "cloud-bad", "cloud", false},
		{"empty form", "", "cloud", false},
		{"empty recipe", "cloud", "", false},
		{"both empty match (sentinel)", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok := subtle.ConstantTimeCompare([]byte(tc.formConfirm), []byte(tc.recipeID)) == 1
			if ok != tc.wantOK {
				t.Errorf("ConstantTimeCompare(%q,%q) = %v, want %v",
					tc.formConfirm, tc.recipeID, ok, tc.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. path-traversal rejection on restore

func TestSafeRestoreTargets(t *testing.T) {
	cloud := recipeByID("cloud")
	if cloud == nil {
		t.Fatal("cloud recipe missing")
	}
	okCases := []struct {
		name string
		path string
	}{
		{"exact prefix", "/srv/store/cloud-data"},
		{"strict child", "/srv/store/cloud-config/config.php"},
		{"deep child", "/srv/store/cloud-apps/files_external/lib/foo.php"},
	}
	for _, tc := range okCases {
		t.Run("ok/"+tc.name, func(t *testing.T) {
			if err := safeRestoreTargets([]string{tc.path}, cloud); err != nil {
				t.Errorf("safeRestoreTargets(%q) returned %v, want nil", tc.path, err)
			}
		})
	}
	badCases := []struct {
		name string
		path string
	}{
		{"escape via traversal", "/srv/store/cloud-data/../../etc/shadow"},
		{"unrelated absolute", "/etc/shadow"},
		{"sibling prefix mismatch", "/srv/store/cloud-data-evil"},
	}
	for _, tc := range badCases {
		t.Run("bad/"+tc.name, func(t *testing.T) {
			if err := safeRestoreTargets([]string{tc.path}, cloud); err == nil {
				t.Errorf("safeRestoreTargets(%q) = nil, want error", tc.path)
			}
		})
	}

	// nil recipe always errors.
	if err := safeRestoreTargets([]string{"/srv/store/cloud-data"}, nil); err == nil {
		t.Error("safeRestoreTargets with nil recipe should error")
	}
}

// ---------------------------------------------------------------------------
// 4. envfile parsing — ported from TestPasswordReadsEnvFile in
//    task_client_test.go to lock the shared helper to the same contract.

func TestReadPostgresPassword(t *testing.T) {
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
			got, err := readPostgresPassword(envPath)
			if err != nil {
				t.Fatalf("readPostgresPassword returned error: %v", err)
			}
			if got != tc.want {
				t.Errorf("readPostgresPassword() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadPostgresPasswordMissingFile(t *testing.T) {
	_, err := readPostgresPassword("/nonexistent/.env.noway")
	if err == nil {
		t.Error("expected error opening missing file")
	}
}

// ---------------------------------------------------------------------------
// 5. parse `restic snapshots --json` output

func TestParseResticSnapshots(t *testing.T) {
	// Hand-crafted fixture that mirrors restic 0.16's actual JSON shape.
	// Two snapshots: one with summary (typical of recent restic), one
	// without (the older v0 archive format leaves Summary nil).
	fixture := []byte(`[
		{
			"id": "abcd1234ef567890aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"time": "2026-04-28T03:00:00Z",
			"tree": "deadbeef",
			"paths": ["/srv/store/cloud-data"],
			"hostname": "vps",
			"tags": ["cloud", "daily"],
			"summary": {
				"total_bytes_processed": 312000000
			}
		},
		{
			"id": "1234567890aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"time": "2026-04-27T03:00:00Z",
			"tree": "deadbeef",
			"paths": ["/srv/store/cloud-data"],
			"hostname": "vps",
			"tags": ["cloud", "daily"]
		}
	]`)

	got, err := parseResticSnapshots(fixture)
	if err != nil {
		t.Fatalf("parseResticSnapshots returned error: %v", err)
	}
	want := []BackupSnapshotView{
		{
			ID:        "abcd1234",
			CreatedAt: "2026-04-28 03:00 UTC",
			Size:      "312 MB",
			Tags:      []string{"cloud", "daily"},
		},
		{
			ID:        "12345678",
			CreatedAt: "2026-04-27 03:00 UTC",
			Size:      "—",
			Tags:      []string{"cloud", "daily"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseResticSnapshots got\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestParseResticSnapshotsEmpty(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("null"),
		[]byte("  "),
		[]byte("[]"),
	}
	for _, body := range cases {
		got, err := parseResticSnapshots(body)
		if err != nil {
			t.Errorf("parseResticSnapshots(%q) returned %v", body, err)
		}
		if len(got) != 0 {
			t.Errorf("parseResticSnapshots(%q) = %+v, want empty", body, got)
		}
	}
}

func TestParseResticSnapshotsMalformed(t *testing.T) {
	if _, err := parseResticSnapshots([]byte("{not json")); err == nil {
		t.Error("expected error on malformed JSON")
	}
}

// formatBytes is exercised indirectly via the snapshot parse but pin
// the boundaries explicitly so a future "use binary units" refactor is
// a deliberate change.
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1 KB"},
		{1500, "1 KB"},
		{1_000_000, "1 MB"},
		{312_000_000, "312 MB"},
		{1_500_000_000, "1.5 GB"},
		{1_500_000_000_000, "1.5 TB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := formatBytes(tc.n); got != tc.want {
				t.Errorf("formatBytes(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 6. retention math

func TestSnapshotsToForget(t *testing.T) {
	mk := func(n int) []resticSnapshot {
		out := make([]resticSnapshot, n)
		// Oldest first; ID embeds the index so we can assert WHICH ones
		// were chosen for forgetting.
		base := time.Date(2026, 4, 1, 3, 0, 0, 0, time.UTC)
		for i := 0; i < n; i++ {
			out[i] = resticSnapshot{
				ID:   itoa(i),
				Time: base.Add(time.Duration(i) * 24 * time.Hour),
			}
		}
		// Shuffle to ensure the function does its own sort. Skip the
		// swap on n<2 — there's nothing to shuffle and the index would
		// panic.
		if n >= 2 {
			out[0], out[len(out)-1] = out[len(out)-1], out[0]
		}
		return out
	}

	cases := []struct {
		name    string
		n       int
		keep    int
		wantLen int
	}{
		{"exactly keep limit", 7, 7, 0},
		{"none yet", 0, 7, 0},
		{"under keep limit", 3, 7, 0},
		{"one over", 8, 7, 1},
		{"nine over → drop two", 9, 7, 2},
		{"a hundred → drop 93", 100, 7, 93},
		{"keep zero → drop all", 5, 0, 5},
		{"negative keep coerced to zero", 5, -1, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snaps := mk(tc.n)
			drop := snapshotsToForget(snaps, tc.keep)
			if len(drop) != tc.wantLen {
				t.Errorf("snapshotsToForget(n=%d, keep=%d) returned %d ids, want %d",
					tc.n, tc.keep, len(drop), tc.wantLen)
			}
			// Sanity: drop list IDs must be the OLDEST entries — the lowest
			// numeric IDs in our deterministic fixture.
			if tc.wantLen > 0 {
				ints := make([]int, 0, tc.wantLen)
				for _, id := range drop {
					ints = append(ints, atoi(id))
				}
				sort.Ints(ints)
				for i, v := range ints {
					if v != i {
						t.Errorf("expected oldest ids 0..%d to be forgotten; got %v",
							tc.wantLen-1, ints)
						break
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers (no external deps for tiny int↔string)

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func atoi(s string) int {
	var i int
	_ = json.Unmarshal([]byte(s), &i)
	return i
}
