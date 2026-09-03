// Package httpapi hosts the HTTP layer: middleware (request ID, logging,
// panic recovery, body limits, CORS, security headers), the /v1 routers
// (auth, sync, account) and the JSON error conventions shared with the
// Python service ({"detail": …}).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"log/slog"

	"github.com/google/uuid"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID returns the request's ID (set by the middleware).
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// withRequestID stamps a fresh ID onto the request context.
func withRequestID(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestIDKey, uuid.NewString()))
}

// Handler wraps the inner handler with the production hardening chain:
// request ID → logging (no bodies, no auth headers) → panic recovery →
// timeouts → body cap → security headers.
func Handler(cfgRouter Router, maxBodyBytes int64, shutdownTimeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withRequestID(r)
		w.Header().Set("X-Request-Id", RequestID(r.Context()))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")

		ctx, cancel := context.WithTimeout(r.Context(), shutdownTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				slog.Error("panic recovered",
					"request_id", RequestID(r.Context()),
					"method", r.Method, "path", r.URL.Path, "panic", p)
				if !sw.wroteHeader {
					WriteDetail(sw, http.StatusInternalServerError, "internal server error")
				}
			}
			slog.Info("request",
				"request_id", RequestID(r.Context()),
				"method", r.Method, "path", r.URL.Path,
				"status", sw.status, "duration_ms", time.Since(start).Milliseconds())
		}()

		cfgRouter.ServeHTTP(sw, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

// writeDetail emits the shared error body {"detail": …}.
func WriteDetail(w http.ResponseWriter, status int, detail any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"detail": detail})
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decodeJSON decodes into dst with a strict-but-tolerant posture:
// unknown fields ignored (forward compatibility), malformed JSON → 400.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			WriteDetail(w, http.StatusRequestEntityTooLarge, "request body too large")
			return err
		}
		WriteDetail(w, http.StatusBadRequest, "invalid JSON body")
		return err
	}
	return nil
}

// Router is implemented by the mux assembly (api router / admin router).
type Router interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// CORS applies the whitelist. Empty list → no CORS headers (same-origin
// iOS app traffic needs none).
func CORS(allowed []string, next http.Handler) http.Handler {
	allowedSet := map[string]bool{}
	for _, o := range allowed {
		allowedSet[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowedSet[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
