// Package modelapi hosts the /v1/models/ai routes: serving the
// pre-downloaded on-device AI models (offline translation GGUFs + the
// image-understanding model) to signed-in clients. The client verifies
// every byte against its own bundled manifest (SHA256 + size), so the
// server never needs to be trusted for integrity — only availability.
//
// Routes:
//
//	GET  /v1/models/ai                 catalog index + on-disk presence
//	GET  /v1/models/ai/{id}/{file}     the model bytes (Range supported)
//	HEAD /v1/models/ai/{id}/{file}
//
// Every route requires an active user (Bearer access token). The files
// are public upstream, but a private server should not leak bandwidth to
// anonymous clients.
package modelapi

import (
	"log/slog"
	"net/http"
	"os"

	"livetranslate/server/internal/httpapi"
	authapi "livetranslate/server/internal/httpapi/auth"
	"livetranslate/server/internal/modelstore"
)

type Handler struct {
	auth   *authapi.Handler
	store  *modelstore.Store
	logger *slog.Logger
}

func NewHandler(auth *authapi.Handler, store *modelstore.Store) *Handler {
	return &Handler{auth: auth, store: store, logger: slog.Default()}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/models/ai", h.index)
	mux.HandleFunc("GET /v1/models/ai/{id}/{file}", h.download)
	mux.HandleFunc("HEAD /v1/models/ai/{id}/{file}", h.download)
}

// index answers the catalog with each model's on-disk presence so the
// client can show 服务器已备/未备 before starting a multi-GB download.
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.RequireUser(w, r); !ok {
		return
	}
	type entry struct {
		ID        string `json:"id"`
		File      string `json:"file"`
		Bytes     int64  `json:"bytes"`
		SHA256    string `json:"sha256"`
		Installed bool   `json:"installed"`
	}
	out := make([]entry, 0, len(modelstore.Catalog()))
	for _, m := range modelstore.Catalog() {
		out = append(out, entry{
			ID:        m.ID,
			File:      m.File,
			Bytes:     m.Bytes,
			SHA256:    m.SHA256,
			Installed: h.store.Installed(m),
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

// download serves one model file. http.ServeFile provides Range/HEAD/
// If-Range — the client installer relies on Range for pause/resume.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.RequireUser(w, r); !ok {
		return
	}
	path, err := h.store.FilePath(r.PathValue("id"), r.PathValue("file"))
	if err != nil {
		httpapi.WriteDetail(w, http.StatusNotFound, "unknown model")
		return
	}
	if _, err := os.Stat(path); err != nil {
		// The route exists but the operator has not pre-downloaded this
		// model — an actionable 404, never a wrong file.
		h.logger.Warn("model not installed on server", "id", r.PathValue("id"))
		httpapi.WriteDetail(w, http.StatusNotFound, "model not downloaded on this server — the operator must run: livetranslate-server download-models")
		return
	}
	http.ServeFile(w, r, path)
}
