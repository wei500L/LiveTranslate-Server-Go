// Package materialapi hosts the /v1/materials routes: binary
// upload/download for course-material ORIGINAL FILES (PDF 附件、讲义原
// 文件). Metadata syncs through the regular /v1/sync protocol; these
// routes only move bytes and verify them against the synced row's
// hash/size contract — the exact /v1/attachments contract, sharing the
// SAME storage backend (paths are keyed by entity UUID, so a material's
// directory can never collide with an attachment's).
//
// Routes:
//
//	PUT  /v1/materials/{materialId}/file
//	GET  /v1/materials/{materialId}/file
//	HEAD /v1/materials/{materialId}/file
//	GET  /v1/materials/{materialId}/file/status
//	DELETE /v1/materials/{materialId}
//
// Every route requires an active user; the material row must exist, be
// live and belong to that user before any byte moves. Materials that
// borrow a classroom attachment's files (source_attachment_id) have no
// file of their own — uploads/downloads on them return 404 (honest
// absence, never a wrong file).
package materialapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"livetranslate/server/internal/httpapi"
	authapi "livetranslate/server/internal/httpapi/auth"
	"livetranslate/server/internal/storage"
	"livetranslate/server/internal/store"
	"livetranslate/server/internal/sync"
)

type Handler struct {
	db        *store.DB
	auth      *authapi.Handler
	files     *storage.Store
	maxUpload int64
}

func NewHandler(db *store.DB, auth *authapi.Handler, files *storage.Store) *Handler {
	return &Handler{db: db, auth: auth, files: files, maxUpload: 200 << 20}
}

// SetMaxUploadBytes overrides the single-upload byte cap (from config).
// PDFs run larger than classroom images, so the default cap is higher.
func (h *Handler) SetMaxUploadBytes(n int64) {
	if n > 0 {
		h.maxUpload = n
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/materials/{materialId}/file", h.upload)
	mux.HandleFunc("GET /v1/materials/{materialId}/file", h.download)
	mux.HandleFunc("HEAD /v1/materials/{materialId}/file", h.download)
	mux.HandleFunc("GET /v1/materials/{materialId}/file/status", h.uploadStatus)
	mux.HandleFunc("DELETE /v1/materials/{materialId}", h.deleteFiles)
}

// parseIDs authenticates and extracts the material id from the path.
func (h *Handler) parseIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("materialId"))
	if err != nil {
		httpapi.WriteDetail(w, http.StatusBadRequest, "invalid material id")
		return uuid.Nil, uuid.Nil, false
	}
	return ac.User.ID, id, true
}

// fetchLive loads the material row for ownership/liveness checks.
func (h *Handler) fetchLive(ctx context.Context, userID, id uuid.UUID) (*sync.MaterialMeta, error) {
	return sync.GetMaterialMeta(ctx, h.db.Q(), userID, id)
}

// upload stores the original file. Idempotent: re-uploading identical
// bytes is a no-op; the row's content_hash is the contract (mismatch →
// 409). The body streams straight into the storage layer (hash computed
// while copying — a 200 MB PDF never buffers whole in memory).
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	meta, err := h.fetchLive(r.Context(), userID, id)
	if err != nil {
		slog.Error("material meta load failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if meta == nil || meta.Deleted {
		httpapi.WriteDetail(w, http.StatusNotFound, "material not found")
		return
	}
	if meta.BorrowsAttachment {
		// A material created from a classroom image references the
		// attachment's files; it has no file of its own.
		httpapi.WriteDetail(w, http.StatusNotFound, "material borrows an attachment's files")
		return
	}

	declaredHash := r.Header.Get("X-Content-Hash")
	if declaredHash == "" {
		httpapi.WriteDetail(w, http.StatusBadRequest, "X-Content-Hash header required")
		return
	}
	// The row's hash wins when present (the metadata is the contract the
	// client pushed); a header disagreement is a client bug worth surfacing.
	if meta.ContentHash != "" && declaredHash != meta.ContentHash {
		httpapi.WriteDetail(w, http.StatusConflict, map[string]string{
			"errorCode": "hash_mismatch",
			"detail":    "X-Content-Hash does not match the material metadata",
		})
		return
	}

	body := http.MaxBytesReader(w, r.Body, h.maxUpload)
	size, err := h.files.Write(r.Context(), userID, id, declaredHash, storage.VariantOriginal, body)
	if err != nil {
		if errors.Is(err, storage.ErrHashMismatch) {
			httpapi.WriteDetail(w, http.StatusConflict, map[string]string{
				"errorCode": "hash_mismatch",
				"detail":    "uploaded bytes do not match the declared hash",
			})
			return
		}
		if errors.Is(err, storage.ErrInvalidName) {
			httpapi.WriteDetail(w, http.StatusBadRequest, "invalid content hash")
			return
		}
		slog.Error("material write failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"materialId":  id.String(),
		"storedBytes": size,
	})
}

// download streams the stored original. Files are immutable per hash, so
// responses are cacheable; ownership is re-checked every request.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	meta, err := h.fetchLive(r.Context(), userID, id)
	if err != nil {
		slog.Error("material meta load failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if meta == nil || meta.Deleted {
		httpapi.WriteDetail(w, http.StatusNotFound, "material not found")
		return
	}
	if meta.BorrowsAttachment {
		httpapi.WriteDetail(w, http.StatusNotFound, "material borrows an attachment's files")
		return
	}
	f, size, err := h.files.Open(userID, id, storage.VariantOriginal)
	if err != nil {
		httpapi.WriteDetail(w, http.StatusNotFound, "file not uploaded yet")
		return
	}
	defer f.Close()
	mime := "application/octet-stream"
	if meta.MimeType != "" {
		mime = meta.MimeType
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", fmt.Sprintf(`"%s_file"`, id))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// uploadStatus reports whether the original file is stored (cheap
// existence probe used by clients deciding what to download).
func (h *Handler) uploadStatus(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	meta, err := h.fetchLive(r.Context(), userID, id)
	if err != nil {
		slog.Error("material meta load failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if meta == nil || meta.Deleted {
		httpapi.WriteDetail(w, http.StatusNotFound, "material not found")
		return
	}
	uploaded := !meta.BorrowsAttachment && h.files.Has(userID, id, storage.VariantOriginal)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"materialId": id.String(),
		"uploaded":   uploaded,
	})
}

// deleteFiles removes the binary file of a live material row (the
// metadata tombstone travels through /v1/sync/push; file reaping for
// sync-driven deletes happens in the sync service post-commit).
func (h *Handler) deleteFiles(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	meta, err := h.fetchLive(r.Context(), userID, id)
	if err != nil {
		slog.Error("material meta load failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if meta == nil || meta.Deleted {
		httpapi.WriteDetail(w, http.StatusNotFound, "material not found")
		return
	}
	if err := h.files.DeleteFiles(userID, id); err != nil {
		slog.Error("material file delete failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
