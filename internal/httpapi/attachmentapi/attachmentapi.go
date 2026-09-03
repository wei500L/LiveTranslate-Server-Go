// Package attachmentapi hosts the /v1/attachments routes: binary
// upload/download for classroom images. Metadata syncs through the
// regular /v1/sync protocol; these routes only move bytes and verify
// them against the synced row's hash/size contract.
//
// Routes:
//
//	PUT  /v1/attachments/{attachmentId}/{variant}
//	GET  /v1/attachments/{attachmentId}/{variant}
//	GET  /v1/attachments/{attachmentId}/{variant}/status
//	DELETE /v1/attachments/{attachmentId}
//
// Every route requires an active user; the attachment row must exist,
// be live and belong to that user before any byte moves.
package attachmentapi

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
	db          *store.DB
	auth        *authapi.Handler
	attachments *storage.Store
	maxUpload   int64
}

func NewHandler(db *store.DB, auth *authapi.Handler, attachments *storage.Store) *Handler {
	return &Handler{db: db, auth: auth, attachments: attachments, maxUpload: 40 << 20}
}

// SetMaxUploadBytes overrides the single-upload byte cap (from config).
func (h *Handler) SetMaxUploadBytes(n int64) {
	if n > 0 {
		h.maxUpload = n
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/attachments/{attachmentId}/{variant}", h.upload)
	mux.HandleFunc("GET /v1/attachments/{attachmentId}/{variant}", h.download)
	mux.HandleFunc("HEAD /v1/attachments/{attachmentId}/{variant}", h.download)
	mux.HandleFunc("GET /v1/attachments/{attachmentId}/{variant}/status", h.uploadStatus)
	mux.HandleFunc("DELETE /v1/attachments/{attachmentId}", h.deleteFiles)
}

// parseIDs authenticates and extracts the attachment id from the path.
func (h *Handler) parseIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	ac, ok := h.auth.RequireUser(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("attachmentId"))
	if err != nil {
		httpapi.WriteDetail(w, http.StatusBadRequest, "invalid attachment id")
		return uuid.Nil, uuid.Nil, false
	}
	return ac.User.ID, id, true
}

func (h *Handler) variantFromPath(w http.ResponseWriter, r *http.Request) (storage.Variant, bool) {
	switch r.PathValue("variant") {
	case "original":
		return storage.VariantOriginal, true
	case "preview":
		return storage.VariantPreview, true
	default:
		httpapi.WriteDetail(w, http.StatusBadRequest, "invalid variant (original|preview)")
		return "", false
	}
}

// fetchLive loads the attachment row for ownership/liveness checks.
func (h *Handler) fetchLive(ctx context.Context, userID, id uuid.UUID) (*sync.AttachmentMeta, error) {
	return sync.GetAttachmentMeta(ctx, h.db.Q(), userID, id)
}

// upload stores one variant. Idempotent: re-uploading identical bytes
// is a no-op; the row's content_hash is the contract (mismatch → 409).
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	variant, ok := h.variantFromPath(w, r)
	if !ok {
		return
	}
	meta, err := h.fetchLive(r.Context(), userID, id)
	if err != nil {
		slog.Error("attachment meta load failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if meta == nil || meta.Deleted {
		httpapi.WriteDetail(w, http.StatusNotFound, "attachment not found")
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
			"detail":    "X-Content-Hash does not match the attachment metadata",
		})
		return
	}

	body := http.MaxBytesReader(w, r.Body, h.maxUpload)
	size, err := h.attachments.Write(r.Context(), userID, id, declaredHash, variant, body)
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
		slog.Error("attachment write failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"attachmentId": id.String(),
		"variant":      string(variant),
		"storedBytes":  size,
	})
}

// download streams a stored variant. Images are immutable per hash, so
// responses are cacheable; ownership is re-checked every request.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	variant, ok := h.variantFromPath(w, r)
	if !ok {
		return
	}
	meta, err := h.fetchLive(r.Context(), userID, id)
	if err != nil {
		slog.Error("attachment meta load failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if meta == nil || meta.Deleted {
		httpapi.WriteDetail(w, http.StatusNotFound, "attachment not found")
		return
	}
	f, size, err := h.attachments.Open(userID, id, variant)
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
	w.Header().Set("ETag", fmt.Sprintf(`"%s_%s"`, id, variant))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// uploadStatus reports whether a variant is stored (cheap existence
// probe used by clients deciding what to download).
func (h *Handler) uploadStatus(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	variant, ok := h.variantFromPath(w, r)
	if !ok {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"attachmentId": id.String(),
		"variant":      string(variant),
		"uploaded":     h.attachments.Has(userID, id, variant),
	})
}

// deleteFiles removes the binary files of a live attachment row (the
// metadata tombstone travels through /v1/sync/push; file reaping for
// sync-driven deletes happens in the sync service post-commit).
func (h *Handler) deleteFiles(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.parseIDs(w, r)
	if !ok {
		return
	}
	meta, err := h.fetchLive(r.Context(), userID, id)
	if err != nil {
		slog.Error("attachment meta load failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if meta == nil || meta.Deleted {
		httpapi.WriteDetail(w, http.StatusNotFound, "attachment not found")
		return
	}
	if err := h.attachments.DeleteFiles(userID, id); err != nil {
		slog.Error("attachment file delete failed", "request_id", httpapi.RequestID(r.Context()), "err", err.Error())
		httpapi.WriteDetail(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
