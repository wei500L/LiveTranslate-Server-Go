-- Tombstone retention: hard-delete tombstoned rows older than
-- TOMBSTONE_RETENTION_DAYS. Rows deleted more recently stay so offline
-- devices still receive the delete event through the change log. Change-log
-- rows themselves are retained for the same window (a device offline longer
-- than the window must do a fresh initial upload; the iOS client handles
-- this via the forced fresh-start path).

-- +goose Up
-- Tombstone GC is implemented in internal/sync/cleanup.go as a periodic
-- task; this migration reserves the version slot and documents the policy.

-- +goose Down
