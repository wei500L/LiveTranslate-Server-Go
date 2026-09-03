package sync

import (
	"context"
	"log/slog"
	"time"
)

// RunTombstoneCleanup hard-deletes tombstoned rows and change-log entries
// older than the retention window. Rows deleted more recently stay so
// offline devices still receive the delete event through the change log; a
// device offline longer than the window must do a fresh initial upload (the
// iOS client handles this via the forced fresh-start path).
func (s *Service) RunTombstoneCleanup(ctx context.Context) error {
	retention := time.Duration(s.cfg.TombstoneRetentionDays) * 24 * time.Hour
	if retention <= 0 {
		retention = 180 * 24 * time.Hour
	}
	cutoff := time.Now().Add(-retention)

	// Tombstoned entity rows (children first to keep the deletes cheap even
	// without FK cascades in the path).
	for _, table := range []string{
		"transcript_entries", "bookmarks", "favorite_sessions", "classroom_sessions",
	} {
		tag, err := s.db.Q().Exec(ctx, `
			DELETE FROM `+table+`
			WHERE deleted_at IS NOT NULL AND deleted_at < $1`, cutoff)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			slog.Info("tombstone gc", "table", table, "rows", tag.RowsAffected())
		}
	}

	// Change-log entries and the idempotency ledger for the same window. A
	// replayed operationId older than the cutoff simply re-applies
	// (operations are idempotent by content), which is acceptable after
	// 180 days.
	if _, err := s.db.Q().Exec(ctx, `
		DELETE FROM sync_changes WHERE created_at < $1`, cutoff); err != nil {
		return err
	}
	if _, err := s.db.Q().Exec(ctx, `
		DELETE FROM processed_operations WHERE created_at < $1`, cutoff); err != nil {
		return err
	}
	return nil
}

// StartCleanupLoop runs RunTombstoneCleanup once per interval until the
// context is cancelled. Errors are logged, never fatal.
func (s *Service) StartCleanupLoop(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				if err := s.RunTombstoneCleanup(cctx); err != nil && ctx.Err() == nil {
					slog.Error("tombstone cleanup failed", "err", err)
				}
				cancel()
			}
		}
	}()
}
