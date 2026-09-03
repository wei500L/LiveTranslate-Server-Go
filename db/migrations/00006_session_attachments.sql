-- Session attachments: classroom photos (blackboards, slides, handwritten
-- notes, documents) linked to a session and optionally anchored to one
-- transcript entry.
--
-- Only metadata and the structured analysis sync through the change log.
-- Binary files (original + preview variants) travel on the dedicated
-- /v1/attachments upload/download routes and live on the server
-- filesystem (ATTACHMENT_STORAGE_DIR), keyed by (user_id, id, variant).
-- The row's content_hash/file_size are the contract the upload route
-- verifies against, so a synced row always describes its files.
--
-- Wire entity name is "attachment" (11 chars — sync_changes.entity_type
-- is VARCHAR(16); "session_attachment" would not fit).
--
-- analysis holds the versioned structured result of the multimodal
-- model (schemaVersion, visibleText, formulas, codeBlocks, keyPoints,
-- explanation, uncertainties, transcriptReferences) as produced by the
-- iOS AttachmentAnalysisParser. ocr_text is the separate local Vision
-- OCR text — never merged with the model result.

-- +goose Up

CREATE TABLE session_attachments (
    id               UUID PRIMARY KEY,
    user_id          UUID NOT NULL,
    session_id       UUID NOT NULL,
    course_id        UUID,
    anchor_entry_id  UUID,
    captured_at      TIMESTAMPTZ NOT NULL,
    title            TEXT NOT NULL DEFAULT '',
    caption          TEXT NOT NULL DEFAULT '',
    kind             VARCHAR(32) NOT NULL DEFAULT 'other', -- blackboard|slides|handwriting|document|chart|code|other
    mime_type        VARCHAR(64) NOT NULL DEFAULT '',
    pixel_width      INT NOT NULL DEFAULT 0,
    pixel_height     INT NOT NULL DEFAULT 0,
    file_size        BIGINT NOT NULL DEFAULT 0,
    content_hash     VARCHAR(64) NOT NULL DEFAULT '',
    sort_index       INT NOT NULL DEFAULT 0,
    -- Non-destructive display transform (rotation + normalized crop) as
    -- JSON; the stored file bytes are never modified.
    transform_json   TEXT NOT NULL DEFAULT '',
    analysis_status  VARCHAR(32) NOT NULL DEFAULT 'pending', -- pending|analyzing|completed|partial|failed
    analysis         JSONB,
    ocr_text         TEXT NOT NULL DEFAULT '',
    server_version   INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    deleted_at       TIMESTAMPTZ
);
CREATE INDEX ix_attachments_user ON session_attachments (user_id);
CREATE INDEX ix_attachments_session ON session_attachments (user_id, session_id);
CREATE INDEX ix_attachments_tombstone ON session_attachments (deleted_at) WHERE deleted_at IS NOT NULL;

-- +goose Down

DROP TABLE IF EXISTS session_attachments;
