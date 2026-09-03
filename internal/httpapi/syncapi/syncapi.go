// Package syncapi hosts the /v1/sync routes: push (idempotent, conflict-
// aware writes), pull (change_sequence cursor) and status. Every route
// requires an active user via the shared auth handler.
package syncapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"livetranslate/server/internal/config"
	"livetranslate/server/internal/httpapi"
	authapi "livetranslate/server/internal/httpapi/auth"
	"livetranslate/server/internal/sync"
)

type Handler struct {
	cfg  *config.Config
	sync *sync.Service
	auth *authapi.Handler
}

func NewHandler(cfg *config.Config, svc *sync.Service, auth *authapi.Handler) *Handler {
	return &Handler{cfg: cfg, sync: svc, auth: auth}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sync/push", h.push)
	mux.HandleFunc("GET /v1/sync/pull", h.pull)
	mux.HandleFunc("GET /v1/sync/status", h.status)
}

// push applies a batch of operations. Schema versions the server cannot
// understand are rejected with 400 + errorCode client_schema_unsupported so
// iOS shows 需要更新 App instead of 网络错误.
func (h *Handler) push(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return
	}
	var req sync.PushRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		return
	}
	if req.SchemaVersion < h.cfg.MinClientSchemaVersion ||
		req.SchemaVersion > h.cfg.MaxClientSchemaVersion {
		httpapi.WriteDetail(w, http.StatusBadRequest, map[string]any{
			"errorCode":              "client_schema_unsupported",
			"minClientSchemaVersion": h.cfg.MinClientSchemaVersion,
			"maxClientSchemaVersion": h.cfg.MaxClientSchemaVersion,
		})
		return
	}
	results, err := h.sync.ApplyPush(r.Context(), ac.User.ID, req.Operations)
	if err != nil {
		slog.Error("push failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, sync.PushResponse{
		SchemaVersion: h.cfg.SchemaVersion,
		Results:       results,
		ServerTime:    time.Now(),
	})
}

// pull streams changes strictly after the cursor (change_sequence > cursor).
func (h *Handler) pull(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return
	}
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	if cursor < 0 {
		cursor = 0
	}
	limit := 200
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			limit = v
		}
	}
	// Service clamps to [1, 500] (mirrors the Python Query bounds).
	resp, err := h.sync.Pull(r.Context(), ac.User.ID, cursor, limit)
	if err != nil {
		slog.Error("pull failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return
	}
	resp, err := h.sync.Status(r.Context(), ac.User.ID)
	if err != nil {
		slog.Error("status failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, resp)
}
