package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"livetranslate/server/internal/store"
)

// The Python Alembic 0001 schema, column lists verbatim. Explicit columns
// (never SELECT *): the copy fails loudly if the source shape differs
// instead of silently misaligning.
var sourceColumns = map[string]string{
	"users":                "id, apple_subject, dev_name, created_at, updated_at, deleted_at",
	"devices":              "id, user_id, client_device_id, display_name, app_version, last_seen_at, revoked_at",
	"refresh_tokens":       "id, user_id, device_id, token_hash, expires_at, revoked_at, replaced_by, created_at",
	"classroom_sessions":   "id, user_id, title, started_at, ended_at, duration, source_language, target_language, session_status, abnormal_termination, server_version, created_at, updated_at, deleted_at",
	"transcript_entries":   "id, user_id, session_id, sequence_id, start_offset, end_offset, russian_text, chinese_text, translation_status, server_version, created_at, updated_at, deleted_at",
	"bookmarks":            "id, user_id, session_id, entry_id, is_bookmarked, server_version, updated_at, deleted_at",
	"favorite_sessions":    "id, user_id, session_id, is_favorite, server_version, updated_at, deleted_at",
	"sync_changes":         "change_sequence, user_id, entity_type, entity_id, operation, server_version, created_at",
	"processed_operations": "id, user_id, operation_id, entity_type, entity_id, result, created_at",
}

// copyTable streams the source table row by row and copies each row with
// its EXPLICIT ids (identities and relations preserved verbatim). ON
// CONFLICT DO NOTHING makes a re-run after a failed import idempotent; in a
// --allow-non-empty merge, rows whose id already exists in the target are
// SKIPPED (the target's row wins).
func copyTable(ctx context.Context, q store.Q, src *sql.DB, table string) (int, error) {
	rows, err := src.QueryContext(ctx, "SELECT "+sourceColumns[table]+" FROM "+table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	scan := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range scan {
		ptrs[i] = &scan[i]
	}

	copied := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return copied, err
		}
		// Column-name keyed view of the row (values are driver scalars:
		// string / int64 / float64 / []byte / nil).
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = scan[i]
		}
		if err := copyRow(ctx, q, table, row); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, rows.Err()
}

// --- Value coercion (driver scalars → typed values) -----------------------------

// cellString renders TEXT/INTEGER/REAL cells; NULL and BLOB → "".
func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func cellNullString(v any) any {
	s := cellString(v)
	if s == "" {
		return nil
	}
	return s
}

func cellInt(v any, fallback int64) (int64, error) {
	switch t := v.(type) {
	case nil:
		return fallback, nil
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n, nil
		}
		return 0, fmt.Errorf("not an integer value %q", t)
	default:
		return 0, fmt.Errorf("not an integer value %v", v)
	}
}

func cellFloat(v any, fallback float64) (float64, error) {
	switch t := v.(type) {
	case nil:
		return fallback, nil
	case float64:
		return t, nil
	case int64:
		return float64(t), nil
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f, nil
		}
		return 0, fmt.Errorf("not a numeric value %q", t)
	default:
		return 0, fmt.Errorf("not a numeric value %v", v)
	}
}

func cellBool(v any, fallback bool) (bool, error) {
	if v == nil {
		return fallback, nil
	}
	n, err := cellInt(v, b2i(fallback))
	return n != 0, err
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// cellTime parses SQLAlchemy's SQLite datetime strings
// ("2006-01-02 15:04:05.635064+00:00"; space separator; offset optional).
func cellTime(v any) (*time.Time, error) {
	raw := strings.TrimSpace(cellString(v))
	if raw == "" {
		return nil, nil
	}
	normalized := strings.Replace(raw, "T", " ", 1)
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, normalized); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("unparseable datetime %q", raw)
}

func cellTimeRequired(row map[string]any, col string) (time.Time, error) {
	t, err := cellTime(row[col])
	if err != nil {
		return time.Time{}, fmt.Errorf("column %s: %w", col, err)
	}
	if t == nil {
		return time.Time{}, fmt.Errorf("column %s: required timestamp is NULL", col)
	}
	return *t, nil
}

// cellUUID accepts both the dashed 36-char form (PostgreSQL) and the
// 32-hex form SQLAlchemy's Uuid type writes on SQLite.
func cellUUID(v any) (uuid.UUID, error) {
	raw := strings.ToLower(strings.TrimSpace(cellString(v)))
	switch len(raw) {
	case 36:
		return uuid.Parse(raw)
	case 32:
		dashed := raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32]
		return uuid.Parse(dashed)
	default:
		return uuid.Nil, fmt.Errorf("not a UUID value %q", raw)
	}
}

func cellUUIDRequired(row map[string]any, col string) (uuid.UUID, error) {
	id, err := cellUUID(row[col])
	if err != nil {
		return uuid.Nil, fmt.Errorf("column %s: %w", col, err)
	}
	return id, nil
}

// --- Per-table copiers -----------------------------------------------------------

func copyRow(ctx context.Context, q store.Q, table string, r map[string]any) error {
	switch table {
	case "users":
		return copyUser(ctx, q, r)
	case "devices":
		return copyDevice(ctx, q, r)
	case "refresh_tokens":
		return copyRefreshToken(ctx, q, r)
	case "classroom_sessions":
		return copySession(ctx, q, r)
	case "transcript_entries":
		return copyEntry(ctx, q, r)
	case "bookmarks":
		return copyBookmark(ctx, q, r)
	case "favorite_sessions":
		return copyFavorite(ctx, q, r)
	case "sync_changes":
		return copySyncChange(ctx, q, r)
	case "processed_operations":
		return copyProcessedOp(ctx, q, r)
	default:
		return fmt.Errorf("unexpected table %s", table)
	}
}

func copyUser(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellUUIDRequired(r, "id")
	if err != nil {
		return err
	}
	createdAt, err := cellTimeRequired(r, "created_at")
	if err != nil {
		return err
	}
	updatedAt, err := cellTimeRequired(r, "updated_at")
	if err != nil {
		return err
	}
	deletedAt, err := cellTime(r["deleted_at"])
	if err != nil {
		return fmt.Errorf("column deleted_at: %w", err)
	}
	// Legacy columns only: the Python schema had NO email — imported users
	// keep their Apple/dev identity and can set a password later through
	// the reset flow. No default password is ever generated.
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
		id, status, cellNullString(r["apple_subject"]), cellNullString(r["dev_name"]),
		createdAt, updatedAt, deletedAt, deletedAt,
	); err != nil {
		return fmt.Errorf("user %s: %w", id, err)
	}
	return nil
}

func copyDevice(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellUUIDRequired(r, "id")
	if err != nil {
		return err
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	lastSeen, err := cellTimeRequired(r, "last_seen_at")
	if err != nil {
		return err
	}
	revokedAt, err := cellTime(r["revoked_at"])
	if err != nil {
		return fmt.Errorf("column revoked_at: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO devices (id, user_id, client_device_id, display_name, app_version, last_seen_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		id, userID, cellString(r["client_device_id"]), cellString(r["display_name"]),
		cellString(r["app_version"]), lastSeen, revokedAt,
	); err != nil {
		return fmt.Errorf("device %s: %w", id, err)
	}
	return nil
}

func copyRefreshToken(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellUUIDRequired(r, "id")
	if err != nil {
		return err
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	deviceID, err := cellUUIDRequired(r, "device_id")
	if err != nil {
		return err
	}
	expiresAt, err := cellTimeRequired(r, "expires_at")
	if err != nil {
		return err
	}
	createdAt, err := cellTimeRequired(r, "created_at")
	if err != nil {
		return err
	}
	revokedAt, err := cellTime(r["revoked_at"])
	if err != nil {
		return fmt.Errorf("column revoked_at: %w", err)
	}
	// token_hash is a SHA-256 digest, not a credential; keeping it lets
	// already-signed-in devices continue their refresh chains.
	var replacedBy any
	if cellString(r["replaced_by"]) != "" {
		rid, err := cellUUID(r["replaced_by"])
		if err != nil {
			return fmt.Errorf("token %s: %w", id, err)
		}
		replacedBy = rid
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, device_id, token_hash, expires_at, revoked_at, replaced_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO NOTHING`,
		id, userID, deviceID, cellString(r["token_hash"]), expiresAt, revokedAt, replacedBy, createdAt,
	); err != nil {
		return fmt.Errorf("refresh token %s: %w", id, err)
	}
	return nil
}

func copySession(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellUUIDRequired(r, "id")
	if err != nil {
		return err
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	startedAt, err := cellTimeRequired(r, "started_at")
	if err != nil {
		return err
	}
	endedAt, err := cellTime(r["ended_at"])
	if err != nil {
		return fmt.Errorf("column ended_at: %w", err)
	}
	deletedAt, err := cellTime(r["deleted_at"])
	if err != nil {
		return fmt.Errorf("column deleted_at: %w", err)
	}
	createdAt, err := cellTimeRequired(r, "created_at")
	if err != nil {
		return err
	}
	updatedAt, err := cellTimeRequired(r, "updated_at")
	if err != nil {
		return err
	}
	duration, err := cellFloat(r["duration"], 0)
	if err != nil {
		return fmt.Errorf("column duration: %w", err)
	}
	abnormal, err := cellBool(r["abnormal_termination"], false)
	if err != nil {
		return fmt.Errorf("column abnormal_termination: %w", err)
	}
	serverVersion, err := cellInt(r["server_version"], 0)
	if err != nil {
		return fmt.Errorf("column server_version: %w", err)
	}
	title := cellString(r["title"])
	if len(title) > 256 {
		title = title[:256] // column limit; longer legacy titles truncate
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO classroom_sessions (id, user_id, title, started_at, ended_at, duration,
			source_language, target_language, session_status, abnormal_termination,
			server_version, created_at, updated_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (id) DO NOTHING`,
		id, userID, title, startedAt, endedAt, duration,
		orDefault(cellString(r["source_language"]), "ru"),
		orDefault(cellString(r["target_language"]), "zh-CN"),
		orDefault(cellString(r["session_status"]), "active"),
		abnormal, serverVersion, createdAt, updatedAt, deletedAt,
	); err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	return nil
}

func copyEntry(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellUUIDRequired(r, "id")
	if err != nil {
		return err
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	sessionID, err := cellUUIDRequired(r, "session_id")
	if err != nil {
		return err
	}
	sequenceID, err := cellInt(r["sequence_id"], 0)
	if err != nil {
		return fmt.Errorf("column sequence_id: %w", err)
	}
	startOffset, err := cellFloat(r["start_offset"], 0)
	if err != nil {
		return fmt.Errorf("column start_offset: %w", err)
	}
	endOffset, err := cellFloat(r["end_offset"], 0)
	if err != nil {
		return fmt.Errorf("column end_offset: %w", err)
	}
	createdAt, err := cellTimeRequired(r, "created_at")
	if err != nil {
		return err
	}
	updatedAt, err := cellTimeRequired(r, "updated_at")
	if err != nil {
		return err
	}
	deletedAt, err := cellTime(r["deleted_at"])
	if err != nil {
		return fmt.Errorf("column deleted_at: %w", err)
	}
	serverVersion, err := cellInt(r["server_version"], 0)
	if err != nil {
		return fmt.Errorf("column server_version: %w", err)
	}
	// chinese_text is nullable; NULL stays NULL (not '').
	if _, err := q.Exec(ctx, `
		INSERT INTO transcript_entries (id, user_id, session_id, sequence_id,
			start_offset, end_offset, russian_text, chinese_text, translation_status,
			server_version, created_at, updated_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (id) DO NOTHING`,
		id, userID, sessionID, sequenceID, startOffset, endOffset,
		cellString(r["russian_text"]), cellNullString(r["chinese_text"]),
		orDefault(cellString(r["translation_status"]), "pending"),
		serverVersion, createdAt, updatedAt, deletedAt,
	); err != nil {
		return fmt.Errorf("entry %s: %w", id, err)
	}
	return nil
}

func copyBookmark(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellUUIDRequired(r, "id")
	if err != nil {
		return err
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	sessionID, err := cellUUIDRequired(r, "session_id")
	if err != nil {
		return err
	}
	entryID, err := cellUUIDRequired(r, "entry_id")
	if err != nil {
		return err
	}
	updatedAt, err := cellTimeRequired(r, "updated_at")
	if err != nil {
		return err
	}
	deletedAt, err := cellTime(r["deleted_at"])
	if err != nil {
		return fmt.Errorf("column deleted_at: %w", err)
	}
	serverVersion, err := cellInt(r["server_version"], 0)
	if err != nil {
		return fmt.Errorf("column server_version: %w", err)
	}
	isBookmarked, err := cellBool(r["is_bookmarked"], true)
	if err != nil {
		return fmt.Errorf("column is_bookmarked: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO bookmarks (id, user_id, session_id, entry_id, is_bookmarked, server_version, updated_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO NOTHING`,
		id, userID, sessionID, entryID, isBookmarked, serverVersion, updatedAt, deletedAt,
	); err != nil {
		return fmt.Errorf("bookmark %s: %w", id, err)
	}
	return nil
}

func copyFavorite(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellUUIDRequired(r, "id")
	if err != nil {
		return err
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	sessionID, err := cellUUIDRequired(r, "session_id")
	if err != nil {
		return err
	}
	updatedAt, err := cellTimeRequired(r, "updated_at")
	if err != nil {
		return err
	}
	deletedAt, err := cellTime(r["deleted_at"])
	if err != nil {
		return fmt.Errorf("column deleted_at: %w", err)
	}
	serverVersion, err := cellInt(r["server_version"], 0)
	if err != nil {
		return fmt.Errorf("column server_version: %w", err)
	}
	isFavorite, err := cellBool(r["is_favorite"], true)
	if err != nil {
		return fmt.Errorf("column is_favorite: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO favorite_sessions (id, user_id, session_id, is_favorite, server_version, updated_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		id, userID, sessionID, isFavorite, serverVersion, updatedAt, deletedAt,
	); err != nil {
		return fmt.Errorf("favorite %s: %w", id, err)
	}
	return nil
}

// copySyncChange preserves change_sequence EXPLICITLY — it is the sync
// cursor domain; after import, new changes continue after the imported max.
func copySyncChange(ctx context.Context, q store.Q, r map[string]any) error {
	seq, err := cellInt(r["change_sequence"], 0)
	if err != nil {
		return fmt.Errorf("column change_sequence: %w", err)
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	entityID, err := cellUUIDRequired(r, "entity_id")
	if err != nil {
		return err
	}
	createdAt, err := cellTimeRequired(r, "created_at")
	if err != nil {
		return err
	}
	serverVersion, err := cellInt(r["server_version"], 0)
	if err != nil {
		return fmt.Errorf("column server_version: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO sync_changes (change_sequence, user_id, entity_type, entity_id, operation, server_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (change_sequence) DO NOTHING`,
		seq, userID, cellString(r["entity_type"]), entityID,
		cellString(r["operation"]), serverVersion, createdAt,
	); err != nil {
		return fmt.Errorf("sync change %d: %w", seq, err)
	}
	return nil
}

// copyProcessedOp preserves the idempotency ledger (id + result JSON).
func copyProcessedOp(ctx context.Context, q store.Q, r map[string]any) error {
	id, err := cellInt(r["id"], 0)
	if err != nil {
		return fmt.Errorf("column id: %w", err)
	}
	userID, err := cellUUIDRequired(r, "user_id")
	if err != nil {
		return err
	}
	operationID, err := cellUUIDRequired(r, "operation_id")
	if err != nil {
		return err
	}
	entityID, err := cellUUIDRequired(r, "entity_id")
	if err != nil {
		return err
	}
	createdAt, err := cellTimeRequired(r, "created_at")
	if err != nil {
		return err
	}
	result := cellString(r["result"])
	if !json.Valid([]byte(result)) {
		return fmt.Errorf("processed operation %d: result is not valid JSON", id)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO processed_operations (id, user_id, operation_id, entity_type, entity_id, result, created_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
		ON CONFLICT (id) DO NOTHING`,
		id, userID, operationID, cellString(r["entity_type"]), entityID, result, createdAt,
	); err != nil {
		return fmt.Errorf("processed operation %d: %w", id, err)
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
