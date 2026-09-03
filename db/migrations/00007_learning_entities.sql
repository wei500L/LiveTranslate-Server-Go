-- Learning entities: glossary terms, study cards (spaced repetition) and
-- assignment tasks — the persistent layer of the review center.
--
-- These are user-editable, reviewable, completable first-class entities,
-- NOT fields inside the study review's JSON: a term survives its review,
-- a card keeps its schedule when the review is regenerated, a task is
-- completed independently of the session that produced it.
--
-- Wire entity names: "term" (4), "study_card" (10), "study_task" (10) —
-- all fit the VARCHAR(16) sync_changes.entity_type.
--
-- Source references are PLAIN nullable UUID columns (the same
-- session-note/attachment convention): rows may arrive before their
-- sources, and deleting a source NEVER deletes the learning material —
-- the client shows "来源已不存在" for dangling refs. Course deletion
-- clears course_id (detach), session deletion leaves the rows alone.
--
-- source_session_ids (terms only): JSON array of session UUID strings —
-- the term's accumulated classroom sources after dedup merges. Stored as
-- TEXT (the server never queries inside it; the client owns the format).
--
-- Review state (study_cards): due_at, last_reviewed_at, review_count,
-- stage, interval_hours, last_grade. Merge rule on the server: the side
-- with the NEWER last_reviewed_at wins for all review-state fields, so a
-- device that reviewed later never loses its schedule.
--
-- Task status: pending | pending_confirm | done | ignored.
-- pending_confirm exists only pre-confirmation on the originating device
-- (AI candidates the user has not approved yet are never pushed); the
-- column still accepts it so a future unconfirmed sync cannot corrupt
-- rows. "done" is sticky: an incoming non-done status cannot resurrect a
-- completed task on the server (the client resolver mirrors this).

-- +goose Up

CREATE TABLE glossary_terms (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    course_id           UUID,
    session_id          UUID,
    source_review_id    UUID,
    source_entry_id     UUID,
    source_attachment_id UUID,
    -- JSON array of session UUIDs (accumulated sources after merges).
    source_session_ids  TEXT NOT NULL DEFAULT '',
    russian             TEXT NOT NULL DEFAULT '',
    chinese             TEXT NOT NULL DEFAULT '',
    explanation         TEXT NOT NULL DEFAULT '',
    part_of_speech      VARCHAR(32) NOT NULL DEFAULT '',
    user_note            TEXT NOT NULL DEFAULT '',
    is_favorite         BOOLEAN NOT NULL DEFAULT FALSE,
    status              VARCHAR(32) NOT NULL DEFAULT 'new', -- new|learning|familiar|mastered
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_glossary_terms_user ON glossary_terms (user_id);
CREATE INDEX ix_glossary_terms_user_course ON glossary_terms (user_id, course_id);

CREATE TABLE study_cards (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    course_id           UUID,
    session_id          UUID,
    source_entry_id     UUID,
    source_attachment_id UUID,
    source_term_id      UUID,
    front               TEXT NOT NULL DEFAULT '',
    back                TEXT NOT NULL DEFAULT '',
    card_type           VARCHAR(32) NOT NULL DEFAULT 'qa', -- ru2zh|zh2ru|qa|concept|formula|code
    user_note            TEXT NOT NULL DEFAULT '',
    origin              VARCHAR(16) NOT NULL DEFAULT 'manual', -- manual|ai
    stage               VARCHAR(32) NOT NULL DEFAULT 'new', -- new|learning|young|mature
    review_count        INT NOT NULL DEFAULT 0,
    interval_hours      INT NOT NULL DEFAULT 0,
    due_at              TIMESTAMPTZ,
    last_reviewed_at    TIMESTAMPTZ,
    last_grade          VARCHAR(32) NOT NULL DEFAULT '', -- forgot|hard|good|easy ('' = never)
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_study_cards_user ON study_cards (user_id);
CREATE INDEX ix_study_cards_user_course ON study_cards (user_id, course_id);
CREATE INDEX ix_study_cards_user_due ON study_cards (user_id, due_at) WHERE deleted_at IS NULL;

CREATE TABLE study_tasks (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    course_id           UUID,
    session_id          UUID,
    source_review_id    UUID,
    source_entry_id     UUID,
    source_attachment_id UUID,
    title               TEXT NOT NULL DEFAULT '',
    detail              TEXT NOT NULL DEFAULT '',
    due_at              TIMESTAMPTZ,
    priority            VARCHAR(16) NOT NULL DEFAULT 'normal', -- low|normal|high
    status              VARCHAR(32) NOT NULL DEFAULT 'pending', -- pending|pending_confirm|done|ignored
    origin              VARCHAR(16) NOT NULL DEFAULT 'manual', -- manual|ai
    uncertainty         TEXT NOT NULL DEFAULT '',
    user_note            TEXT NOT NULL DEFAULT '',
    completed_at        TIMESTAMPTZ,
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_study_tasks_user ON study_tasks (user_id);
CREATE INDEX ix_study_tasks_user_course ON study_tasks (user_id, course_id);
CREATE INDEX ix_study_tasks_user_status ON study_tasks (user_id, status) WHERE deleted_at IS NULL;

-- +goose Down

DROP TABLE IF EXISTS study_tasks;
DROP TABLE IF EXISTS study_cards;
DROP TABLE IF EXISTS glossary_terms;
