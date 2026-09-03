// Package webapp serves the small public web surfaces of the API host:
// the password-reset deep-link landing page and the Apple app-site
// association file. Pages carry no third-party scripts, no tracking, and
// never log the token.
//
// Security posture: the reset credential reaches the app ONLY through the
// HTTPS universal link (the link the user taps in Mail opens the app
// directly when installed). This web page exists for the no-app case; it
// shows the token's state and offers the ONE-TIME manual credential for
// copy-paste. It never embeds the token in a custom scheme URL, never
// auto-copies anything, and passes no secret to any redirect.
package webapp

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/httpapi/middleware"
	"livetranslate/server/internal/store"
	"livetranslate/server/internal/token"
)

//go:embed templates/*.html
var templateFS embed.FS

// Handler owns the public web routes (mounted on the API listener).
type Handler struct {
	cfg     *config.Config
	db      *store.DB
	auth    *auth.Service
	trust   *middleware.ProxyTrust
	limiter *middleware.RateLimiter
	tpl     *template.Template
}

func NewHandler(cfg *config.Config, db *store.DB, authSvc *auth.Service, trust *middleware.ProxyTrust) *Handler {
	tpl := template.Must(template.ParseFS(templateFS, "templates/*.html"))
	return &Handler{
		cfg: cfg, db: db, auth: authSvc, trust: trust,
		// Same per-minute budget as the auth endpoints for this public form.
		limiter: middleware.NewRateLimiter(cfg.AuthRateLimitPerMinute, true),
		tpl:     tpl,
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/apple-app-site-association", h.aasa)
	mux.HandleFunc("GET "+h.resetPath(), h.resetLanding)
	// Re-send entry for users who cannot get into the app: same flow as the
	// app's 忘记密码 (anti-enumeration, per-email cooldown enforced by the
	// auth service; this limiter bounds the public form itself).
	mux.HandleFunc("POST "+h.resetPath()+"/resend", h.resetResend)
}

func (h *Handler) resetPath() string {
	p := h.cfg.PasswordResetPath
	if p == "" {
		p = "/reset-password"
	}
	// A path without a leading slash can never be routed; normalize rather
	// than silently serving at the wrong place.
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// aasa serves the Apple app-site association document. The Team ID comes
// from AASA_TEAM_ID; when unset the document carries an EMPTY app list —
// a deliberately visible "not configured yet" state, never a guessed value.
// Content-Type is application/json per Apple's docs (the AASA CDN accepts
// both, but json is the canonical form).
func (h *Handler) aasa(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if h.cfg.AASATeamID == "" || h.cfg.AppleBundleID == "" {
		// Not configured: serve a well-formed document with no entries so
		// clients (and Apple's CDN validation) see a valid, inert file.
		_, _ = w.Write([]byte(`{"applinks":{"details":[]},"webcredentials":{"apps":[]}}`))
		return
	}
	doc := `{"applinks":{"details":[{"appIDs":["` + h.cfg.AASATeamID + `.` + h.cfg.AppleBundleID +
		`"],"components":[{"":"/reset-password","comment":"LiveTranslate password reset"}]}]},` +
		`"webcredentials":{"apps":["` + h.cfg.AASATeamID + `.` + h.cfg.AppleBundleID + `"]}}`
	_, _ = w.Write([]byte(doc))
}

// pageSecurityHeaders are page-level additions on top of httpapi.Handler's
// set. CSP allows inline styles for the page chrome and nothing else — no
// scripts, no remote origins, no form-action exfiltration targets.
func pageSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'none'; connect-src 'none'; img-src 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
}

// resetLanding is the page a reset link opens when the app is NOT installed
// (or when opened from a desktop browser). It states the token's status
// (valid / expired / used / invalid), offers the manual one-time credential
// and the re-send entry. The token itself is high-entropy, single-use and
// short-lived, and the page never logs it.
func (h *Handler) resetLanding(w http.ResponseWriter, r *http.Request) {
	pageSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	raw := strings.TrimSpace(r.URL.Query().Get("token"))
	if raw == "" || !isTokenShaped(raw) {
		h.renderLanding(w, http.StatusBadRequest, "invalid", raw)
		return
	}
	state, err := store.LookupPasswordResetTokenState(r.Context(), h.db.Q(), token.HashToken(raw))
	if err != nil {
		slog.Error("reset landing lookup failed", "err", err.Error())
		h.renderLanding(w, http.StatusInternalServerError, "invalid", raw)
		return
	}
	h.renderLanding(w, http.StatusOK, state.Status, raw)
}

// resetResend is the landing page's 重新发送 entry: a plain HTML form
// posting an email. It calls the SAME anti-enumeration forgot-password flow
// as the app; the response page is identical whether or not the account
// exists.
func (h *Handler) resetResend(w http.ResponseWriter, r *http.Request) {
	pageSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	ip := middleware.ClientIP(r, h.trust)
	if ok, retry := h.limiter.Allow("reset-web", ip); !ok {
		w.Header().Set("Retry-After", retry.Round(time.Second).String())
		h.renderLanding(w, http.StatusTooManyRequests, "ratelimited", "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderLanding(w, http.StatusBadRequest, "invalid", "")
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	if email == "" || len(email) > 320 || !auth.ValidEmail(email) {
		// Uniform page — no enumeration signal.
		h.renderLanding(w, http.StatusOK, "resent", "")
		return
	}
	// Anti-enum service: always nil error to the caller; outcome is uniform.
	_ = h.auth.ForgotPassword(r.Context(), email, ip)
	h.renderLanding(w, http.StatusOK, "resent", "")
}

// isTokenShaped rejects values that can never be a reset token (the real
// tokens are 64-char lowercase hex) before they touch the database.
func isTokenShaped(raw string) bool {
	if len(raw) != 64 {
		return false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// renderLanding renders the page for a given state. rawToken is only
// rendered for the "valid" state as the manual credential.
func (h *Handler) renderLanding(w http.ResponseWriter, status int, state, rawToken string) {
	data := map[string]any{
		"State": state,
		"Token": rawToken,
	}
	w.WriteHeader(status)
	if err := h.tpl.ExecuteTemplate(w, "reset_landing.html", data); err != nil {
		slog.Error("reset landing render failed", "err", err.Error())
	}
}
