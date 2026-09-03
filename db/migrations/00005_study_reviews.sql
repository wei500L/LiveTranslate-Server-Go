-- AI study reviews: one post-class review per classroom session.
--
-- The review entity's id IS the session id (the same pattern bookmarks
-- and favorites use for their targets): one review per session per user,
-- structurally — two devices can never create competing rows for the
-- same classroom.
--
-- Content versioning (product semantics, see iOS StudyReview):
--   content           — what the user currently reads/edits (may carry
--                       user edits and user additions);
--   generated_content — the model's structured output as parsed at
--                       generation time (replaced only by an explicit
--                       user-initiated regeneration).
-- Only the structured result syncs — never prompts, raw model responses
-- or request parameters.
--
-- Chunk-level intermediate state (per-device generation progress) is
-- intentionally NOT synced: it is only meaningful on the device that is
-- generating.

-- +goose Up

CREATE TABLE study_reviews (
    id               UUID PRIMARY KEY,          -- == classroom_sessions.id
    user_id          UUID NOT NULL,
    session_id       UUID NOT NULL,
    status           VARCHAR(32) NOT NULL DEFAULT 'completed', -- generating|completed|partial|failed
    content          TEXT NOT NULL DEFAULT '',
    generated_content TEXT NOT NULL DEFAULT '',
    review_model     VARCHAR(128) NOT NULL DEFAULT '',
    generated_at     TIMESTAMPTZ,
    source_updated_at TIMESTAMPTZ,
    server_version   INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    deleted_at       TIMESTAMPTZ
);
CREATE INDEX ix_study_reviews_user ON study_reviews (user_id);
CREATE INDEX ix_study_reviews_session ON study_reviews (user_id, session_id);

-- +goose Down

DROP TABLE IF EXISTS study_reviews;
