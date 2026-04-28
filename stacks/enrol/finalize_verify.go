// finalize_verify.go — observable-outcome verifications for each finalize
// step.
//
// Background. Earlier finalize implementations marked CompletedSteps.X = true
// the moment a step's function returned nil. Several step functions can
// return nil WITHOUT actually completing the work:
//
//   * finalizeRenderTemplates: render-templates.sh exits 0 even when the
//     env var driving a substitution still holds the bootstrap-placeholder
//     string. The rendered file is "successfully" rendered with the wrong
//     value, and Authelia restart-loops on the resulting hash.
//   * finalizeWireNPM: a transient 401 inside the loop, or a typed-decode
//     error misclassified as success, can leave fewer than four hosts in
//     the database.
//   * finalizeWriteAdmin: a flush() that produces an empty users mapping
//     (zero-value bug regression) leaves a syntactically-valid YAML with
//     no operator entry.
//
// Each verifyXxx below re-derives the observable, on-disk / over-the-wire
// outcome — never trusting in-memory state from the step function. They
// run AFTER the step (so a successful step proves itself) AND once more in
// runFinalizeVerifyAll just before touching the sentinel (defence in depth
// against orchestration bugs falling through). If any verification fails,
// the corresponding step's CompletedSteps flag stays false and the SSE
// stream surfaces a clear error.
//
// Stdlib + the same yaml.v3 / pbkdf2 deps the rest of the package uses;
// no shell-out, no docker exec.

package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// finalizeVerifyResult is purely informational — surfaces as the message
// in the SSE error event for the failing step. Returned errors from each
// verifyXxx wrap one of these.

// verifyUsersDB asserts that the YAML on-disk parses cleanly AND contains
// the operator's admin username under the top-level `users:` mapping.
func (s *server) verifyUsersDB(adminUsername string) error {
	if adminUsername == "" {
		return errors.New("verify users_db: admin username unset in state.json")
	}
	b, err := os.ReadFile(s.cfg.usersDBPath)
	if err != nil {
		return fmt.Errorf("verify users_db: read %s: %w", s.cfg.usersDBPath, err)
	}
	var doc struct {
		Users map[string]map[string]any `yaml:"users"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("verify users_db: parse %s: %w", s.cfg.usersDBPath, err)
	}
	if _, ok := doc.Users[adminUsername]; !ok {
		return fmt.Errorf("verify users_db: admin %q not present in %s "+
			"(file parses but the upsert silently produced an empty mapping)",
			adminUsername, s.cfg.usersDBPath)
	}
	return nil
}

// (Pre-Wave-1: verifySharedVolume asserted the LUKS keyfile + mapper
// mountpoint for the system-wide /shared volume copyparty bind-mounted.
// With Nextcloud-managed cloud-data there's no shared LUKS volume to
// verify — Nextcloud's group-folders app provides shared space inside
// the regular datadir.)

// unrendered matches an envsubst-style placeholder of the form
// ${SHOUTY_SNAKE_CASE} — the exact shape envsubst leaves behind when an
// expected variable was unset. Tightened from a substring check on `${`
// so this doesn't false-positive on configs that legitimately use shell
// or nginx-style $variable syntax.
var unrendered = regexp.MustCompile(`\$\{[A-Z_][A-Z0-9_]*\}`)

// verifyTemplatesRendered asserts the rendered Authelia configuration.yml
// contains NO bootstrap-placeholder strings AND that the OIDC console
// client_secret is a syntactically-valid pbkdf2-sha512 hash. It also
// scans for any leftover envsubst placeholder of the form ${UPPER_NAME}
// (see the unrendered regex above) — that's the "envsubst silently
// blanked an unset var" failure mode. This is the check that would have
// caught the silent failure: render-templates.sh happily substituted
// `$pbkdf2-sha512$bootstrap-placeholder` from the env var into the YAML,
// exited 0, and Authelia then refused to start.
func (s *server) verifyTemplatesRendered() error {
	cfgPath := filepath.Join(s.cfg.stacksDir, "authelia/configuration.yml")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("verify templates: read %s: %w", cfgPath, err)
	}
	// Strip comments before scanning for the placeholder. The committed
	// configuration.yml.template has the literal "bootstrap-placeholder"
	// in a comment block explaining the bootstrap flow; that text is
	// preserved by envsubst and we MUST NOT match against it.
	scrubbed := stripYAMLComments(string(b))
	if strings.Contains(scrubbed, "bootstrap-placeholder") {
		return fmt.Errorf("verify templates: %s still contains 'bootstrap-placeholder' "+
			"(OIDC client_secret rotation was skipped or failed)", cfgPath)
	}
	if strings.Contains(scrubbed, "REPLACE_ME") {
		return fmt.Errorf("verify templates: %s still contains 'REPLACE_ME' tokens", cfgPath)
	}
	// Scan the file bytes for an envsubst-style ${UPPER_NAME} placeholder.
	if m := unrendered.Find([]byte(scrubbed)); m != nil {
		return fmt.Errorf("verify templates: %s has unsubstituted %s token", cfgPath, string(m))
	}
	// Dig out the OIDC client_secret line and pbkdf2-validate it.
	hash := extractOIDCClientSecret(string(b))
	if hash == "" {
		return fmt.Errorf("verify templates: cannot locate identity_providers.oidc.clients[].client_secret in %s", cfgPath)
	}
	if !pbkdf2HashRe.MatchString(hash) {
		return fmt.Errorf("verify templates: OIDC client_secret in %s is not a valid pbkdf2-sha512 hash: %q", cfgPath, hash)
	}
	return nil
}

// stripYAMLComments returns content with everything from `#` to end-of-line
// removed. Preserves whitespace + newlines so line numbers match.
// Naive (doesn't handle `#` inside quoted strings), but sufficient for
// scanning Authelia's config which only uses `#` for comments.
func stripYAMLComments(content string) string {
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if idx := strings.Index(line, "#"); idx >= 0 {
			b.WriteString(line[:idx])
		} else {
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// extractOIDCClientSecret pulls the (single-quoted) client_secret value
// out of the Authelia config. Returns the unquoted string, or "" if the
// line isn't found. We use a line-scan rather than yaml.Unmarshal because
// the Authelia config uses the `secret` template func (e.g.
// `key: {{ secret "/path" | mindent 10 "|" | msquote }}`) which yaml.v3
// can't parse — it's intentionally not valid YAML pre-template-rendering.
func extractOIDCClientSecret(content string) string {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "client_secret:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "client_secret:"))
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			return val[1 : len(val)-1]
		}
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			return val[1 : len(val)-1]
		}
		return val
	}
	return ""
}

// verifyCertIssued asserts the fullchain.pem exists, parses, and contains
// SANs covering at least the apex and one subdomain (`*.<apex>`). The
// per-domain check defends against a partial certbot run that only got the
// apex (which would later 502 every subdomain).
func (s *server) verifyCertIssued(domain string) error {
	if domain == "" {
		return errors.New("verify cert: domain unset in state.json")
	}
	// Path matches the bind-mount in stacks/ingress/docker-compose.yml.
	// /etc/letsencrypt on disk maps to <stacksDir>/ingress/letsencrypt.
	chainPath := filepath.Join(s.cfg.stacksDir, "ingress/letsencrypt/live", domain, "fullchain.pem")
	b, err := os.ReadFile(chainPath)
	if err != nil {
		return fmt.Errorf("verify cert: read %s: %w", chainPath, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return fmt.Errorf("verify cert: %s is not PEM-encoded", chainPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("verify cert: parse %s: %w", chainPath, err)
	}
	wantApex := domain
	wantWildcard := "*." + domain
	hasApex, hasWildcard := false, false
	for _, dns := range cert.DNSNames {
		if dns == wantApex {
			hasApex = true
		}
		if dns == wantWildcard {
			hasWildcard = true
		}
	}
	if !hasApex || !hasWildcard {
		return fmt.Errorf("verify cert: %s SANs %v missing %q or %q", chainPath, cert.DNSNames, wantApex, wantWildcard)
	}
	return nil
}

// verifyNPMRoutes asserts the five required proxy hosts exist in NPM. Logs
// in with the wizard-rotated admin (operator's email + plaintext from the
// in-memory cache); if the cache is empty (post-finalize wipe, or operator
// didn't re-walk /setup/admin) returns a clear "re-enter password" error.
//
// We deliberately call the live NPM API rather than reading the SQLite DB:
// the wire-up step talks to the API too, so a verification that reads the
// same surface catches API/db divergence (e.g. rolled-back transactions).
func (s *server) verifyNPMRoutes(ctx context.Context, st *setupState) error {
	if st.AdminUsername == "" || st.AdminEmail == "" || st.Domain == "" {
		return errors.New("verify npm_routes: admin/email/domain unset in state.json")
	}
	pw := setupSecretCache.get(st.AdminUsername)
	if pw == "" {
		return errors.New("verify npm_routes: admin password missing from in-memory cache " +
			"(restart wizard at /setup/admin to re-enter, then re-run finalize)")
	}
	npm := NewNPMClient(finalizeNPMBaseURL(), nil)
	if err := npm.Login(ctx, st.AdminEmail, []byte(pw)); err != nil {
		return fmt.Errorf("verify npm_routes: login as %s: %w", st.AdminEmail, err)
	}
	hosts, err := npm.ListProxyHosts(ctx)
	if err != nil {
		return fmt.Errorf("verify npm_routes: list: %w", err)
	}
	want := []string{
		"auth." + st.Domain,
		"enrol." + st.Domain,
		"cloud." + st.Domain,
		"console." + st.Domain,
		"task." + st.Domain,
	}
	have := map[string]bool{}
	for _, h := range hosts {
		for _, d := range h.DomainNames {
			have[d] = true
		}
	}
	var missing []string
	for _, d := range want {
		if !have[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("verify npm_routes: NPM is missing proxy host(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// verifySentinel asserts the sentinel file exists and is non-empty (the
// timestamp we wrote). Trivial check — this is the very last step, so a
// failure here means the disk is full or the path is read-only.
func (s *server) verifySentinel() error {
	info, err := os.Stat(s.cfg.setupCompleteSentinel)
	if err != nil {
		return fmt.Errorf("verify sentinel: %s: %w", s.cfg.setupCompleteSentinel, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("verify sentinel: %s is empty (expected RFC3339 timestamp)", s.cfg.setupCompleteSentinel)
	}
	return nil
}

// finalizeVerifyAll re-runs every per-step verification in dependency
// order. Returns the FIRST failure (with its step tag) so the SSE stream
// renders a clear "stuck on <step>" message. Used as the pre-touch-sentinel
// belt-and-braces check.
//
// Excludes verifySentinel (the sentinel doesn't exist yet at this point —
// that's the point of running this BEFORE touching it).
func (s *server) finalizeVerifyAll(ctx context.Context, st *setupState) *finalizeErr {
	if err := s.verifyUsersDB(st.AdminUsername); err != nil {
		return &finalizeErr{Step: "users_db", Message: err.Error()}
	}
	if err := s.verifyTemplatesRendered(); err != nil {
		return &finalizeErr{Step: "templates", Message: err.Error()}
	}
	if err := s.verifyCertIssued(st.Domain); err != nil {
		return &finalizeErr{Step: "cert", Message: err.Error()}
	}
	if err := s.verifyNPMRoutes(ctx, st); err != nil {
		return &finalizeErr{Step: "npm", Message: err.Error()}
	}
	return nil
}

// ---- helpers ----------------------------------------------------------

// reloadAuthelia kicks the authelia container so it picks up a freshly-
// rendered configuration.yml. Best-effort: if `docker` is missing or the
// container isn't running yet the function returns nil — the templates
// step's verification has already proven the on-disk file is correct.
//
// Used as a side-effect of the templates step (not as a verification).
// Without it, Authelia would stay restart-looped against the placeholder
// even though the file on disk is now correct, until the operator manually
// `docker compose restart`s.
func reloadAuthelia(ctx context.Context, logLine func(string)) {
	if testModeEnabled() {
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return
	}
	cmd := exec.CommandContext(ctx, "docker", "restart", "authelia")
	out, err := cmd.CombinedOutput()
	if err != nil {
		logLine(fmt.Sprintf("authelia restart skipped/failed (will retry on next finalize): %v: %s", err, strings.TrimSpace(string(out))))
		return
	}
	logLine("authelia container restarted to pick up new configuration.yml")
}

