-- Interpreter conversations (随身翻译): saved face-to-face errand
-- dialogs between the Russian-speaking staff and the Chinese user.
--
-- Wire entity names: "interpreter_conversation" (24 chars) and
-- "interpreter_turn" (17 chars) — both fit the VARCHAR(32) entity_type
-- columns widened by migration 00008.
--
-- Lifecycle: the client keeps the working session as a DEVICE-LOCAL
-- draft (never pushed); only SAVED conversations ever reach the wire.
-- status = draft|saved|discarded is therefore client-managed state the
-- server merely stores ('draft'/'discarded' are wire-legal for forward
-- tolerance; the server accepts them without interpretation).
--
-- interpreter_conversations:
--   scene = general|school|dorm|bank|hospital|migration|telecom|post
--           (the errand scene selector; the client enforces the set).
--   context_note = the user's temporary background line (我是莫斯科国立
--          大学留学生…) — full desired state, empty string clears.
--   title defaults to '<scene label> · M月d日' on the client; never
--           AI-generated as a save precondition.
--
-- interpreter_turns: one dialogue round.
--   speaker = counterpart|user. direction = ru2zh|zh2ru (who spoke and
--           which way the translation ran). input_method = audio|text.
--   source_text = what that speaker actually said (Russian for the
--           counterpart, Chinese for the user).
--   plain_russian  / stressed_russian: the plain and U+0301-stressed
--           Russian renderings (kept side by side — the stressed form is
--           display-only; TTS and search read the plain one).
--   chinese_text = the natural Chinese translation (counterpart turns)
--           or the user's original Chinese (user turns).
--   back_translation = the Chinese back-translation shown for the user
--           to verify a zh2ru turn.
--   details = JSONB; wire form a JSON STRING (the citations convention):
--           the structured-detail snapshot the user chose to keep —
--           keywords, alternatives, uncertainty notes. Never raw model
--           responses or prompts.
--   modified_at = the user-edit tiebreak (same-version merges of the
--           same turn: newer modified_at wins — mirrors the correction
--           convention). sequence is the append order inside the
--           conversation.
--
-- Deletes: a CONVERSATION delete cascades tombstones to its turns
-- (server-side cascade, not an FK); a TURN delete is a plain row delete.
-- Account purge and tombstone GC cover both tables.

-- +goose Up

CREATE TABLE interpreter_conversations (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    -- general|school|dorm|bank|hospital|migration|telecom|post.
    scene        VARCHAR(16) NOT NULL DEFAULT 'general',
    -- The user's temporary background note (free text, full desired state).
    context_note TEXT NOT NULL DEFAULT '',
    -- draft|saved|discarded (client-managed; see header).
    status       VARCHAR(16) NOT NULL DEFAULT 'saved',
    started_at   TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ,
    server_version INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    deleted_at   TIMESTAMPTZ
);
CREATE INDEX ix_interpreter_conversations_user ON interpreter_conversations (user_id);
CREATE INDEX ix_interpreter_conversations_user_date ON interpreter_conversations (user_id, started_at);

CREATE TABLE interpreter_turns (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL,
    conversation_id UUID NOT NULL,
    -- counterpart|user.
    speaker         VARCHAR(16) NOT NULL DEFAULT 'counterpart',
    -- ru2zh|zh2ru.
    direction       VARCHAR(16) NOT NULL DEFAULT 'ru2zh',
    -- audio|text (how this turn's input was captured).
    input_method    VARCHAR(16) NOT NULL DEFAULT 'audio',
    sequence        INT NOT NULL DEFAULT 0,
    source_text     TEXT NOT NULL DEFAULT '',
    plain_russian   TEXT NOT NULL DEFAULT '',
    stressed_russian TEXT NOT NULL DEFAULT '',
    chinese_text    TEXT NOT NULL DEFAULT '',
    back_translation TEXT NOT NULL DEFAULT '',
    -- InterpreterTurnDetails JSON string (NULL = none).
    details         JSONB,
    -- User-edit tiebreak on same-version merges (newer wins).
    modified_at     TIMESTAMPTZ,
    server_version  INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX ix_interpreter_turns_user ON interpreter_turns (user_id);
CREATE INDEX ix_interpreter_turns_user_conversation ON interpreter_turns (user_id, conversation_id);

-- +goose Down

DROP TABLE IF EXISTS interpreter_turns;
DROP TABLE IF EXISTS interpreter_conversations;
