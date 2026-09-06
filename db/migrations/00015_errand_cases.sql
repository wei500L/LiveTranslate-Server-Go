-- Errand cases (办事事项): a continuing real-world errand — the dorm
-- registration, the bank card, the clinic visit, the visa paperwork —
-- organized as an executable checklist with appointments, deadlines and
-- follow-ups. Round 18 closes the loop the interpreter (00014) and the
-- document assistant (round 16) opened: the user can now TRACK an errand
-- across conversations and files instead of only living inside one.
--
-- Wire entity names: "errand_case" (11 chars) and "errand_case_item"
-- (16 chars) — both fit the VARCHAR(32) entity_type columns widened by
-- migration 00008. "errand" is the vocabulary 00014 already established
-- for 办事 (the interpreter scene selector is the "errand scene").
--
-- Lifecycle: like interpreter conversations, the client keeps UNCONFIRMED
-- work as a DEVICE-LOCAL draft that never reaches the wire; only cases
-- the user explicitly saved sync. status is client-managed state the
-- server stores without interpreting it ('draft' stays wire-legal for
-- forward tolerance, same as interpreter 'draft'/'discarded').
--
-- errand_cases:
--   scene = general|school|dorm|bank|hospital|migration|telecom|post
--           (the SAME allowlist as interpreter_conversations.scene —
--           the errand scene selector, reused verbatim).
--   status = draft|preparing|scheduled|waitingForResult|needsFollowUp|
--            completed|cancelled|archived (client-computed; the server
--            validates the enum, never derives it).
--   purpose = the user-confirmed short purpose (full desired state,
--             '' clears — the context_note convention).
--   timezone = IANA id the case's wall-clock times anchor to (validated
--             via time.LoadLocation, '' = unset).
--   expected_result_at = the 预计结果时间 (waitingForResult semantics —
--             NOT a dueDate: appointment/deadline/follow-up times live
--             on their own items, never merged here).
--   has_local_sources = content-free flag: the SAVING device has local
--             source links (conversations / interpreter documents).
--             The links themselves — file names, page numbers, snippets,
--             document ids — NEVER ride the wire (round 17 boundary).
--
-- errand_case_items: one checklist row — requiredDocument, action,
--   question, payment, appointment, deadline or followUp. Items are
--   independent rows with independent server_versions so checking one
--   item on device A never overwrites a different item on device B.
--   status = unconfirmed|pending|done|skipped; origin = manual|ai
--   (ai rows are user-confirmed AI candidates — the model never writes
--   rows by itself). due_at is the user-CONFIRMED instant; date_text
--   preserves the source wording ("до пятницы"), is_relative_date marks
--   relative conversions, date_uncertain marks unresolved ambiguity.
--   fee_text/fee_amount/fee_currency only carry values the source
--   actually stated or the user typed (no conversion, no guessing).
--   modified_at is the user-edit tiebreak (same-version merges resolve
--   newer-wins, the correction/turn convention).
--
-- Deletes: a CASE delete cascades tombstones to its items (server-side
-- cascade, not an FK — the interpreter convention); an ITEM delete is a
-- plain row delete. Account purge and tombstone GC cover both tables.

-- +goose Up

CREATE TABLE errand_cases (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    -- general|school|dorm|bank|hospital|migration|telecom|post (the
    -- interpreter scene allowlist, reused verbatim).
    scene        VARCHAR(16) NOT NULL DEFAULT 'general',
    -- draft|preparing|scheduled|waitingForResult|needsFollowUp|
    -- completed|cancelled|archived (client-managed; see header).
    status       VARCHAR(32) NOT NULL DEFAULT 'preparing',
    -- The user-confirmed short purpose ('' clears, full desired state).
    purpose      TEXT NOT NULL DEFAULT '',
    user_note    TEXT NOT NULL DEFAULT '',
    -- IANA timezone id ('' = unset).
    timezone     VARCHAR(64) NOT NULL DEFAULT '',
    location     TEXT NOT NULL DEFAULT '',
    contact      TEXT NOT NULL DEFAULT '',
    -- 预计结果时间 (waitingForResult semantics; appointment/deadline/
    -- follow-up times live on their own items).
    expected_result_at TIMESTAMPTZ,
    -- User-pinned (a home-card display condition).
    pinned       BOOLEAN NOT NULL DEFAULT FALSE,
    -- Content-free flag: the saving device holds local source links.
    -- File names, page numbers, snippets, document ids never sync.
    has_local_sources BOOLEAN NOT NULL DEFAULT FALSE,
    server_version INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    deleted_at   TIMESTAMPTZ
);
CREATE INDEX ix_errand_cases_user ON errand_cases (user_id);

CREATE TABLE errand_case_items (
    id          UUID PRIMARY KEY,
    user_id     UUID NOT NULL,
    case_id     UUID NOT NULL,
    -- The item's short title (rides the shared wire "title" key — the
    -- plan-item convention).
    title       TEXT NOT NULL DEFAULT '',
    -- requiredDocument|action|question|payment|appointment|deadline|followUp.
    kind        VARCHAR(32) NOT NULL DEFAULT 'action',
    -- unconfirmed|pending|done|skipped.
    status      VARCHAR(16) NOT NULL DEFAULT 'pending',
    -- Sort order inside the case.
    sequence    INT NOT NULL DEFAULT 0,
    -- Optional explanation (材料说明如 原件/复印件/翻译件/公证件).
    detail      TEXT NOT NULL DEFAULT '',
    -- The user-CONFIRMED instant (nil = none confirmed).
    due_at      TIMESTAMPTZ,
    -- Source wording of the date ("до пятницы"), preserved verbatim.
    date_text   TEXT NOT NULL DEFAULT '',
    is_relative_date BOOLEAN NOT NULL DEFAULT FALSE,
    date_uncertain    BOOLEAN NOT NULL DEFAULT FALSE,
    -- manual|ai (ai = a user-confirmed AI candidate).
    origin      VARCHAR(16) NOT NULL DEFAULT 'manual',
    confirmed   BOOLEAN NOT NULL DEFAULT FALSE,
    -- Fee as the source stated it or the user typed (no conversion).
    fee_text     TEXT NOT NULL DEFAULT '',
    fee_amount   DOUBLE PRECISION,
    fee_currency VARCHAR(8) NOT NULL DEFAULT '',
    -- User-edit tiebreak on same-version merges (newer wins).
    modified_at  TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    server_version INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    deleted_at   TIMESTAMPTZ
);
CREATE INDEX ix_errand_case_items_user ON errand_case_items (user_id);
CREATE INDEX ix_errand_case_items_user_case ON errand_case_items (user_id, case_id);

-- +goose Down

DROP TABLE IF EXISTS errand_case_items;
DROP TABLE IF EXISTS errand_cases;
