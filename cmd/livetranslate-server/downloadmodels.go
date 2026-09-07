package main

import (
	"fmt"
	"log/slog"
	"os"

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
// Reads MODEL_STORAGE_DIR directly (no database or full config needed):
//
//	MODEL_STORAGE_DIR=/var/lib/livetranslate/models livetranslate-server download-models
func runDownloadModels() error {
	root := os.Getenv("MODEL_STORAGE_DIR")
	if root == "" {
		return fmt.Errorf("MODEL_STORAGE_DIR is not set — point it at a dedicated directory (it must match the serve deployment's MODEL_STORAGE_DIR)")
	}
	store, err := modelstore.NewStore(root)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	dl := modelstore.Downloader{
		Log: func(format string, args ...any) {
			logger.Info(fmt.Sprintf(format, args...))
		},
	}
	if err := dl.DownloadAll(store); err != nil {
		return err
	}
	logger.Info("all models present and verified", "dir", store.Root())
	return nil
}
