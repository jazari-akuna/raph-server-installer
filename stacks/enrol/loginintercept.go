// loginintercept.go — proxy POST /api/firstfactor through to Authelia and,
// on a successful login, auto-unlock the user's LUKS volume.
//
// Wiring:
//   NPM rewrites only POST /api/firstfactor to enrol's /login-intercept.
//   Every other Authelia endpoint (verify, second-factor, logout, …) goes
//   straight to authelia:9091 unmodified.
//
// On 200 from Authelia we fire a goroutine that calls luksUnlock(). Login
// NEVER fails because LUKS failed — the audit log records the unlock
// outcome and that's all. The user's LUKS passphrase is assumed to equal
// their Authelia password (operator-enforced via /users/<u>/luks/passphrase
// after a password change; documented in README.md "auto-unlock at login").

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Single shared client. Authelia is reachable on the host loopback so a
// short timeout is fine and per-request construction is wasteful.
var autheliaHTTPClient = &http.Client{Timeout: 5 * time.Second}

type firstFactorBody struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	KeepMeLoggedIn bool   `json:"keepMeLoggedIn"`
}

func (s *server) handleLoginIntercept(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	// Bare unmarshal for password capture; failure is non-fatal — we
	// still proxy so Authelia can produce its real error response.
	var creds firstFactorBody
	_ = json.Unmarshal(bodyBytes, &creds)

	upstreamURL := strings.TrimRight(s.cfg.autheliaURL, "/") + "/api/firstfactor"
	upstreamReq, err := http.NewRequestWithContext(r.Context(),
		http.MethodPost, upstreamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, "build upstream req: "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	// Host header — http.Client uses req.Host, NOT req.Header["Host"].
	// Setting via Header.Set is silently ignored. Authelia validates the
	// Host against its trusted-redirect domains; a wrong/empty Host
	// produces a generic 401 with no useful log line.
	upstreamReq.Host = r.Host

	if ct := r.Header.Get("Content-Type"); ct != "" {
		upstreamReq.Header.Set("Content-Type", ct)
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		upstreamReq.Header.Set("X-Forwarded-For", xff)
	} else {
		upstreamReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		upstreamReq.Header.Set("X-Forwarded-Host", xfh)
	} else {
		upstreamReq.Header.Set("X-Forwarded-Host", r.Host)
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		upstreamReq.Header.Set("X-Forwarded-Proto", xfp)
	} else {
		upstreamReq.Header.Set("X-Forwarded-Proto", "https")
	}
	if cookie := r.Header.Get("Cookie"); cookie != "" {
		upstreamReq.Header.Set("Cookie", cookie)
	}
	// Strip Authorization explicitly: enrol's other routes accept
	// Remote-User from upstream, but Authelia's first-factor doesn't
	// want a stray Authorization confusing it.
	upstreamReq.Header.Del("Authorization")
	// And we never forward identity headers — Authelia would refuse to
	// re-authenticate an already-impersonated request.
	upstreamReq.Header.Del("Remote-User")
	upstreamReq.Header.Del("Remote-Groups")

	resp, err := autheliaHTTPClient.Do(upstreamReq)
	if err != nil {
		log.Printf("login-intercept: upstream error: %v", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers, skipping hop-by-hop. Set-Cookie is special:
	// it can repeat and must be Add'd, never Set (Set clobbers).
	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Set-Cookie":
			continue
		case "Transfer-Encoding", "Connection", "Content-Length":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for _, v := range resp.Header["Set-Cookie"] {
		w.Header().Add("Set-Cookie", v)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)

	if resp.StatusCode == http.StatusOK && creds.Username != "" && creds.Password != "" {
		// Copy the password into a fresh byte slice handed to the
		// goroutine; zero the captured strings immediately. (Go strings
		// are immutable so we can only zero the local copies, but
		// shrinking the lifetime helps a little.)
		passBytes := make([]byte, len(creds.Password))
		copy(passBytes, creds.Password)
		user := creds.Username
		creds.Password = ""
		creds.Username = ""
		auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")
		go s.autoUnlock(user, passBytes, auditPath)
	}
}

// autoUnlock runs LUKS unlock for `user` using `passBytes` (zeroed in
// defer). Holds s.mu for the duration to serialise against UI-driven
// LUKS ops. Recovers panics so a crash in cryptsetup wrappers can't
// take down the server.
func (s *server) autoUnlock(user string, passBytes []byte, auditPath string) {
	defer func() {
		for i := range passBytes {
			passBytes[i] = 0
		}
		if rec := recover(); rec != nil {
			log.Printf("autoUnlock(%s): panic: %v", user, rec)
			writeAudit(auditPath, auditEntry{
				Action: "luks.auto-unlock", Actor: user, Target: user,
				Result: "fail", Note: "panic",
			})
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := luksUnlock(s.cfg, user, string(passBytes)); err != nil {
		writeAudit(auditPath, auditEntry{
			Action: "luks.auto-unlock", Actor: user, Target: user,
			Result: "fail", Note: classifyLuksError(err),
		})
		return
	}
	writeAudit(auditPath, auditEntry{
		Action: "luks.auto-unlock", Actor: user, Target: user,
		Result: "ok", Note: "via login",
	})
}

// classifyLuksError returns a stable category string for the audit log.
// We deliberately do NOT echo the raw error: it can include cryptsetup's
// stderr which on some failure paths contains the passphrase prompt,
// and we don't want any chance of leaking secret material to the JSONL.
func classifyLuksError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no luks blob"):
		return "no LUKS blob"
	case strings.Contains(msg, "no key available"),
		strings.Contains(msg, "wrong passphrase"),
		strings.Contains(msg, "no key with this passphrase"):
		return "wrong passphrase"
	case strings.Contains(msg, "cryptsetup open"):
		return "cryptsetup open failed"
	case strings.Contains(msg, "mount:"), strings.Contains(msg, "mount failed"):
		return "mount failed"
	}
	return "unknown"
}
