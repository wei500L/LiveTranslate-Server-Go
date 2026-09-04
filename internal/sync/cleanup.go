package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"livetranslate/server/internal/metrics"
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

	deleted := int64(0)
	// Tombstoned entity rows (children first to keep the deletes cheap even
	// without FK cascades in the path). session_attachments and
	// course_materials go first of all so their files can be reaped while
	// the rows still exist.
	attachmentIDs, err := s.gcTombstonedAttachments(ctx, cutoff)
	if err != nil {
		return err
	}
	deleted += int64(len(attachmentIDs))
	materialIDs, err := s.gcTombstonedMaterials(ctx, cutoff)
	if err != nil {
		return err
	}
	deleted += int64(len(materialIDs))
	for _, table := range []string{
		"transcript_entries", "bookmarks", "favorite_sessions", "session_notes",
		"study_reviews", "classroom_sessions", "courses",
		"glossary_terms", "study_cards", "study_tasks",
		"transcript_corrections",
		"schedule_exceptions", "course_schedules",
		"material_pages", "material_annotations",
		"assistant_messages", "assistant_threads",
		"study_plan_items", "study_plans", "exam_topics", "exams",
		"study_activities",
	} {
		tag, err := s.db.Q().Exec(ctx, `
			DELETE FROM `+table+`
			WHERE deleted_at IS NOT NULL AND deleted_at < $1`, cutoff)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			slog.Info("tombstone gc", "table", table, "rows", tag.RowsAffected())
			deleted += tag.RowsAffected()
		}
	}

	// Change-log entries and the idempotency ledger for the same window. A
	// replayed operationId older than the cutoff simply re-applies
	// (operations are idempotent by content), which is acceptable after
	// 180 days.
	for _, table := range []string{"sync_changes", "processed_operations"} {
		tag, err := s.db.Q().Exec(ctx, `
			DELETE FROM `+table+` WHERE created_at < $1`, cutoff)
		if err != nil {
			return err
		}
		deleted += tag.RowsAffected()
	}

	metrics.Inc(metrics.TombstoneGcRuns)
	metrics.Add(metrics.MaintenanceDeleted, deleted)
	return nil
}

// gcTombstonedAttachments deletes attachment rows past retention and
// reaps their files. Rows are collected (user, id) first so the files
// can be removed after the row delete without a second query.
func (s *Service) gcTombstonedAttachments(ctx context.Context, cutoff time.Time) ([][2]string, error) {
	rows, err := s.db.Q().Query(ctx, `
		SELECT user_id::text, id::text FROM session_attachments
		WHERE deleted_at IS NOT NULL AND deleted_at < $1`, cutoff)
	if err != nil {
		return nil, err
	}
	var ids [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, pair)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := s.db.Q().Exec(ctx, `
		DELETE FROM session_attachments
		WHERE deleted_at IS NOT NULL AND deleted_at < $1`, cutoff); err != nil {
		return nil, err
	}
	slog.Info("tombstone gc", "table", "session_attachments", "rows", len(ids))
	if s.attachments != nil {
		for _, pair := range ids {
			uid, err := uuid.Parse(pair[0])
			if err != nil {
				continue
			}
			aid, err := uuid.Parse(pair[1])
			if err != nil {
				continue
			}
			if err := s.attachments.DeleteFiles(uid, aid); err != nil {
				// Best-effort: a stray file is disk noise, not data loss.
				slog.Error("attachment file gc failed", "user_id", uid, "attachment_id", aid, "err", err.Error())
			}
		}
	}
	return ids, nil
}

// gcTombstonedMaterials deletes material rows past retention and reaps
// their files (the attachment GC pattern — rows collected first so files
// can be removed after the row delete). Materials that borrow a
// classroom attachment's files (source_attachment_id set) have no file of
// their own; the storage layer's DeleteFiles on a never-written directory
// is a cheap no-op, so no filter is needed.
func (s *Service) gcTombstonedMaterials(ctx context.Context, cutoff time.Time) ([][2]string, error) {
	rows, err := s.db.Q().Query(ctx, `
		SELECT user_id::text, id::text FROM course_materials
		WHERE deleted_at IS NOT NULL AND deleted_at < $1`, cutoff)
	if err != nil {
		return nil, err
	}
	var ids [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, pair)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := s.db.Q().Exec(ctx, `
		DELETE FROM course_materials
		WHERE deleted_at IS NOT NULL AND deleted_at < $1`, cutoff); err != nil {
		return nil, err
	}
	slog.Info("tombstone gc", "table", "course_materials", "rows", len(ids))
	if s.attachments != nil {
		for _, pair := range ids {
			uid, err := uuid.Parse(pair[0])
			if err != nil {
				continue
			}
			mid, err := uuid.Parse(pair[1])
			if err != nil {
				continue
			}
			if err := s.attachments.DeleteFiles(uid, mid); err != nil {
				// Best-effort: a stray file is disk noise, not data loss.
				slog.Error("material file gc failed", "user_id", uid, "material_id", mid, "err", err.Error())
			}
		}
	}
	return ids, nil
}

// RunMaintenanceCleanup prunes the auth-side ephemeral state. Kept separate
// from the tombstone GC so each can run on its own cadence; both are driven
// by StartCleanupLoop.
//
// Windows (all env-configurable where it matters):
//   - email_challenges: consumed or expired for >24h;
//   - password_reset_tokens: consumed or expired for >24h;
//   - refresh_tokens: expired (or revoked) for >7 days;
//   - admin_sessions: expired/revoked for >7 days;
//   - login_events: older than LOGIN_EVENTS_RETENTION_DAYS (0 = keep);
//   - audit_events: older than AUDIT_RETENTION_DAYS (0 = keep forever —
//     the audit trail is the accountability record, so the default is a
//     full year and an explicit 0 opts out of pruning).
func (s *Service) RunMaintenanceCleanup(ctx context.Context) error {
	q := s.db.Q()

	type job struct {
		name string
		sql  string
		args []any
	}
	jobs := []job{
		{"email_challenges", `
			DELETE FROM email_challenges
			WHERE (consumed_at IS NOT NULL AND consumed_at < now() - interval '24 hours')
			   OR expires_at < now() - interval '24 hours'`, nil},
		{"password_reset_tokens", `
			DELETE FROM password_reset_tokens
			WHERE (consumed_at IS NOT NULL AND consumed_at < now() - interval '24 hours')
			   OR expires_at < now() - interval '24 hours'`, nil},
		{"refresh_tokens", `
			DELETE FROM refresh_tokens
			WHERE (expires_at < now() - interval '7 days')
			   OR (revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')`, nil},
		{"admin_sessions", `
			DELETE FROM admin_sessions
			WHERE (expires_at < now() - interval '7 days')
			   OR (revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')`, nil},
	}
	if s.cfg.LoginEventsRetentionDays > 0 {
		jobs = append(jobs, job{"login_events", `
			DELETE FROM login_events WHERE created_at < now() - make_interval(days => $1)`,
			[]any{s.cfg.LoginEventsRetentionDays}})
	}
	if s.cfg.AuditRetentionDays > 0 {
		jobs = append(jobs, job{"audit_events", `
			DELETE FROM audit_events WHERE created_at < now() - make_interval(days => $1)`,
			[]any{s.cfg.AuditRetentionDays}})
	}

	for _, j := range jobs {
		// A nil args slice spreads to zero placeholders — one call shape
		// covers both parameterized and plain statements.
		tag, err := q.Exec(ctx, j.sql, j.args...)
		if err != nil {
			return fmt.Errorf("maintenance %s: %w", j.name, err)
		}
		if tag.RowsAffected() > 0 {
			slog.Info("maintenance cleanup", "table", j.name, "rows", tag.RowsAffected())
			if j.name == "login_events" {
				metrics.Add(metrics.LoginEventsPruned, tag.RowsAffected())
			} else {
				metrics.Add(metrics.MaintenanceDeleted, tag.RowsAffected())
			}
		}
	}
	metrics.Inc(metrics.TombstoneGcRuns)
	return nil
}

// StartCleanupLoop runs RunTombstoneCleanup once per interval until the
// context is cancelled; the auth-side maintenance cleanup piggybacks on the
// same cadence (its statements are cheap and indexed). Errors are logged,
// never fatal.
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
				if err := s.RunMaintenanceCleanup(cctx); err != nil && ctx.Err() == nil {
					slog.Error("maintenance cleanup failed", "err", err)
				}
				cancel()
			}
		}
	}()
}
