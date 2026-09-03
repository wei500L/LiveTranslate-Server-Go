package authapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/httpapi"
	"livetranslate/server/internal/httpapi/middleware"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
)

// httpapi helpers re-exported for brevity in this package.
func httpapiWriteDetail(w http.ResponseWriter, status int, detail any) {
	httpapi.WriteDetail(w, status, detail)
}

// allowIP applies the per-IP limiter; on limit → 429 with Retry-After.
func (h *Handler) allowIP(w http.ResponseWriter, r *http.Request) bool {
	ip := middleware.ClientIP(r, h.trust)
	ok, retry := h.limiter.Allow("ip", ip)
	if !ok {
		w.Header().Set("Retry-After", retry.Round(time.Second).String())
		httpapi.WriteDetail(w, http.StatusTooManyRequests, "too many requests")
		return false
	}
	return true
}

// --- Register -----------------------------------------------------------------

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req auth.RegisterRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	_, err := h.auth.Register(r.Context(), &req, ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	// Generic success regardless of email-existence (anti-enumeration). The
	// body must be byte-identical for taken and fresh emails: no userId.
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":   true,
		"detail": "If the email is valid, a verification code has been sent.",
	})
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req auth.VerifyEmailRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	resp, err := h.auth.VerifyEmail(r.Context(), &req, ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) resend(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req auth.ResendRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	// Per-email resend limiter (stricter than the IP window).
	if ok, retry := h.resendLimiter.Allow("resend", auth.NormalizeEmail(req.Email)); !ok {
		w.Header().Set("Retry-After", retry.Round(time.Second).String())
		httpapi.WriteDetail(w, http.StatusTooManyRequests, "please wait before requesting another code")
		return
	}
	if err := h.auth.Resend(r.Context(), &req, ip); err != nil {
		// The DB-level cooldown is surfaced so the client can disable the
		// resend button for real; without it the response would claim a code
		// was sent when none was. Other outcomes stay uniform (anti-
		// enumeration).
		if errors.Is(err, auth.ErrResendCooldown) {
			w.Header().Set("Retry-After", h.cfg.ResendCooldown.Round(time.Second).String())
			httpapi.WriteDetail(w, http.StatusTooManyRequests, "please wait before requesting another code")
			return
		}
		if errors.Is(err, auth.ErrNoMailTransport) {
			h.mapAuthError(w, r, err)
			return
		}
	}
	// Uniform response (cooldowns included).
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":   true,
		"detail": "If the email is valid, a verification code has been sent.",
	})
}

// --- Login / tokens -------------------------------------------------------------

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req auth.LoginRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	resp, err := h.auth.Login(r.Context(), &req, ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req RefreshRequestWire
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	resp, err := h.auth.Refresh(r.Context(), req.RefreshToken, ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req LogoutRequestWire
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	_ = h.auth.Logout(r.Context(), req.RefreshToken, req.RevokeDevice)
	// Idempotent: unknown tokens still 204 (matches Python).
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	if err := h.auth.LogoutAllUser(r.Context(), ac.User.ID, ip); err != nil {
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Password flows ---------------------------------------------------------------

func (h *Handler) forgot(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req auth.ForgotPasswordRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	if ok, retry := h.resendLimiter.Allow("forgot", auth.NormalizeEmail(req.Email)); !ok {
		w.Header().Set("Retry-After", retry.Round(time.Second).String())
		httpapi.WriteDetail(w, http.StatusTooManyRequests, "please wait before retrying")
		return
	}
	_ = h.auth.ForgotPassword(r.Context(), req.Email, ip)
	// ALWAYS the unified response.
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":   true,
		"detail": "If the email corresponds to a valid account, we have sent reset instructions.",
	})
}

func (h *Handler) reset(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req auth.ResetPasswordRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	if err := h.auth.ResetPassword(r.Context(), &req, ip); err != nil {
		if errors.Is(err, auth.ErrBadCode) {
			httpapi.WriteDetail(w, http.StatusBadRequest, "invalid or expired reset token")
			return
		}
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"reset": true})
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	var req auth.ChangePasswordRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	if err := h.auth.ChangePassword(r.Context(), ac.User.ID, ac.Device, &req, ip); err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"changed":              true,
		"otherSessionsRevoked": true,
	})
}

// --- Me & devices -------------------------------------------------------------------

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, syncpkg.MeResponse{
		UserID:       ac.User.ID.String(),
		DisplayLabel: ac.User.DisplayLabel(),
		CreatedAt:    ac.User.CreatedAt,
	})
}

func (h *Handler) listDevices(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	devices, err := h.auth.ListDevices(r.Context(), ac.User.ID, ac.Device)
	if err != nil {
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (h *Handler) revokeDevice(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.RequireUser(w, r)
	if !ok {
		return
	}
	deviceID, err := uuid.Parse(r.PathValue("deviceID"))
	if err != nil {
		httpapi.WriteDetail(w, http.StatusBadRequest, "invalid device id")
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	if err := h.auth.RevokeDevice(r.Context(), ac.User.ID, deviceID, ip); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpapi.WriteDetail(w, http.StatusNotFound, "device not found")
			return
		}
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Apple (preserved) ---------------------------------------------------------------

type appleRequest struct {
	IdentityToken     string      `json:"identityToken"`
	AuthorizationCode string      `json:"authorizationCode,omitempty"`
	Device            DeviceInput `json:"device"`
}

func (h *Handler) apple(w http.ResponseWriter, r *http.Request) {
	if !h.allowIP(w, r) {
		return
	}
	var req appleRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	if req.IdentityToken == "" {
		httpapi.WriteDetail(w, http.StatusUnauthorized, "missing identity token")
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
	ip := middleware.ClientIP(r, h.trust)
	resp, created, err := h.auth.AppleLogin(r.Context(), sub, req.Device.DeviceInfo(), ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	resp.IsNewUser = created
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

// --- Dev login (gated) -----------------------------------------------------------------

type devRequest struct {
	DevName string      `json:"devName"`
	Device  DeviceInput `json:"device"`
}

func (h *Handler) devLogin(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.DevLoginEnabled {
		httpapi.WriteDetail(w, http.StatusNotFound, "Not Found")
		return
	}
	var req devRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	ip := middleware.ClientIP(r, h.trust)
	resp, err := h.auth.DevLogin(r.Context(), req.DevName, req.Device.DeviceInfo(), ip)
	if err != nil {
		h.mapAuthError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

// --- Error mapping ---------------------------------------------------------------------

func (h *Handler) mapAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		httpapi.WriteDetail(w, http.StatusUnauthorized, "invalid email or password")
	case errors.Is(err, auth.ErrNotVerified):
		httpapi.WriteDetail(w, http.StatusForbidden, "email not verified")
	case errors.Is(err, auth.ErrSuspended):
		httpapi.WriteDetail(w, http.StatusForbidden, "account is not available")
	case errors.Is(err, auth.ErrEmailTaken):
		// Anti-enumeration: registration "succeeds" with the generic
		// response — byte-identical to the fresh-signup success.
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{
			"sent":   true,
			"detail": "If the email is valid, a verification code has been sent.",
		})
	case errors.Is(err, auth.ErrWeakPassword):
		httpapi.WriteDetail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrBadCode):
		httpapi.WriteDetail(w, http.StatusBadRequest, "invalid or expired code")
	case errors.Is(err, auth.ErrTooManyAttempts):
		httpapi.WriteDetail(w, http.StatusTooManyRequests, "too many attempts, request a new code")
	case errors.Is(err, auth.ErrResendCooldown):
		httpapi.WriteDetail(w, http.StatusTooManyRequests, "please wait before requesting another code")
	case errors.Is(err, auth.ErrRateLimited):
		httpapi.WriteDetail(w, http.StatusTooManyRequests, "too many attempts, try later")
	case errors.Is(err, auth.ErrNoMailTransport):
		httpapi.WriteDetail(w, http.StatusServiceUnavailable, "mail transport unavailable")
	case errors.Is(err, store.ErrReuseDetected),
		errors.Is(err, store.ErrTokenExpired),
		errors.Is(err, store.ErrAccountUnavailable),
		errors.Is(err, store.ErrNotFound):
		httpapi.WriteDetail(w, http.StatusUnauthorized, rotateErrText(err))
	default:
		slog.Error("auth handler error",
			"request_id", httpapi.RequestID(r.Context()),
			"path", r.URL.Path, "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
	}
}

func rotateErrText(err error) string {
	var re *store.RotateError
	if errors.As(err, &re) {
		return re.Msg
	}
	return "invalid credentials"
}
