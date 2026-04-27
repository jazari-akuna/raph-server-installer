// oidc_test.go — unit coverage for the OIDC console client_secret rotation.
//
// Tests use only the stdlib + golang.org/x/crypto/pbkdf2 (already a
// transitive dep via argon2). No live Authelia, no shell-out.

package main

import (
	"crypto/sha512"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

// TestPBKDF2HashRoundTrip pins a fixed plaintext + salt and asserts the
// emitted hash is byte-for-byte verifiable against the same pbkdf2-sha512
// derivation. Catches regressions in the encoding (padding leak, alphabet
// swap) or rounds/key-length defaults.
func TestPBKDF2HashRoundTrip(t *testing.T) {
	plaintext := "hunter2hunter2"
	salt := []byte("0123456789abcdef") // 16 bytes — matches CLI default
	got := pbkdf2SHA512Hash(plaintext, salt)

	// Format check.
	if !pbkdf2HashRe.MatchString(got) {
		t.Fatalf("emitted hash %q does not match Authelia format regex", got)
	}

	// Re-derive and compare the embedded hash chunk.
	parts := strings.Split(got, "$")
	// $pbkdf2-sha512$<rounds>$<salt-b64>$<hash-b64> → ["", "pbkdf2-sha512", "<rounds>", "<salt>", "<hash>"]
	if len(parts) != 5 {
		t.Fatalf("expected 5 segments, got %d (%q)", len(parts), got)
	}
	wantSaltB64 := oidcBase64.EncodeToString(salt)
	if parts[3] != wantSaltB64 {
		t.Errorf("salt segment mismatch: got %q want %q", parts[3], wantSaltB64)
	}
	derived := pbkdf2.Key([]byte(plaintext), salt, oidcPBKDF2Rounds, oidcPBKDF2KeyLen, sha512.New)
	wantHashB64 := oidcBase64.EncodeToString(derived)
	if parts[4] != wantHashB64 {
		t.Errorf("hash segment mismatch: got %q want %q", parts[4], wantHashB64)
	}
}

// TestPBKDF2HashRejectsBootstrapPlaceholder ensures the regex used in the
// templates-step verification rejects every flavour of the bootstrap.sh
// placeholder. A regression here would let a silent template-render
// failure be marked "complete" again.
func TestPBKDF2HashRejectsBootstrapPlaceholder(t *testing.T) {
	bad := []string{
		"$pbkdf2-sha512$bootstrap-placeholder",
		"$pbkdf2-sha512$check-mode-placeholder",
		"REPLACE_ME_WITH_PBKDF2_HASH",
		"",
		"$pbkdf2-sha512$310000$$",
		"$pbkdf2-sha512$310000$valid$=padded=", // padding not allowed
	}
	for _, b := range bad {
		if pbkdf2HashRe.MatchString(b) {
			t.Errorf("regex incorrectly accepted placeholder %q", b)
		}
	}
}

// TestRotateOIDCConsoleSecretWritesPlaintextAndEnv covers the happy path:
// fresh placeholder in env → after rotate, env has a real-looking hash
// AND the plaintext file exists with mode 0600.
func TestRotateOIDCConsoleSecretWritesPlaintextAndEnv(t *testing.T) {
	tmp := t.TempDir()
	envPath := filepath.Join(tmp, ".env")
	plaintextPath := filepath.Join(tmp, "oidc-secret")

	original := "DOMAIN='example.com'\n" +
		oidcEnvVar + "='$pbkdf2-sha512$bootstrap-placeholder'\n" +
		"OTHER='unchanged'\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatalf("seed env: %v", err)
	}

	if err := rotateOIDCConsoleSecret(envPath, plaintextPath); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	envOut, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	gotHash := readEnvVar(string(envOut), oidcEnvVar)
	if !pbkdf2HashRe.MatchString(gotHash) {
		t.Errorf("post-rotate env hash not valid: %q", gotHash)
	}
	if !strings.Contains(string(envOut), "DOMAIN='example.com'") ||
		!strings.Contains(string(envOut), "OTHER='unchanged'") {
		t.Errorf("rotation clobbered surrounding env lines: %s", envOut)
	}

	info, err := os.Stat(plaintextPath)
	if err != nil {
		t.Fatalf("stat plaintext: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("plaintext mode = %o, want 0600", info.Mode().Perm())
	}
	plain, err := os.ReadFile(plaintextPath)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if len(strings.TrimSpace(string(plain))) < 30 {
		t.Errorf("plaintext too short to be the random secret: %q", plain)
	}
}

// TestRotateOIDCConsoleSecretIdempotent: a second call with a real-looking
// hash AND existing plaintext file is a no-op (no churn). This is the
// finalize-retry contract — we MUST NOT regenerate on every retry, or
// operators would have to reconfigure Portainer after every blip.
func TestRotateOIDCConsoleSecretIdempotent(t *testing.T) {
	tmp := t.TempDir()
	envPath := filepath.Join(tmp, ".env")
	plaintextPath := filepath.Join(tmp, "oidc-secret")
	if err := os.WriteFile(envPath, []byte(oidcEnvVar+"='$pbkdf2-sha512$bootstrap-placeholder'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateOIDCConsoleSecret(envPath, plaintextPath); err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	envFirst, _ := os.ReadFile(envPath)
	plainFirst, _ := os.ReadFile(plaintextPath)
	if err := rotateOIDCConsoleSecret(envPath, plaintextPath); err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	envSecond, _ := os.ReadFile(envPath)
	plainSecond, _ := os.ReadFile(plaintextPath)
	if string(envFirst) != string(envSecond) {
		t.Errorf("env churned on second call:\nfirst:  %s\nsecond: %s", envFirst, envSecond)
	}
	if string(plainFirst) != string(plainSecond) {
		t.Errorf("plaintext churned on second call (operator would need to re-paste into Portainer)")
	}
}

// TestRotateOIDCConsoleSecretRecoversFromMissingPlaintext: if the env hash
// looks valid BUT the plaintext file is missing (e.g. /etc/raph-installer
// was wiped by a partial cleanup), we MUST regenerate so the operator has
// something to paste. Otherwise the system is wedged silently.
func TestRotateOIDCConsoleSecretRecoversFromMissingPlaintext(t *testing.T) {
	tmp := t.TempDir()
	envPath := filepath.Join(tmp, ".env")
	plaintextPath := filepath.Join(tmp, "oidc-secret")
	// Seed with a syntactically valid hash but no plaintext file.
	salt := []byte("0123456789abcdef")
	hash := pbkdf2SHA512Hash("never-known", salt)
	if err := os.WriteFile(envPath, []byte(oidcEnvVar+"='"+hash+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateOIDCConsoleSecret(envPath, plaintextPath); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := os.Stat(plaintextPath); err != nil {
		t.Errorf("plaintext should have been regenerated: %v", err)
	}
	envOut, _ := os.ReadFile(envPath)
	newHash := readEnvVar(string(envOut), oidcEnvVar)
	if newHash == hash {
		t.Errorf("hash should have rotated when plaintext was missing")
	}
}

// TestReplaceOrAppendEnvLineAppendsWhenAbsent: if the key isn't present,
// the function appends. Sanity check for the env-rewrite primitive.
func TestReplaceOrAppendEnvLineAppendsWhenAbsent(t *testing.T) {
	in := "FOO=bar\n"
	out := replaceOrAppendEnvLine(in, "BAZ", "BAZ=qux")
	if !strings.Contains(out, "BAZ=qux\n") {
		t.Errorf("expected BAZ line appended, got %q", out)
	}
	if !strings.Contains(out, "FOO=bar\n") {
		t.Errorf("clobbered existing FOO line: %q", out)
	}
}

// TestReadEnvVarStripsQuotes covers the three quoting styles bootstrap.sh
// emits ('single', "double", and bare).
func TestReadEnvVarStripsQuotes(t *testing.T) {
	cases := map[string]string{
		"K='v1'\n":          "v1",
		"K=\"v2\"\n":        "v2",
		"K=v3\n":            "v3",
		"export K='v4'\n":   "v4",
		"  export K=v5  \n": "v5",
	}
	for in, want := range cases {
		if got := readEnvVar(in, "K"); got != want {
			t.Errorf("readEnvVar(%q)=%q want %q", in, got, want)
		}
	}
}
