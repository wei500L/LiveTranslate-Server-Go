-- Link materials: a saved web URL as course material (shared from
-- Safari / chat apps through the Share Extension into the inbox, then
-- confirmed by the user). The SAME course_materials rows carry links —
-- never a second material entity.
--
-- format gains the value 'link' (no DB constraint existed before and
-- none is added — the Go-side allowlist extends). A link material has
-- NO FILE: file_size stays 0, content_hash stays '' (the file routes
-- refuse links, mirroring the borrow case), page_count stays 0.
--
-- source_url   = the saved URL (insert-only identity — the same
--                 convention as content_hash: a link is the URL it was
--                 shared as; only the TITLE and shared_text are
--                 user-editable).
-- shared_text  = text the sender shared alongside the URL (selected
--                 passage / share note). Full desired state on update
--                 (empty string clears, absent payload keeps stored —
--                 the title convention inverted: the client always
--                 sends it, an ABSENT field is the "keep" signal).
--
-- No new entity kind, table, index, route or cascade: deletes, GC
-- (course_materials is already in the tombstone-GC list), account
-- purge and the change-log plumbing are unchanged. The wire contract
-- gains 2 optional payload fields (materialSourceURL /
-- materialSharedText) — old clients keep working because they never
-- send them; new clients require the new server
-- (DisallowUnknownFields, see 00011).

-- +goose Up

ALTER TABLE course_materials
    ADD COLUMN source_url  TEXT NOT NULL DEFAULT '',
    ADD COLUMN shared_text TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE course_materials
    DROP COLUMN shared_text,
    DROP COLUMN source_url;
