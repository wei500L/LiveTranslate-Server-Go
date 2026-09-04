-- Exam center & executable study plans: exams, their knowledge topics,
-- study plans, plan items and study activities — the 考试中心 layer.
--
-- Wire entity names: "exam" (4), "exam_topic" (10), "study_plan" (10),
-- "study_plan_item" (15), "study_activity" (14) — all fit the
-- VARCHAR(32) entity_type columns widened by 00008.
--
-- course_id / exam_id / plan_id / plan_item_id / topic_id are PLAIN
-- nullable UUID columns (the term/card convention): rows may arrive
-- before their sources. A COURSE delete DETACHES exams and activities
-- (course_id cleared — 学习历史转入未归类, never tombstoned); an EXAM
-- delete cascades tombstones to its topics, plans and plan items
-- (server-side cascades, not FKs), while its study ACTIVITIES detach
-- (exam_id / plan_item_id cleared — the actual learning-time history
-- survives); a PLAN delete cascades to its items; an ITEM delete detaches
-- its activities' plan_item_id.
--
-- exams:
--   kind        = midterm|final|quiz|lab|oral|report|defense|custom.
--   status      = pending|scheduled|done|cancelled. `pending` is the
--                 AI-candidate state — clients keep candidates
--                 DEVICE-LOCAL (never pushed, no reminder, no plan);
--                 the value is wire-legal for forward tolerance only.
--   exam_date   = DATE (YYYY-MM-DD wall clock, the course-schedule
--                 convention). start_secs/end_secs are wall-clock
--                 seconds since midnight; -1 = time unknown / no end.
--   source      = JSONB; wire form a JSON STRING (the citations
--                 convention) — the candidate's origin snapshot (stable
--                 ids + the original relative wording, e.g. 下周三).
--                 Never image bytes, file paths or raw model responses.
--
-- exam_topics: the exam's real knowledge topics. status =
-- not_started|learning|needs_review|mastered — `mastered` is only ever
-- set by explicit user action (the client enforces it; the server merely
-- stores). self_rating = none|vague|basic|proficient. source = JSONB
-- string (same convention).
--
-- study_plans: one plan per exam (the client enforces single-active).
-- status = active|paused|archived. Dates are DATE columns; rest_days /
-- focus_topics / blocked_times ride as JSONB strings (weekday list /
-- topic-id list / time-ranges) — opaque metadata server-side.
--
-- study_plan_items: status = pending|in_progress|done|skipped|deferred.
-- `in_progress` is real persisted state (a timer running on another
-- device is honest information). status_changed_at is the STATUS MERGE
-- ORDER: on a same-version merge the status with the NEWER timestamp
-- wins, and done/skipped are sticky — a stale pending push can never
-- resurrect or blank progress. kind = material|session|review|task|
-- cards|topic|terms|practice|custom. source = JSONB string (the jump
-- target: material id + page, session id, task id, topic id…).
--
-- study_activities: append-style learning-time records (真实学习计时).
-- status = in_progress|completed|abandoned — completed/abandoned are
-- terminal and sticky (an append-style row is never re-opened by a
-- stale push). duration_seconds rides full desired state and merges by
-- MAX (multi-device accumulation must never lose minutes).

-- +goose Up

CREATE TABLE exams (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL,
    course_id      UUID,
    title          TEXT NOT NULL DEFAULT '',
    -- midterm|final|quiz|lab|oral|report|defense|custom.
    kind           VARCHAR(16) NOT NULL DEFAULT 'custom',
    -- Wall-clock date (DATE, YYYY-MM-DD).
    exam_date      DATE NOT NULL,
    -- Wall-clock seconds since midnight; -1 = unknown / none.
    start_secs     INT NOT NULL DEFAULT -1,
    end_secs       INT NOT NULL DEFAULT -1,
    location       TEXT NOT NULL DEFAULT '',
    -- 考试范围 (user/AI-edited free text).
    scope_text     TEXT NOT NULL DEFAULT '',
    note           TEXT NOT NULL DEFAULT '',
    target_score   TEXT NOT NULL DEFAULT '',
    -- pending|scheduled|done|cancelled (see header).
    status         VARCHAR(16) NOT NULL DEFAULT 'scheduled',
    -- manual|ai.
    origin         VARCHAR(16) NOT NULL DEFAULT 'manual',
    -- ExamSource JSON string (NULL = none).
    source         JSONB,
    server_version INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    deleted_at     TIMESTAMPTZ
);
CREATE INDEX ix_exams_user ON exams (user_id);
CREATE INDEX ix_exams_user_course ON exams (user_id, course_id);
CREATE INDEX ix_exams_user_date ON exams (user_id, exam_date);

CREATE TABLE exam_topics (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL,
    exam_id        UUID NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    -- low|normal|high.
    importance     VARCHAR(16) NOT NULL DEFAULT 'normal',
    -- none|vague|basic|proficient (user self-rating).
    self_rating    VARCHAR(16) NOT NULL DEFAULT 'none',
    -- not_started|learning|needs_review|mastered.
    status         VARCHAR(16) NOT NULL DEFAULT 'not_started',
    -- TopicSource JSON string (NULL = none).
    source         JSONB,
    -- Whether the user edited this row (regeneration preserves it).
    user_edited    BOOLEAN NOT NULL DEFAULT false,
    server_version INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    deleted_at     TIMESTAMPTZ
);
CREATE INDEX ix_exam_topics_user ON exam_topics (user_id);
CREATE INDEX ix_exam_topics_user_exam ON exam_topics (user_id, exam_id);

CREATE TABLE study_plans (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL,
    exam_id        UUID NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    start_date     DATE NOT NULL,
    end_date       DATE NOT NULL,
    weekday_minutes INT NOT NULL DEFAULT 60,
    weekend_minutes INT NOT NULL DEFAULT 90,
    -- Weekday-number JSON array string ('' = none).
    rest_days      JSONB,
    finish_early_days INT NOT NULL DEFAULT 0,
    include_cards  BOOLEAN NOT NULL DEFAULT true,
    include_tasks  BOOLEAN NOT NULL DEFAULT true,
    include_materials BOOLEAN NOT NULL DEFAULT true,
    include_sessions   BOOLEAN NOT NULL DEFAULT true,
    -- Topic-id JSON array string (NULL = none).
    focus_topics   JSONB,
    -- Blocked time-ranges JSON string (NULL = none).
    blocked_times  JSONB,
    -- active|paused|archived.
    status         VARCHAR(16) NOT NULL DEFAULT 'active',
    server_version INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    deleted_at     TIMESTAMPTZ
);
CREATE INDEX ix_study_plans_user ON study_plans (user_id);
CREATE INDEX ix_study_plans_user_exam ON study_plans (user_id, exam_id);

CREATE TABLE study_plan_items (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL,
    plan_id        UUID NOT NULL,
    exam_id        UUID,
    -- Wall-clock plan date (DATE, YYYY-MM-DD).
    item_date      DATE NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    -- material|session|review|task|cards|topic|terms|practice|custom.
    kind           VARCHAR(16) NOT NULL DEFAULT 'custom',
    estimated_minutes INT NOT NULL DEFAULT 30,
    actual_minutes INT NOT NULL DEFAULT 0,
    -- pending|in_progress|done|skipped|deferred (status merge header).
    status         VARCHAR(16) NOT NULL DEFAULT 'pending',
    status_changed_at TIMESTAMPTZ,
    item_order     INT NOT NULL DEFAULT 0,
    -- PlanItemSource JSON string (NULL = none) — the jump target.
    source         JSONB,
    user_note      TEXT NOT NULL DEFAULT '',
    user_edited    BOOLEAN NOT NULL DEFAULT false,
    server_version INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    deleted_at     TIMESTAMPTZ
);
CREATE INDEX ix_study_plan_items_user ON study_plan_items (user_id);
CREATE INDEX ix_study_plan_items_user_plan ON study_plan_items (user_id, plan_id);
CREATE INDEX ix_study_plan_items_user_date ON study_plan_items (user_id, item_date);

CREATE TABLE study_activities (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL,
    -- Nil = free study (not tied to a plan item).
    plan_item_id   UUID,
    -- Nil after the exam was deleted (the history survives).
    exam_id        UUID,
    course_id      UUID,
    topic_id       UUID,
    started_at     TIMESTAMPTZ NOT NULL,
    ended_at       TIMESTAMPTZ,
    -- Accumulated ACTIVE seconds (pauses excluded; merges by MAX).
    duration_seconds INT NOT NULL DEFAULT 0,
    -- in_progress|completed|abandoned (terminal states sticky).
    status         VARCHAR(16) NOT NULL DEFAULT 'in_progress',
    note           TEXT NOT NULL DEFAULT '',
    server_version INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    deleted_at     TIMESTAMPTZ
);
CREATE INDEX ix_study_activities_user ON study_activities (user_id);
CREATE INDEX ix_study_activities_user_item ON study_activities (user_id, plan_item_id);
CREATE INDEX ix_study_activities_user_exam ON study_activities (user_id, exam_id);

-- +goose Down

DROP TABLE IF EXISTS study_activities;
DROP TABLE IF EXISTS study_plan_items;
DROP TABLE IF EXISTS study_plans;
DROP TABLE IF EXISTS exam_topics;
DROP TABLE IF EXISTS exams;
