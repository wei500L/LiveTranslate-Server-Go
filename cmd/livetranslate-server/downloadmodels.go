package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"livetranslate/server/internal/modelstore"
)

// runDownloadModels pre-fetches the client's on-device AI models (offline
// translation GGUFs + the image-understanding model) from their pinned
// Hugging Face revisions into MODEL_STORAGE_DIR, verifying each file's
// SHA256 before it lands. Idempotent: already-verified files are skipped,
// interrupted downloads resume from their .partial sibling.
//
// This is the server-operator step that makes the iOS app's 云端服务器
// download source work; clients always re-verify bytes against their own
// bundled manifest, so the server is only trusted for availability.
//
// Reads MODEL_STORAGE_DIR directly (no database or full config needed —
// the same path rules as serve's config validation apply):
//
//	MODEL_STORAGE_DIR=/var/lib/livetranslate/models livetranslate-server download-models
func runDownloadModels() error {
	root := os.Getenv("MODEL_STORAGE_DIR")
	if root == "" {
		return fmt.Errorf("MODEL_STORAGE_DIR is not set — point it at a dedicated directory (it must match the serve deployment's MODEL_STORAGE_DIR)")
	}
	// Same discipline as config.ValidateProduction enforces for serve: a
	// relative root would resolve against THIS process's cwd while the
	// serve deployment (different cwd under systemd/containers) reads a
	// different directory — clients then see 404s with no local clue why.
	if !filepath.IsAbs(root) {
		return fmt.Errorf("MODEL_STORAGE_DIR must be an absolute path (got %q — relative paths break when serve runs under a different working directory)", root)
	}
	if root == "/tmp" || root == "/var/tmp" {
		return fmt.Errorf("MODEL_STORAGE_DIR must not point at the system temp directory")
	}
	store, err := modelstore.NewStore(root)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Progress lines go to stderr so stdout stays machine-parseable and
	// `download-models > log` still shows progress on the terminal.
	progressLine := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var startedAt time.Time
	dl := modelstore.Downloader{
		Log: func(format string, args ...any) {
			logger.Info(fmt.Sprintf(format, args...))
		},
		OnModelStart: func(m modelstore.Model) {
			startedAt = time.Now()
		},
		Progress: func(received, total int64) {
			pct := 0.0
			if total > 0 {
				pct = float64(received) / float64(total) * 100
			}
			elapsed := time.Since(startedAt).Seconds()
			rate := 0.0
			if elapsed > 0.5 {
				rate = float64(received) / (1 << 20) / elapsed
			}
			progressLine.Info("progress",
				"mib", fmt.Sprintf("%.0f/%.0f", float64(received)/(1<<20), float64(total)/(1<<20)),
				"pct", fmt.Sprintf("%.1f%%", pct),
				"rate", fmt.Sprintf("%.1f MiB/s", rate),
			)
		},
		OnModelDone: func(m modelstore.Model) {
			progressLine.Info("done",
				"model", m.ID,
				"elapsed", time.Since(startedAt).Round(time.Second).String(),
			)
		},
	}
	if err := dl.DownloadAll(store); err != nil {
		return err
	}
	logger.Info("all models present and verified", "dir", store.Root())
	return nil
}
