// logout.go — single-click cross-app logout orchestrator.
//
// Authelia 4.39.x has no OIDC end_session_endpoint, no frontchannel /
// backchannel logout client config, and no SLO cascade — issue #5057
// remains open upstream. Without that, clicking Authelia's session-portal
// /logout only kills Authelia's cookie and leaves Nextcloud + Vikunja
// happily showing whoever was last in. Vikunja's session is a JWT in
// localStorage that nobody can clear from outside the Vikunja origin;
// Nextcloud's PHP session sticks until the cookie expires or the user
// hits Nextcloud's own /logout. Result: switching identity in enrol with
// the previous user's tabs still open visibly leaks the old session.
//
// Workaround: enrol owns the "Logout" link, and serves a tiny HTML page
// that fans the logout out across all three apps in one click. Browser
// orchestration is the only mechanism that touches every origin.
//
// The page does:
//   1. Two hidden iframes — one to Vikunja's frontend /logout (its SPA
//      route clears localStorage on load), one to Nextcloud's
//      /index.php/logout (best-effort; CSRF-gated so often a no-op, but
//      cheap to try).
//   2. After 1.5s (give the iframes time to do their work), redirect the
//      main tab to Authelia's /logout?rd=<launcher>.
//
// Why iframes can fail silently and we still call it a fix:
//   - Vikunja: same-origin localStorage wipe inside the iframe; this is
//     the load-bearing step.
//   - Nextcloud: X-Frame-Options: SAMEORIGIN blocks the iframe render,
//     but the GET still hits Nextcloud's PHP. The CSRF check on /logout
//     means the session usually doesn't actually clear — so a stale
//     Nextcloud cookie may linger until the next interactive logout from
//     inside Nextcloud or until the cookie expires (~24h default). This
//     is a known gap; documented in ADR-NEXT (and unblockable until
//     Authelia ships SLO).
//   - Authelia: redirect kills its session cookie unconditionally.
//
// Anyone hitting this URL — authenticated or not — gets the page; logout
// MUST work even when the session is already gone.

package main

import (
	"net/http"
)

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	d := s.cfg.domain
	page := `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<title>Logging out…</title>
<style>
  body { font-family: system-ui, -apple-system, sans-serif;
         text-align: center; margin-top: 6em; color: #555; }
  .spinner { display: inline-block; width: 28px; height: 28px;
             border: 3px solid #ddd; border-top-color: #777;
             border-radius: 50%; animation: spin 0.8s linear infinite;
             margin-bottom: 1em; }
  @keyframes spin { to { transform: rotate(360deg); } }
  iframe { position: absolute; left: -9999px; width: 1px; height: 1px; border: 0; }
</style>
</head><body>
<div class="spinner"></div>
<p>Logging you out of all apps…</p>
<iframe src="https://task.` + d + `/logout"></iframe>
<iframe src="https://cloud.` + d + `/index.php/logout"></iframe>
<script>
setTimeout(function () {
  window.location.href = "https://auth.` + d + `/logout?rd=https://enrol.` + d + `/";
}, 1500);
</script>
</body></html>`
	_, _ = w.Write([]byte(page))
}
