package authapi

import (
	"errors"
	"net/http"
	"strings"

	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/httpapi"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
)

// syncDevicePayload is the wire shape of the optional device field on
// profile flows that re-issue tokens (email verify) — same fields as the
// login device payload.
type syncDevicePayload struct {
	ClientDeviceID string `json:"clientDeviceId"`
	DisplayName    string `json:"displayName"`
	AppVersion     string `json:"appVersion"`
}

func (d syncDevicePayload) DeviceInfo() syncpkg.DeviceInfo {
	return syncpkg.DeviceInfo{
		ClientDeviceID: d.ClientDeviceID,
		DisplayName:    d.DisplayName,
		AppVersion:     d.AppVersion,
	}
}

// --- PATCH /v1/me (display name) ------------------------------------------------

// updateProfileRequest is deliberately narrow: only the display name is
// accepted. role/status/userId are NEVER read from the wire.
type updateProfileRequest struct {
	DisplayName *string `json:"displayName"`
}

func (h *Handler) patchMe(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	var req updateProfileRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	if req.DisplayName == nil {
		httpapi.WriteDetail(w, http.StatusBadRequest, "displayName is required")
		return
	}
	ip := h.ClientIP(r)
	user, err := h.auth.UpdateProfile(r.Context(), ac.User.ID, *req.DisplayName, ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, user)
}

// --- GET /v1/me (enriched profile) ------------------------------------------------

func (h *Handler) meProfile(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	profile, err := h.auth.GetMeProfile(r.Context(), ac.User.ID)
	if err != nil {
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, profile)
}

// --- POST /v1/me/email/change (step 1: re-auth + code to the NEW address) --------

type emailChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewEmail        string `json:"newEmail"`
}

func (h *Handler) requestEmailChange(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	var req emailChangeRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	if req.CurrentPassword == "" || strings.TrimSpace(req.NewEmail) == "" {
		httpapi.WriteDetail(w, http.StatusBadRequest, "currentPassword and newEmail are required")
		return
	}
	ip := h.ClientIP(r)
	state, err := h.auth.RequestEmailChange(r.Context(), ac.User.ID, req.CurrentPassword, req.NewEmail, ip)
	if err != nil {
		// ErrEmailTaken is handled HERE, not in mapAuthError: registration
		// maps it to the anti-enumeration 200, but this caller is an
		// authenticated user changing their OWN address — a plain 409 is
		// correct and creates no enumeration surface.
		if errors.Is(err, auth.ErrEmailTaken) {
			httpapi.WriteDetail(w, http.StatusConflict, "this email is already in use")
			return
		}
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":        true,
		"targetEmail": state.TargetEmail,
		"expiresAt":   state.ExpiresAt,
	})
}

// --- POST /v1/me/email/verify (step 2: consume code, swap email, rotate tokens) ---

type emailVerifyRequest struct {
	Code   string            `json:"code"`
	Device syncDevicePayload `json:"device"`
}

func (h *Handler) verifyEmailChange(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	var req emailVerifyRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := h.ClientIP(r)
	resp, err := h.auth.VerifyEmailChange(r.Context(), ac.User.ID, ac.Device, req.Code, req.Device.DeviceInfo(), ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

// --- POST /v1/me/apple/bind --------------------------------------------------------

type appleBindRequest struct {
	IdentityToken string `json:"identityToken"`
}

func (h *Handler) bindApple(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	var req appleBindRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	if req.IdentityToken == "" {
		httpapi.WriteDetail(w, http.StatusBadRequest, "missing identity token")
		return
	}
	sub, err := VerifyAppleIdentity(r.Context(), h.cfg, req.IdentityToken)
	if err != nil {
		var ate *AppleTokenError
		msg := "token failed verification"
		if errors.As(err, &ate) {
			msg = ate.Msg
		}
		httpapi.WriteDetail(w, http.StatusUnauthorized, msg)
		return
	}
	ip := h.ClientIP(r)
	if err := h.auth.BindApple(r.Context(), ac.User.ID, sub, ip); err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"bound": true})
}

// --- DELETE /v1/me/apple (unbind, password re-verification required) --------------

type appleUnbindRequest struct {
	CurrentPassword string `json:"currentPassword"`
}

func (h *Handler) unbindApple(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	var req appleUnbindRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	if req.CurrentPassword == "" {
		httpapi.WriteDetail(w, http.StatusBadRequest, "currentPassword is required")
		return
	}
	ip := h.ClientIP(r)
	if err := h.auth.UnbindApple(r.Context(), ac.User.ID, req.CurrentPassword, ip); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpapi.WriteDetail(w, http.StatusNotFound, "no Apple account is linked")
			return
		}
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"bound": false})
}
