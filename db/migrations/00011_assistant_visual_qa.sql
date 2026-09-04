-- Assistant visual Q&A: the visual layer rides the SAME
-- assistant_messages rows (never a second chat system).
--
-- mode              = text | visual — whether the turn carried evidence
--                     images. Defaults to 'text' so every pre-00011 row
--                     (and every text ask after it) stays valid.
-- visual_evidence   = JSONB; wire form a JSON STRING (the citations /
--                     digest convention). The evidence SNAPSHOT: stable
--                     source ids, page numbers and NORMALIZED crop
--                     rects — metadata only. Image bytes, base64, file
--                     paths, request bodies and raw responses never
--                     ride the wire.
-- answer            = JSONB; wire form a JSON string. The loosely
--                     structured visual answer payload (answer / steps /
--                     citations / visibleText / formulas /
--                     uncertainties / suggestedActions).
-- model_name        = the model that produced the answer (provenance).
--
-- Merge semantics (mirroring citations): an absent or EMPTY payload
-- field KEEPS the stored value — a multi-device merge must never blank a
-- complete answer with "". A deleted source (attachment/material) never
-- touches the message: the evidence row stays, the client renders
-- 原图片已不存在 instead of substituting another image.
--
-- No new entity kind, table, index, route or cascade: deletes, GC
-- (tombstone cleanup already lists assistant_messages), account purge
-- and the file endpoints are all unchanged. The wire contract gains 4
-- optional payload fields — old clients keep working because they never
-- send them; new clients require the new server (DisallowUnknownFields).

-- +goose Up

ALTER TABLE assistant_messages
    ADD COLUMN mode           VARCHAR(16) NOT NULL DEFAULT 'text',
    ADD COLUMN visual_evidence JSONB,
    ADD COLUMN answer         JSONB,
    ADD COLUMN model_name     TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE assistant_messages
    DROP COLUMN model_name,
    DROP COLUMN answer,
    DROP COLUMN visual_evidence,
    DROP COLUMN mode;
