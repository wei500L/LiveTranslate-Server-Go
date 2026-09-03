// Package webapp serves the small public web surfaces of the API host:
// the password-reset deep-link landing page and the Apple app-site
// association file. Pages carry no third-party scripts, no tracking, and
// never log the token.
package webapp

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"livetranslate/server/internal/config"
	"livetranslate/server/internal/store"
	"livetranslate/server/internal/token"
)

//go:embed templates/*.html
var templateFS embed.FS

// Handler owns the public web routes (mounted on the API listener).
type Handler struct {
	cfg *config.Config
	db  *store.DB
	tpl *template.Template
}

func NewHandler(cfg *config.Config, db *store.DB) *Handler {
	tpl := template.Must(template.ParseFS(templateFS, "templates/*.html"))
	return &Handler{cfg: cfg, db: db, tpl: tpl}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/apple-app-site-association", h.aasa)
	mux.HandleFunc("GET "+h.resetPath(), h.resetLanding)
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

// resetLanding is the page a reset link opens when the app is NOT installed
// (or when opened from a desktop browser). It states the token's status
// (valid / expired / used / invalid) and offers two paths:
//   - open the reset in the app (universal link + custom-scheme fallback),
//   - copy the token for manual paste in the app.
//
// The token itself is high-entropy, single-use and short-lived, and the
// page never logs it.
func (h *Handler) resetLanding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'none'; img-src 'none'; frame-ancestors 'none'")
	// No caching: token state must be evaluated per request.
	w.Header().Set("Cache-Control", "no-store")

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

func (h *Handler) renderLanding(w http.ResponseWriter, status int, state, rawToken string) {
	data := map[string]any{
		"State": state,
		// Custom-scheme fallback works even before the associated-domain
		// entitlement is configured (the web page bridges to the app).
		"SchemeLink": "livetranslate://reset-password?token=" + rawToken,
		"Token":      rawToken,
	}
	w.WriteHeader(status)
	if err := h.tpl.ExecuteTemplate(w, "reset_landing.html", data); err != nil {
		slog.Error("reset landing render failed", "err", err.Error())
	}
}
