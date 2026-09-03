-- Course schedules and per-occurrence exceptions — the pre-class layer.
--
-- A CourseSchedule is a recurring rule ("Monday 10:30–12:05, weekly, this
-- semester") owned by a course; a ScheduleException is one dated deviation
-- of that rule (cancelled / moved / relocated / ad-hoc note). The server
-- stores ONLY rules and exceptions — occurrences are computed client-side;
-- no per-week rows are ever materialized here.
--
-- Wire entity names: "course_schedule" (15) and "schedule_exception" (17)
-- — both fit the VARCHAR(32) entity_type columns widened by 00008.
--
-- course_id / schedule_id are PLAIN nullable UUID columns (the term/card
-- convention): rows may arrive before their sources; deleting a source
-- never deletes the other side on the sync path. A COURSE delete, however,
-- cascades tombstones to its schedules (a schedule without its course is
-- meaningless), and a schedule delete cascades to its exceptions — both
-- via the server-side delete handlers, not FK constraints.
--
-- recurrence: weekly | biweekly | odd_weeks | even_weeks | once.
-- Odd/even is resolved against the user's semester first-week anchor on
-- the client (parity_index + first_week_parity travel with the row so all
-- devices compute the same calendar).
--
-- week_parity_anchor (schedule): the Monday starting the semester's week 1
-- in the course timezone; first_week_is_odd (BOOLEAN) fixes its parity.
-- Both ride the wire so every device derives identical odd/even weeks.
--
-- teacher_override / location_override / note: optional per-schedule
-- cover values; empty string = "no override" (the client shows the
-- course default). NULL is never needed — absent-on-wire keeps stored.
--
-- reminder lead: -1 = no reminder (the default; reminders are a local
-- device concern), 0 = at start, > 0 = minutes before start.
--
-- Exception kind: cancelled | time_changed | ad_hoc. Ad-hoc rows carry a
-- schedule_id but no original date dependency — they ADD one occurrence
-- (a one-off extra class). changed_start/end, location_override,
-- teacher_override and note apply per kind.
--
-- occurrence_key: the client-computed stable id for ONE concrete class
-- ("scheduleUUID:YYYY-MM-DD" in the course timezone). Sessions store it
-- so attendance and duplicate-start protection survive rule edits; the
-- server never parses it, it is an opaque grouping string.

-- +goose Up

CREATE TABLE course_schedules (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    course_id           UUID,
    -- 0=Sun .. 6=Sat, interpreted in the course timezone.
    weekday             INT NOT NULL DEFAULT 1,
    -- Local wall-clock seconds since midnight in the course timezone.
    start_secs          INT NOT NULL DEFAULT 0,
    end_secs            INT NOT NULL DEFAULT 0,
    recurrence          VARCHAR(16) NOT NULL DEFAULT 'weekly', -- weekly|biweekly|odd_weeks|even_weeks|once
    -- Monday that starts week 1 of the term (NULL when recurrence needs
    -- no parity: weekly/once).
    week_parity_anchor  DATE,
    first_week_is_odd   BOOLEAN NOT NULL DEFAULT TRUE,
    semester_start      DATE NOT NULL,
    semester_end        DATE NOT NULL,
    -- IANA timezone id; never a fixed UTC offset.
    timezone            VARCHAR(64) NOT NULL DEFAULT 'UTC',
    teacher_override    TEXT NOT NULL DEFAULT '',
    location_override   TEXT NOT NULL DEFAULT '',
    note                TEXT NOT NULL DEFAULT '',
    -- -1 none | 0 at start | >0 minutes before start.
    reminder_lead_mins  INT NOT NULL DEFAULT -1,
    is_enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    -- Single-date recurrence (recurrence='once'): the one date it runs.
    once_date           DATE,
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_course_schedules_user ON course_schedules (user_id);
CREATE INDEX ix_course_schedules_user_course ON course_schedules (user_id, course_id);

CREATE TABLE schedule_exceptions (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    schedule_id         UUID NOT NULL,
    course_id           UUID,
    -- The originally planned date (in the course timezone) this exception
    -- replaces. NULL for ad_hoc (a one-off extra occurrence).
    original_date       DATE,
    exception_kind      VARCHAR(16) NOT NULL DEFAULT 'cancelled', -- cancelled|time_changed|ad_hoc
    -- Shifted wall-clock times (seconds since midnight, course tz).
    changed_start       INT,
    changed_end         INT,
    -- Date relocation: when set the occurrence moves to this date.
    moved_to_date       DATE,
    location_override   TEXT NOT NULL DEFAULT '',
    teacher_override    TEXT NOT NULL DEFAULT '',
    note                TEXT NOT NULL DEFAULT '',
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_schedule_exceptions_user ON schedule_exceptions (user_id);
CREATE INDEX ix_schedule_exceptions_user_schedule ON schedule_exceptions (user_id, schedule_id);

-- +goose Down

DROP TABLE IF EXISTS schedule_exceptions;
DROP TABLE IF EXISTS course_schedules;
