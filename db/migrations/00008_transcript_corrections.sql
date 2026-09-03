-- Transcript corrections: the user's manual edit layer over the
-- immutable model output, plus the entry time-provenance column.
--
-- Design (mirrors the iOS TranscriptCorrection @Model):
-- - id == transcript_entries.id (one correction per entry, structural);
-- - the entry's russian_text / chinese_text stay the MODEL's output and
--   keep their immutability rules (applyEntry still rejects a differing
--   russianText); corrections are a separate sync entity so the original
--   can never be overwritten, only overlaid;
-- - deleting a correction means "revert to the model original" — the
--   tombstone bumps the version, so an old device's upsert cannot
--   resurrect a reverted correction (it lands on the delete-wins path).
--
-- Multi-device conflicts: the side with the NEWER modified_at wins
-- (server-side rule in applyCorrection; the client mirrors it in
-- applyRemoteCorrection and additionally preserves the losing side as a
-- local conflict copy the user can adopt).
--
-- chinese_text semantics: NULL = the user never corrected the Chinese
-- (the model translation applies); '' = a deliberate blank correction.
--
-- transcript_entries.time_source: provenance of the entry's audio
-- offsets — 'audio' (from the recorded sample timeline), 'legacy'
-- (stored by an older version, same derivation, unmarked). Absent
-- payload field keeps the stored value (the merge default is 'legacy').
--
-- session_notes.time_offset: classroom-relative seconds when the note
-- was taken (live time or playback position); NULL = legacy note with
-- no recorded position (the note rides without one; the client falls
-- back to createdAt − session.startTime as an approximation).
--
-- Wire entity name: "transcript_correction" (21 chars) — the entity_type
-- columns in sync_changes / processed_operations were VARCHAR(16), so
-- this migration widens them to VARCHAR(32).

-- +goose Up

ALTER TABLE sync_changes ALTER COLUMN entity_type TYPE VARCHAR(32);
ALTER TABLE processed_operations ALTER COLUMN entity_type TYPE VARCHAR(32);

ALTER TABLE transcript_entries
    ADD COLUMN time_source VARCHAR(8) NOT NULL DEFAULT 'legacy';

ALTER TABLE session_notes
    ADD COLUMN time_offset DOUBLE PRECISION;

CREATE TABLE transcript_corrections (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    session_id          UUID NOT NULL,
    -- The user's corrected Russian. '' = the Russian correction is empty
    -- (the model original applies).
    russian_text        TEXT NOT NULL DEFAULT '',
    -- NULL = never corrected (model translation applies); '' = a
    -- deliberate blank.
    chinese_text        TEXT,
    modified_at         TIMESTAMPTZ NOT NULL,
    needs_retranslation BOOLEAN NOT NULL DEFAULT FALSE,
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_corrections_user_id ON transcript_corrections (user_id);
CREATE INDEX ix_corrections_user_session ON transcript_corrections (user_id, session_id);

-- +goose Down

DROP TABLE IF EXISTS transcript_corrections;
ALTER TABLE transcript_entries DROP COLUMN IF EXISTS time_source;
ALTER TABLE session_notes DROP COLUMN IF EXISTS time_offset;
ALTER TABLE sync_changes ALTER COLUMN entity_type TYPE VARCHAR(16);
ALTER TABLE processed_operations ALTER COLUMN entity_type TYPE VARCHAR(16);
