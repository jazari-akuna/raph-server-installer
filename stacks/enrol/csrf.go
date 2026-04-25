// csrf.go — token mint + verify.
//
// Strategy:
//   - 32 bytes from crypto/rand, base64url-no-padding.
//   - Stored in cookie `enrol_csrf` (Path=/, Secure, SameSite=Strict).
//   - Embedded in every form as <input type="hidden" name="csrf">.
//   - Verified on every POST: form value must equal cookie value.
//   - Mismatch => 403 + audit `csrf.fail`.
//
// We do not separately bind the token to the Authelia session because
// Authelia owns the session entirely upstream — we'd be re-implementing
// a session table for no gain. Pinning to the cookie is enough as long
// as SameSite=Strict is honored (it is, in every modern browser).

package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"path/filepath"
	"time"
)

const csrfCookie = "enrol_csrf"

// ensureCSRF mints a fresh token if the cookie is absent or malformed,
// otherwise leaves the existing one in place. Returns the token to embed
// in the rendered HTML.
func ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && validCSRFLen(c.Value) {
		return c.Value
	}
	tok := newCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    tok,
		Path:     "/",
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		// Lifetime: 24h. Tokens roll on next GET if expired.
		Expires: time.Now().Add(24 * time.Hour),
	})
	return tok
}

// requireCSRF returns nil iff the form's `csrf` field equals the cookie.
// Reject with 403 on mismatch.
func requireCSRF(r *http.Request) error {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || !validCSRFLen(cookie.Value) {
		return errCSRFMissing
	}
	if err := r.ParseForm(); err != nil {
		return errCSRFMissing
	}
	formTok := r.Form.Get("csrf")
	if !validCSRFLen(formTok) {
		return errCSRFMissing
	}
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formTok)) != 1 {
		return errCSRFMismatch
	}
	return nil
}

// withCSRF wraps a POST handler with CSRF verification. On failure we
// emit a 403 and write a `csrf.fail` audit entry.
func (s *server) withCSRF(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			h(w, r)
			return
		}
		if err := requireCSRF(r); err != nil {
			actor := r.Header.Get("X-Enrol-User")
			writeAudit(filepath.Join(s.cfg.awgDir, "peers-audit.log"), auditEntry{
				Time:   time.Now().UTC(),
				Action: "csrf.fail",
				Actor:  actor,
				Note:   r.URL.Path + ": " + err.Error(),
			})
			http.Error(w, "403 — CSRF token missing or invalid. Reload the form and try again.",
				http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func newCSRFToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the kernel CSPRNG is broken;
		// nothing safe to do but bail.
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func validCSRFLen(s string) bool {
	// 32 bytes => 43-char base64url-no-padding.
	return len(s) == 43
}

// Sentinel errors so callers can distinguish "no token" from "wrong
// token" if they want different audit details. Currently the audit
// entry just records the message.
var (
	errCSRFMissing  = csrfError("missing CSRF cookie or form value")
	errCSRFMismatch = csrfError("CSRF token mismatch")
)

type csrfError string

func (e csrfError) Error() string { return string(e) }
