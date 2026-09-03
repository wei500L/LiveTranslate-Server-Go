package authapi

import (
	"net/http"

	"livetranslate/server/internal/config"
	"livetranslate/server/internal/httpapi"
)

// capabilitiesResponse is the PUBLIC, unauthenticated description of what
// the server currently accepts. It intentionally contains ONLY booleans and
// the registration mode — never SMTP addresses, admin configuration,
// internal environment values or version details.
type capabilitiesResponse struct {
	// open | invite_only | disabled
	Registration string `json:"registration"`
	// Whether the registration flow requires an invitation code.
	RequiresInvitation bool `json:"requiresInvitation"`
	// Sign-in methods the server accepts.
	PasswordLogin bool `json:"passwordLogin"`
	AppleLogin    bool `json:"appleLogin"`
	// True while the operator flagged maintenance (reserved; the flag is
	// env-driven and off by default).
	Maintenance bool `json:"maintenance"`
	// Schema bounds so the client can show 需要更新 App before the first
	// push attempt.
	MinClientSchemaVersion int `json:"minClientSchemaVersion"`
	MaxClientSchemaVersion int `json:"maxClientSchemaVersion"`
}

// capabilities serves GET /v1/auth/capabilities. Public and cache-unfriendly
// (operators must be able to flip registration off instantly).
func (h *Handler) capabilities(w http.ResponseWriter, r *http.Request) {
	mode := h.cfg.RegistrationMode
	httpapi.WriteJSON(w, http.StatusOK, capabilitiesResponse{
		Registration:           mode,
		RequiresInvitation:     mode == config.RegistrationInviteOnly,
		PasswordLogin:          true,
		AppleLogin:             true,
		Maintenance:            false,
		MinClientSchemaVersion: h.cfg.MinClientSchemaVersion,
		MaxClientSchemaVersion: h.cfg.MaxClientSchemaVersion,
	})
}
