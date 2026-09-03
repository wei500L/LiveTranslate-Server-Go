// Package accountapi hosts the /v1/account routes kept for iOS protocol
// compatibility: /me (identity probe), /cloud-data (purge synced classroom
// data, keep the account) and DELETE "" (delete the account entirely).
package accountapi

import (
	"net/http"

	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/httpapi"
	authapi "livetranslate/server/internal/httpapi/auth"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
)

type Handler struct {
	db   *store.DB
	auth *authapi.Handler
	svc  *auth.Service
}

func NewHandler(db *store.DB, auth *authapi.Handler, svc *auth.Service) *Handler {
	return &Handler{db: db, auth: auth, svc: svc}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/account/me", h.me)
	mux.HandleFunc("DELETE /v1/account/cloud-data", h.deleteCloudData)
	mux.HandleFunc("DELETE /v1/account", h.deleteAccount)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, syncpkg.MeResponse{
		UserID:       ac.User.ID.String(),
		DisplayLabel: ac.User.DisplayLabel(),
		CreatedAt:    ac.User.CreatedAt,
	})
}

// deleteCloudData removes the user's synced classroom data on the server.
// The account and devices survive; local iOS data is untouched by design.
func (h *Handler) deleteCloudData(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return
	}
	if err := store.PurgeUserSyncData(r.Context(), h.db.Q(), ac.User.ID); err != nil {
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteAccount revokes every refresh token, hard-deletes the synced
// classroom data and soft-deletes the user row (Apple `sub` stays
// tombstoned so a re-login creates a fresh, empty account rather than
// resurrecting old data).
func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return
	}
	ip := h.auth.ClientIP(r)
	if err := h.svc.DeleteAccount(r.Context(), ac.User.ID, ip); err != nil {
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
