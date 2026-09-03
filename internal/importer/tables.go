package importer

import (
	"context"
	"encoding/json"
	"fmt"

	"livetranslate/server/internal/sqlitereader"
	"livetranslate/server/internal/store"
)

// copyTable dispatches one table's rows to its typed copier. Every insert
// uses explicit UUIDs (preserving identities and relations). ON CONFLICT DO
// NOTHING makes a re-run after a failed import idempotent; in a
// --allow-non-empty merge, rows whose id already exists in the target are
// SKIPPED (the target's row wins) — the report counts only what the source
// holds, so re-check the target after such a merge.
func copyTable(ctx context.Context, q store.Q, table string, rows []sqlitereader.Row, report *Report) error {
	switch table {
	case "users":
		return copyUsers(ctx, q, rows)
	case "devices":
		return copyDevices(ctx, q, rows)
	case "refresh_tokens":
		return copyRefreshTokens(ctx, q, rows)
	case "classroom_sessions":
		return copySessions(ctx, q, rows)
	case "transcript_entries":
		return copyEntries(ctx, q, rows)
	case "bookmarks":
		return copyBookmarks(ctx, q, rows)
	case "favorite_sessions":
		return copyFavorites(ctx, q, rows)
	case "sync_changes":
		return copySyncChanges(ctx, q, rows)
	case "processed_operations":
		return copyProcessedOps(ctx, q, rows)
	default:
		return fmt.Errorf("unexpected table %s", table)
	}
}

func copyUsers(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := rowUUID(r, "id")
		if err != nil {
			return err
		}
		createdAt, err := rowTime(r, "created_at")
		if err != nil {
			return err
		}
		updatedAt, err := rowTime(r, "updated_at")
		if err != nil {
			return err
		}
		deletedAt, err := rowTimePtr(r, "deleted_at")
		if err != nil {
			return err
		}
		// Legacy columns only: the Python schema had NO email — imported
		// users keep their Apple/dev identity and can set a password later
		// through the reset flow. No default password is ever generated.
		appleSubject := nullableString(rowString(r, "apple_subject"))
		devName := nullableString(rowString(r, "dev_name"))
		status := "active"
		if deletedAt != nil {
			status = "deleted"
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO users (id, email, normalized_email, display_name, status, role,
				apple_subject, dev_name, email_verified_at, created_at, updated_at,
				last_login_at, deletion_requested_at, deleted_at)
			VALUES ($1, NULL, NULL, '', $2, 'user', $3, $4, NULL, $5, $6, NULL, $7, $8)
			ON CONFLICT (id) DO NOTHING`,
			id, status, appleSubject, devName, createdAt, updatedAt, deletedAt, deletedAt,
		); err != nil {
			return fmt.Errorf("user %s: %w", id, err)
		}
	}
	return nil
}

func copyDevices(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := rowUUID(r, "id")
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		lastSeen, err := rowTime(r, "last_seen_at")
		if err != nil {
			return err
		}
		revokedAt, err := rowTimePtr(r, "revoked_at")
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO devices (id, user_id, client_device_id, display_name, app_version, last_seen_at, revoked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO NOTHING`,
			id, userID, rowString(r, "client_device_id"), rowString(r, "display_name"),
			rowString(r, "app_version"), lastSeen, revokedAt,
		); err != nil {
			return fmt.Errorf("device %s: %w", id, err)
		}
	}
	return nil
}

func copyRefreshTokens(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := rowUUID(r, "id")
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		deviceID, err := rowUUID(r, "device_id")
		if err != nil {
			return err
		}
		expiresAt, err := rowTime(r, "expires_at")
		if err != nil {
			return err
		}
		createdAt, err := rowTime(r, "created_at")
		if err != nil {
			return err
		}
		revokedAt, err := rowTimePtr(r, "revoked_at")
		if err != nil {
			return err
		}
		// token_hash is a SHA-256 digest, not a credential; keeping it lets
		// already-signed-in devices continue their refresh chains.
		var replacedBy any
		if v, ok := r.Col("replaced_by"); ok && !v.IsNull() {
			rid, err := normalizeUUID(v.AsString())
			if err != nil {
				return fmt.Errorf("token %s: %w", id, err)
			}
			replacedBy = rid
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO refresh_tokens (id, user_id, device_id, token_hash, expires_at, revoked_at, replaced_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (id) DO NOTHING`,
			id, userID, deviceID, rowString(r, "token_hash"), expiresAt, revokedAt, replacedBy, createdAt,
		); err != nil {
			return fmt.Errorf("refresh token %s: %w", id, err)
		}
	}
	return nil
}

func copySessions(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := rowUUID(r, "id")
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		startedAt, err := rowTime(r, "started_at")
		if err != nil {
			return err
		}
		createdAt, err := rowTime(r, "created_at")
		if err != nil {
			return err
		}
		updatedAt, err := rowTime(r, "updated_at")
		if err != nil {
			return err
		}
		endedAt, err := rowTimePtr(r, "ended_at")
		if err != nil {
			return err
		}
		deletedAt, err := rowTimePtr(r, "deleted_at")
		if err != nil {
			return err
		}
		duration, err := rowFloat(r, "duration", 0)
		if err != nil {
			return err
		}
		abnormal := false
		if v, ok := r.Col("abnormal_termination"); ok && !v.IsNull() {
			if n, err := v.AsInt(); err == nil {
				abnormal = n != 0
			}
		}
		serverVersion, err := rowInt(r, "server_version", 0)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO classroom_sessions (id, user_id, title, started_at, ended_at, duration,
				source_language, target_language, session_status, abnormal_termination,
				server_version, created_at, updated_at, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (id) DO NOTHING`,
			id, userID, rowString(r, "title"), startedAt, endedAt, duration,
			orDefault(rowString(r, "source_language"), "ru"),
			orDefault(rowString(r, "target_language"), "zh-CN"),
			orDefault(rowString(r, "session_status"), "active"),
			abnormal, serverVersion, createdAt, updatedAt, deletedAt,
		); err != nil {
			return fmt.Errorf("session %s: %w", id, err)
		}
	}
	return nil
}

func copyEntries(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := rowUUID(r, "id")
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		sessionID, err := rowUUID(r, "session_id")
		if err != nil {
			return err
		}
		sequenceID, err := rowInt(r, "sequence_id", 0)
		if err != nil {
			return err
		}
		startOffset, err := rowFloat(r, "start_offset", 0)
		if err != nil {
			return err
		}
		endOffset, err := rowFloat(r, "end_offset", 0)
		if err != nil {
			return err
		}
		createdAt, err := rowTime(r, "created_at")
		if err != nil {
			return err
		}
		updatedAt, err := rowTime(r, "updated_at")
		if err != nil {
			return err
		}
		deletedAt, err := rowTimePtr(r, "deleted_at")
		if err != nil {
			return err
		}
		serverVersion, err := rowInt(r, "server_version", 0)
		if err != nil {
			return err
		}
		// chinese_text is nullable; NULL stays NULL (not '').
		var chinese any
		if v, ok := r.Col("chinese_text"); ok && !v.IsNull() {
			chinese = v.AsString()
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO transcript_entries (id, user_id, session_id, sequence_id,
				start_offset, end_offset, russian_text, chinese_text, translation_status,
				server_version, created_at, updated_at, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (id) DO NOTHING`,
			id, userID, sessionID, sequenceID, startOffset, endOffset,
			rowString(r, "russian_text"), chinese,
			orDefault(rowString(r, "translation_status"), "pending"),
			serverVersion, createdAt, updatedAt, deletedAt,
		); err != nil {
			return fmt.Errorf("entry %s: %w", id, err)
		}
	}
	return nil
}

func copyBookmarks(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := rowUUID(r, "id")
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		sessionID, err := rowUUID(r, "session_id")
		if err != nil {
			return err
		}
		entryID, err := rowUUID(r, "entry_id")
		if err != nil {
			return err
		}
		updatedAt, err := rowTime(r, "updated_at")
		if err != nil {
			return err
		}
		deletedAt, err := rowTimePtr(r, "deleted_at")
		if err != nil {
			return err
		}
		serverVersion, err := rowInt(r, "server_version", 0)
		if err != nil {
			return err
		}
		isBookmarked := true
		if v, ok := r.Col("is_bookmarked"); ok && !v.IsNull() {
			if n, err := v.AsInt(); err == nil {
				isBookmarked = n != 0
			}
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO bookmarks (id, user_id, session_id, entry_id, is_bookmarked, server_version, updated_at, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (id) DO NOTHING`,
			id, userID, sessionID, entryID, isBookmarked, serverVersion, updatedAt, deletedAt,
		); err != nil {
			return fmt.Errorf("bookmark %s: %w", id, err)
		}
	}
	return nil
}

func copyFavorites(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := rowUUID(r, "id")
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		sessionID, err := rowUUID(r, "session_id")
		if err != nil {
			return err
		}
		updatedAt, err := rowTime(r, "updated_at")
		if err != nil {
			return err
		}
		deletedAt, err := rowTimePtr(r, "deleted_at")
		if err != nil {
			return err
		}
		serverVersion, err := rowInt(r, "server_version", 0)
		if err != nil {
			return err
		}
		isFavorite := true
		if v, ok := r.Col("is_favorite"); ok && !v.IsNull() {
			if n, err := v.AsInt(); err == nil {
				isFavorite = n != 0
			}
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO favorite_sessions (id, user_id, session_id, is_favorite, server_version, updated_at, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO NOTHING`,
			id, userID, sessionID, isFavorite, serverVersion, updatedAt, deletedAt,
		); err != nil {
			return fmt.Errorf("favorite %s: %w", id, err)
		}
	}
	return nil
}

// copySyncChanges preserves change_sequence EXPLICITLY — it is the sync
// cursor domain; after import, new changes continue after the imported max.
func copySyncChanges(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		seq, err := r.Col("change_sequence").AsInt()
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		entityID, err := rowUUID(r, "entity_id")
		if err != nil {
			return err
		}
		createdAt, err := rowTime(r, "created_at")
		if err != nil {
			return err
		}
		serverVersion, err := rowInt(r, "server_version", 0)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO sync_changes (change_sequence, user_id, entity_type, entity_id, operation, server_version, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (change_sequence) DO NOTHING`,
			seq, userID, rowString(r, "entity_type"), entityID,
			rowString(r, "operation"), serverVersion, createdAt,
		); err != nil {
			return fmt.Errorf("sync change %d: %w", seq, err)
		}
	}
	return nil
}

// copyProcessedOps preserves the idempotency ledger (id + result JSON).
func copyProcessedOps(ctx context.Context, q store.Q, rows []sqlitereader.Row) error {
	for _, r := range rows {
		id, err := r.Col("id").AsInt()
		if err != nil {
			return err
		}
		userID, err := rowUUID(r, "user_id")
		if err != nil {
			return err
		}
		operationID, err := rowUUID(r, "operation_id")
		if err != nil {
			return err
		}
		entityID, err := rowUUID(r, "entity_id")
		if err != nil {
			return err
		}
		createdAt, err := rowTime(r, "created_at")
		if err != nil {
			return err
		}
		result := rowString(r, "result")
		if !json.Valid([]byte(result)) {
			return fmt.Errorf("processed operation %d: result is not valid JSON", id)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO processed_operations (id, user_id, operation_id, entity_type, entity_id, result, created_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
			ON CONFLICT (id) DO NOTHING`,
			id, userID, operationID, rowString(r, "entity_type"), entityID, result, createdAt,
		); err != nil {
			return fmt.Errorf("processed operation %d: %w", id, err)
		}
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
