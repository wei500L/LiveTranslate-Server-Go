package authapi

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"livetranslate/server/internal/httpapi"
	"livetranslate/server/internal/store"
)

// AuthContext carries the resolved identity for a request: the user row and
// the device id from the access token.
type AuthContext struct {
	User   *store.User
	Device uuid.UUID
}

// RequireUser enforces Bearer access token → live user row, writing the
// 401 itself on failure. Users must be active — suspended, pending
// (unverified email), pending_deletion and deleted all refuse. This is the
// gate that keeps unverified emails from syncing.
func (h *Handler) RequireUser(w http.ResponseWriter, r *http.Request) (*AuthContext, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		httpapi.WriteDetail(w, http.StatusUnauthorized, "missing bearer token")
		return nil, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	claims, err := h.tokens.VerifyAccessToken(raw)
	if err != nil {
		httpapi.WriteDetail(w, http.StatusUnauthorized, "invalid access token")
		return nil, false
	}
	uid, err := uuid.Parse(claims.UserID)
	if err != nil {
		httpapi.WriteDetail(w, http.StatusUnauthorized, "malformed token subject")
		return nil, false
	}
	devID, err := uuid.Parse(claims.DeviceID)
	if err != nil {
		httpapi.WriteDetail(w, http.StatusUnauthorized, "malformed token subject")
		return nil, false
	}
	u, err := store.GetUserByID(r.Context(), h.db.Q(), uid)
	if err != nil || u == nil {
		httpapi.WriteDetail(w, http.StatusUnauthorized, "account unavailable")
		return nil, false
	}
	if u.DeletedAt != nil || u.Status != "active" {
		httpapi.WriteDetail(w, http.StatusUnauthorized, "account unavailable")
		return nil, false
	}
	return &AuthContext{User: u, Device: devID}, true
}
