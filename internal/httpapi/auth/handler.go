package authapi

import (
	"net/http"
	"time"

	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/httpapi/middleware"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
	"livetranslate/server/internal/token"
)

// Handler owns the /v1/auth routes and the Bearer-token user resolution
// shared with the sync/account routers.
type Handler struct {
	cfg           *config.Config
	auth          *auth.Service
	tokens        *token.Manager
	db            *store.DB
	trust         *middleware.ProxyTrust
	limiter       *middleware.RateLimiter
	resendLimiter *middleware.RateLimiter
}

func NewHandler(cfg *config.Config, svc *auth.Service, tokens *token.Manager, db *store.DB, trust *middleware.ProxyTrust) *Handler {
	return &Handler{
		cfg:           cfg,
		auth:          svc,
		tokens:        tokens,
		db:            db,
		trust:         trust,
		limiter:       middleware.NewRateLimiter(cfg.AuthRateLimitPerMinute, true),
		resendLimiter: middleware.NewRateLimiterWindow(5, 10*time.Minute),
	}
}

// DeviceInput is the wire shape of the device payload (mirrors the Python
// DeviceInfo schema).
type DeviceInput struct {
	ClientDeviceID string `json:"clientDeviceId"`
	DisplayName    string `json:"displayName"`
	AppVersion     string `json:"appVersion"`
}

// DeviceInfo converts the wire shape into the sync DTO.
func (d DeviceInput) DeviceInfo() syncpkg.DeviceInfo {
	return syncpkg.DeviceInfo{
		ClientDeviceID: d.ClientDeviceID,
		DisplayName:    d.DisplayName,
		AppVersion:     d.AppVersion,
	}
}

// RefreshRequestWire / LogoutRequestWire mirror the Python request schemas.
type RefreshRequestWire struct {
	RefreshToken string `json:"refreshToken"`
}

type LogoutRequestWire struct {
	RefreshToken string `json:"refreshToken"`
	RevokeDevice bool   `json:"revokeDevice"`
}

// ClientIP resolves the request's client IP through the shared proxy-trust
// configuration (X-Forwarded-For only from trusted peers).
func (h *Handler) ClientIP(r *http.Request) string {
	return middleware.ClientIP(r, h.trust)
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/register", h.register)
	mux.HandleFunc("POST /v1/auth/email/verify", h.verifyEmail)
	mux.HandleFunc("POST /v1/auth/email/resend", h.resend)
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
	mux.HandleFunc("POST /v1/auth/logout", h.logout)
	mux.HandleFunc("POST /v1/auth/logout-all", h.logoutAll)
	mux.HandleFunc("POST /v1/auth/password/forgot", h.forgot)
	mux.HandleFunc("POST /v1/auth/password/reset", h.reset)
	mux.HandleFunc("POST /v1/auth/apple", h.apple)
	mux.HandleFunc("POST /v1/auth/dev", h.devLogin)
	// Me / password-change / devices live under /v1/me per the spec.
	mux.HandleFunc("GET /v1/me", h.me)
	mux.HandleFunc("GET /v1/me/password/change", h.getMethodNotAllowed)
	mux.HandleFunc("POST /v1/me/password/change", h.changePassword)
	mux.HandleFunc("GET /v1/me/devices", h.listDevices)
	mux.HandleFunc("DELETE /v1/me/devices/{deviceID}", h.revokeDevice)
}

func (h *Handler) getMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	httpapiWriteDetail(w, http.StatusMethodNotAllowed, "method not allowed")
}
