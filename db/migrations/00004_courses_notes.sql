-- Courses and classroom notes.
--
-- Two new synced entities:
--
--   courses       — a reusable course a student attends weekly (name,
--                   optional teacher/location, palette color, archive
--                   state). Classroom sessions reference a course by id;
--                   deleting a course leaves its sessions standalone
--                   (course_id is NULLed, sessions are never deleted).
--   session_notes — user-typed notes for one classroom session,
--                   optionally anchored to a transcript entry.
--
-- Both follow the established sync-table shape: user_id isolation,
-- server_version optimistic concurrency, deleted_at tombstones. The
-- Go-side session delete cascade tombstones session_notes alongside
-- entries/bookmarks/favorites. course_id on classroom_sessions is a
-- plain nullable UUID (no FK): courses and sessions sync independently
-- and a tombstoned course must not block session writes — the sync
-- service nullifies course_id when a course is deleted.

-- +goose Up

CREATE TABLE courses (
    id            UUID PRIMARY KEY,
    user_id       UUID NOT NULL,
    name          VARCHAR(256) NOT NULL DEFAULT '',
    teacher       VARCHAR(128) NOT NULL DEFAULT '',
    location      VARCHAR(128) NOT NULL DEFAULT '',
    color_index   INT NOT NULL DEFAULT 0,
    is_archived   BOOLEAN NOT NULL DEFAULT FALSE,
    server_version INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    deleted_at    TIMESTAMPTZ
);
CREATE INDEX ix_courses_user ON courses (user_id);

ALTER TABLE classroom_sessions ADD COLUMN course_id UUID;
CREATE INDEX ix_sessions_course ON classroom_sessions (user_id, course_id);

CREATE TABLE session_notes (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL,
    session_id      UUID NOT NULL,
    anchor_entry_id UUID,
    note_text       TEXT NOT NULL DEFAULT '',
    server_version  INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX ix_notes_user_session ON session_notes (user_id, session_id);
CREATE INDEX ix_notes_user_id ON session_notes (user_id);

-- +goose Down

DROP TABLE IF EXISTS session_notes;
DROP INDEX IF EXISTS ix_sessions_course;
ALTER TABLE classroom_sessions DROP COLUMN IF EXISTS course_id;
DROP TABLE IF EXISTS courses;
