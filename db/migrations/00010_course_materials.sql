-- Course-material library: imported documents (PDF/text/image), their
-- page-level extracted text and OCR, user page annotations, and the
-- course assistant's threads/messages — the 资料库 layer.
--
-- Wire entity names: "material" (8), "material_page" (13),
-- "material_annotation" (19), "assistant_thread" (16) and
-- "assistant_message" (17) — all fit the VARCHAR(32) entity_type columns
-- widened by 00008.
--
-- course_id / session_id are PLAIN nullable UUID columns (the term/card
-- convention): rows may arrive before their sources. A COURSE delete
-- DETACHES materials (course_id cleared — 资料转入未归类, they are never
-- cascaded); a SESSION delete clears session_id (the material still
-- belongs to the course). A MATERIAL delete cascades tombstones to its
-- pages and annotations, and an ASSISTANT THREAD delete cascades to its
-- messages — both via the server-side delete handlers, not FKs.
--
-- material_annotations: kind = note | bookmark; the note body rides
-- note_text ('' for bookmarks).
--
-- material_pages: the row id is CLIENT-DETERMINISTIC (derived from
-- materialID + pageNumber on iOS) so two devices extracting the same PDF
-- produce the same rows. extracted_text is the PDF text layer /
-- text-file content; ocr_text is the Vision OCR layer — two separate
-- layers, never merged server-side.
--
-- course_materials.digest is a JSONB column whose wire form is a JSON
-- STRING (the attachmentAnalysis/reviewContent convention).
-- digest_source_hash records the extracted-content hash at digest time —
-- the client's staleness check (资料内容已更新，可重新整理).
--
-- The ORIGINAL FILE (PDF bytes etc.) never rides sync/push: it travels
-- on /v1/materials/{id}/file with the same hash-verified contract as
-- /v1/attachments. content_hash / file_size / mime_type are that
-- upload route's verification contract. Materials borrowed from a
-- classroom image carry source_attachment_id and NO file of their own.
--
-- assistant_messages: role = user | assistant. The answer's provenance
-- rides citations (JSONB; wire form a JSON string) — the client resolves
-- every citation to a locally-verifiable row before displaying it.
-- scope_material_id / scope_session_id / scope_page_number record the
-- question's scope (absent = course-wide).
--
-- 00010 also adds the material source columns to the three learning
-- tables (glossary_terms / study_cards / study_tasks): terms/cards/tasks
-- created from a material digest carry source_material_id +
-- source_material_page so one tap jumps back into the reader.

-- +goose Up

CREATE TABLE course_materials (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    course_id           UUID,
    session_id          UUID,
    -- Opaque occurrence key ("scheduleUUID:YYYY-MM-DD"); the server never
    -- parses it. '' = not linked to a class.
    occurrence_key      VARCHAR(128) NOT NULL DEFAULT '',
    title               TEXT NOT NULL DEFAULT '',
    original_file_name  TEXT NOT NULL DEFAULT '',
    mime_type           VARCHAR(64) NOT NULL DEFAULT '',
    -- lecture|homework|lab|reading|exam|other.
    kind                VARCHAR(16) NOT NULL DEFAULT 'other',
    -- pdf|text|markdown|image|other.
    format              VARCHAR(16) NOT NULL DEFAULT 'other',
    -- The upload route's verification contract.
    file_size           BIGINT NOT NULL DEFAULT 0,
    content_hash        VARCHAR(64) NOT NULL DEFAULT '',
    -- Real page count for paged materials; 1 for text/image; 0 unknown.
    page_count          INT NOT NULL DEFAULT 0,
    -- Set when the material borrows a classroom attachment's files.
    source_attachment_id UUID,
    -- pending|extracting(local-only)|completed|partial|failed|unsupported.
    extraction_status   VARCHAR(16) NOT NULL DEFAULT 'pending',
    -- pending|analyzing(local-only)|completed|partial|failed.
    digest_status       VARCHAR(16) NOT NULL DEFAULT 'pending',
    -- MaterialDigestResult JSON (NULL = none).
    digest              JSONB,
    digest_model        TEXT NOT NULL DEFAULT '',
    digest_generated_at TIMESTAMPTZ,
    digest_source_hash  VARCHAR(64) NOT NULL DEFAULT '',
    -- Reading position (synced): 0 = never opened.
    last_read_page      INT NOT NULL DEFAULT 0,
    last_opened_at      TIMESTAMPTZ,
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_course_materials_user ON course_materials (user_id);
CREATE INDEX ix_course_materials_user_course ON course_materials (user_id, course_id);
CREATE INDEX ix_course_materials_user_session ON course_materials (user_id, session_id);

CREATE TABLE material_pages (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    material_id         UUID NOT NULL,
    -- 1-based page number.
    page_number         INT NOT NULL DEFAULT 1,
    -- PDF text layer / text-file content ('' = no text layer).
    extracted_text      TEXT NOT NULL DEFAULT '',
    -- Vision OCR text ('' = not run / none found).
    ocr_text            TEXT NOT NULL DEFAULT '',
    -- none|pending|running(local-only)|done|failed.
    ocr_status          VARCHAR(16) NOT NULL DEFAULT 'none',
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_material_pages_user ON material_pages (user_id);
CREATE INDEX ix_material_pages_user_material ON material_pages (user_id, material_id);

CREATE TABLE material_annotations (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    material_id         UUID NOT NULL,
    page_number         INT NOT NULL DEFAULT 1,
    -- note|bookmark.
    kind                VARCHAR(16) NOT NULL DEFAULT 'note',
    -- Note body ('' for bookmarks).
    note_text           TEXT NOT NULL DEFAULT '',
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_material_annotations_user ON material_annotations (user_id);
CREATE INDEX ix_material_annotations_user_material ON material_annotations (user_id, material_id);

CREATE TABLE assistant_threads (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    course_id           UUID,
    title               TEXT NOT NULL DEFAULT '',
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_assistant_threads_user ON assistant_threads (user_id);
CREATE INDEX ix_assistant_threads_user_course ON assistant_threads (user_id, course_id);

CREATE TABLE assistant_messages (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    thread_id           UUID NOT NULL,
    -- user|assistant.
    role                VARCHAR(16) NOT NULL DEFAULT 'user',
    text                TEXT NOT NULL DEFAULT '',
    -- AssistantMessageCitation list JSON (NULL = none).
    citations           JSONB,
    -- Question scope (NULL = course-wide).
    scope_material_id   UUID,
    scope_session_id    UUID,
    scope_page_number   INT NOT NULL DEFAULT 0,
    server_version      INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX ix_assistant_messages_user ON assistant_messages (user_id);
CREATE INDEX ix_assistant_messages_user_thread ON assistant_messages (user_id, thread_id);

-- Material provenance on the learning tables: terms/cards/tasks created
-- from a material digest jump back into the reader.
ALTER TABLE glossary_terms ADD COLUMN source_material_id UUID;
ALTER TABLE glossary_terms ADD COLUMN source_material_page INT NOT NULL DEFAULT 0;
ALTER TABLE study_cards ADD COLUMN source_material_id UUID;
ALTER TABLE study_cards ADD COLUMN source_material_page INT NOT NULL DEFAULT 0;
ALTER TABLE study_tasks ADD COLUMN source_material_id UUID;
ALTER TABLE study_tasks ADD COLUMN source_material_page INT NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE study_tasks DROP COLUMN IF EXISTS source_material_page;
ALTER TABLE study_tasks DROP COLUMN IF EXISTS source_material_id;
ALTER TABLE study_cards DROP COLUMN IF EXISTS source_material_page;
ALTER TABLE study_cards DROP COLUMN IF EXISTS source_material_id;
ALTER TABLE glossary_terms DROP COLUMN IF EXISTS source_material_page;
ALTER TABLE glossary_terms DROP COLUMN IF EXISTS source_material_id;
DROP TABLE IF EXISTS assistant_messages;
DROP TABLE IF EXISTS assistant_threads;
DROP TABLE IF EXISTS material_annotations;
DROP TABLE IF EXISTS material_pages;
DROP TABLE IF EXISTS course_materials;
