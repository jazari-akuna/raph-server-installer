// totp.go — TOTP secret generation via `docker exec authelia …`.
//
// We rely on Authelia's CLI to do the actual TOTP write, because:
//   1. The secret must land in Authelia's encrypted sqlite at
//      /var/lib/authelia/db.sqlite3 (encrypted with
//      $AUTHELIA_STORAGE_ENCRYPTION_KEY). We don't have the encryption
//      key in enrol; only the authelia container does.
//   2. The CLI handles secret-format details (algorithm/digits/period)
//      consistently with the portal-enrolment path.
//
// Output of `authelia storage user totp generate <user> --force`
// includes the otpauth URI on stdout. We extract it via regex, render
// a QR PNG via `qrencode`, and return the URI + PNG bytes for one-shot
// display in the browser. The URI is NOT logged.
//
// On user-delete: `authelia storage user totp delete <user>` returns
// 0 even if no TOTP entry exists; idempotent.

package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var reOtpauth = regexp.MustCompile(`otpauth://totp/[^\s]+`)

// totpGenerate runs the Authelia CLI to (re)generate a TOTP config and
// returns (otpauth_uri, qr_png_bytes, error). The QR is generated with
// qrencode in this container; the URI is not persisted anywhere except
// in the encrypted sqlite (managed by authelia itself).
func totpGenerate(cfg config, user string) (otpauth string, qrPNG []byte, err error) {
	if !validUsername(user) {
		return "", nil, fmt.Errorf("invalid username %q", user)
	}
	cmd := exec.Command("docker", "exec", cfg.autheliaContainer,
		"authelia", "storage", "user", "totp", "generate", user, "--force")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("authelia totp generate %s: %w (%s)",
			user, err, strings.TrimSpace(string(out)))
	}
	m := reOtpauth.FindString(string(out))
	if m == "" {
		return "", nil, fmt.Errorf("authelia totp generate %s: no otpauth URI in output", user)
	}
	otpauth = m

	qrPNG, err = qrencode(otpauth)
	if err != nil {
		// We already have the URI; the QR is convenience.
		return otpauth, nil, fmt.Errorf("qrencode: %w", err)
	}
	return otpauth, qrPNG, nil
}

// totpDelete removes the user's TOTP config. Idempotent — Authelia
// returns 0 even if no row exists.
func totpDelete(cfg config, user string) error {
	if !validUsername(user) {
		return fmt.Errorf("invalid username %q", user)
	}
	cmd := exec.Command("docker", "exec", cfg.autheliaContainer,
		"authelia", "storage", "user", "totp", "delete", user)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Tolerate "no TOTP configuration" / "no rows" / "not found"
		// gracefully — that's the success case for a delete on a user
		// who never enrolled TOTP. Surface anything else.
		s := strings.ToLower(string(out))
		if strings.Contains(s, "no rows") ||
			strings.Contains(s, "not found") ||
			strings.Contains(s, "no totp configuration") {
			return nil
		}
		return fmt.Errorf("authelia totp delete %s: %w (%s)",
			user, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// qrencode renders the given text into a PNG via the `qrencode` binary.
// Higher resolution (-s 6) for legibility on retina screens.
func qrencode(text string) ([]byte, error) {
	cmd := exec.Command("qrencode", "-t", "PNG", "-o", "-", "-s", "6", "-l", "L")
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("qrencode: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
