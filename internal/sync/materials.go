// Course-material sync entities: imported documents (PDF/text/image),
// page-level extracted text and OCR, user page annotations, and the
// course assistant's threads/messages.
//
// Semantics follow the established entity conventions:
//   - delete: phantom tombstone / idempotent re-delete / tombstone +
//     cascade (material → pages+annotations; thread → messages), with the
//     material's ORIGINAL FILE reaped post-commit through the SAME
//     attachment store instance (paths are keyed by entity UUID, so a
//     material's directory can never collide with an attachment's);
//   - upsert: insert@v1 / base==ver merge / base<ver conflict / base>ver
//     rejected; deleted rows conflict (delete-wins);
//   - identity fields (content_hash/file_size/page_count/mime) are
//     immutable after insert (the upload route's verification contract);
//   - a COURSE delete DETACHES materials (course_id cleared — they
//     transfer to 未归类, never tombstoned); a SESSION delete clears
//     session_id (handled in cascadeDeleteChildren).
package sync

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/internal/store"
)

// --- Row readers ----------------------------------------------------------------

type materialRow struct {
	id               uuid.UUID
	userID           uuid.UUID
	courseID         *uuid.UUID
	sessionID        *uuid.UUID
	occurrenceKey    string
	title            string
	fileName         string
	mimeType         string
	kind             string
	format           string
	fileSize         int64
	contentHash      string
	pageCount        int
	sourceAttachment *uuid.UUID
	extractionStatus string
	digestStatus     string
	digest           *string // JSONB as string
	digestModel      string
	digestGenerated  *time.Time
	digestSourceHash string
	lastReadPage     int
	lastOpenedAt     *time.Time
	serverVer        int
	createdAt        time.Time
	updatedAt        time.Time
	deletedAt        *time.Time
}

func fetchMaterial(ctx context.Context, q store.Q, userID, id uuid.UUID) (*materialRow, error) {
	m := &materialRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, course_id, session_id, occurrence_key,
		       title, original_file_name, mime_type, kind, format,
		       file_size, content_hash, page_count, source_attachment_id,
		       extraction_status, digest_status, digest::text,
		       digest_model, digest_generated_at, digest_source_hash,
		       last_read_page, last_opened_at,
		       server_version, created_at, updated_at, deleted_at
		FROM course_materials WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&m.id, &m.userID, &m.courseID, &m.sessionID, &m.occurrenceKey,
		&m.title, &m.fileName, &m.mimeType, &m.kind, &m.format,
		&m.fileSize, &m.contentHash, &m.pageCount, &m.sourceAttachment,
		&m.extractionStatus, &m.digestStatus, &m.digest,
		&m.digestModel, &m.digestGenerated, &m.digestSourceHash,
		&m.lastReadPage, &m.lastOpenedAt,
		&m.serverVer, &m.createdAt, &m.updatedAt, &m.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func materialRowView(m *materialRow) learningRowView {
	if m == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: m.serverVer, updatedAt: m.updatedAt}
}

func materialRecordJSON(m *materialRow) json.RawMessage {
	var digest *string
	if m.digest != nil && *m.digest != "" {
		digest = m.digest
	}
	b, _ := json.Marshal(courseMaterialRecord{
		EntityType: EntityMaterial, ID: m.id.String(),
		Title:    m.title,
		CourseID: optUUIDString(m.courseID), SessionID: optUUIDString(m.sessionID),
		OccurrenceKey: m.occurrenceKey,
		SourceAttach:  optUUIDString(m.sourceAttachment),
		Kind:          m.kind, MimeType: m.mimeType, FileName: m.fileName,
		Format: m.format, FileSize: m.fileSize, ContentHash: m.contentHash,
		PageCount: m.pageCount, Extraction: m.extractionStatus,
		DigestStatus: m.digestStatus, Digest: digest,
		DigestModel: m.digestModel, DigestAt: m.digestGenerated,
		DigestSrcHash: m.digestSourceHash,
		LastReadPage:  m.lastReadPage, LastOpenedAt: m.lastOpenedAt,
		ServerVersion: m.serverVer, Deleted: m.deletedAt != nil,
	})
	return b
}

type materialPageRow struct {
	id            uuid.UUID
	userID        uuid.UUID
	materialID    uuid.UUID
	pageNumber    int
	extractedText string
	ocrText       string
	ocrStatus     string
	serverVer     int
	createdAt     time.Time
	updatedAt     time.Time
	deletedAt     *time.Time
}

func fetchMaterialPage(ctx context.Context, q store.Q, userID, id uuid.UUID) (*materialPageRow, error) {
	p := &materialPageRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, material_id, page_number,
		       extracted_text, ocr_text, ocr_status,
		       server_version, created_at, updated_at, deleted_at
		FROM material_pages WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&p.id, &p.userID, &p.materialID, &p.pageNumber,
		&p.extractedText, &p.ocrText, &p.ocrStatus,
		&p.serverVer, &p.createdAt, &p.updatedAt, &p.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func materialPageRowView(p *materialPageRow) learningRowView {
	if p == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: p.serverVer, updatedAt: p.updatedAt}
}

func materialPageRecordJSON(p *materialPageRow) json.RawMessage {
	b, _ := json.Marshal(materialPageRecord{
		EntityType: EntityMaterialPage, ID: p.id.String(),
		MaterialID: p.materialID.String(), PageNumber: p.pageNumber,
		ExtractedText: p.extractedText, OcrText: p.ocrText, OcrStatus: p.ocrStatus,
		ServerVersion: p.serverVer, Deleted: p.deletedAt != nil,
	})
	return b
}

type materialAnnotationRow struct {
	id         uuid.UUID
	userID     uuid.UUID
	materialID uuid.UUID
	pageNumber int
	kind       string
	noteText   string
	serverVer  int
	createdAt  time.Time
	updatedAt  time.Time
	deletedAt  *time.Time
}

func fetchMaterialAnnotation(ctx context.Context, q store.Q, userID, id uuid.UUID) (*materialAnnotationRow, error) {
	a := &materialAnnotationRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, material_id, page_number, kind, note_text,
		       server_version, created_at, updated_at, deleted_at
		FROM material_annotations WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&a.id, &a.userID, &a.materialID, &a.pageNumber, &a.kind, &a.noteText,
		&a.serverVer, &a.createdAt, &a.updatedAt, &a.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func materialAnnotationRowView(a *materialAnnotationRow) learningRowView {
	if a == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: a.serverVer, updatedAt: a.updatedAt}
}

func materialAnnotationRecordJSON(a *materialAnnotationRow) json.RawMessage {
	b, _ := json.Marshal(materialAnnotationRecord{
		EntityType: EntityMaterialAnnotation, ID: a.id.String(),
		MaterialID: a.materialID.String(), PageNumber: a.pageNumber,
		Kind: a.kind, NoteText: a.noteText,
		ServerVersion: a.serverVer, Deleted: a.deletedAt != nil,
	})
	return b
}

type assistantThreadRow struct {
	id        uuid.UUID
	userID    uuid.UUID
	courseID  *uuid.UUID
	title     string
	serverVer int
	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time
}

func fetchAssistantThread(ctx context.Context, q store.Q, userID, id uuid.UUID) (*assistantThreadRow, error) {
	t := &assistantThreadRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, course_id, title,
		       server_version, created_at, updated_at, deleted_at
		FROM assistant_threads WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&t.id, &t.userID, &t.courseID, &t.title,
		&t.serverVer, &t.createdAt, &t.updatedAt, &t.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func assistantThreadRowView(t *assistantThreadRow) learningRowView {
	if t == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: t.serverVer, updatedAt: t.updatedAt}
}

func assistantThreadRecordJSON(t *assistantThreadRow) json.RawMessage {
	b, _ := json.Marshal(assistantThreadRecord{
		EntityType: EntityAssistantThread, ID: t.id.String(),
		Title: t.title, CourseID: optUUIDString(t.courseID),
		ServerVersion: t.serverVer, Deleted: t.deletedAt != nil,
	})
	return b
}

type assistantMessageRow struct {
	id           uuid.UUID
	userID       uuid.UUID
	threadID     uuid.UUID
	role         string
	text         string
	citations    *string // JSONB as string
	scopeMatID   *uuid.UUID
	scopeSessID  *uuid.UUID
	scopePageNum int
	mode         string  // text | visual (00011; 'text' for pre-00011 rows)
	visualEvid   *string // JSONB as string (00011)
	answer       *string // JSONB as string (00011)
	modelName    string  // 00011
	serverVer    int
	createdAt    time.Time
	updatedAt    time.Time
	deletedAt    *time.Time
}

func fetchAssistantMessage(ctx context.Context, q store.Q, userID, id uuid.UUID) (*assistantMessageRow, error) {
	m := &assistantMessageRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, thread_id, role, text, citations::text,
		       scope_material_id, scope_session_id, scope_page_number,
		       mode, visual_evidence::text, answer::text, model_name,
		       server_version, created_at, updated_at, deleted_at
		FROM assistant_messages WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&m.id, &m.userID, &m.threadID, &m.role, &m.text, &m.citations,
		&m.scopeMatID, &m.scopeSessID, &m.scopePageNum,
		&m.mode, &m.visualEvid, &m.answer, &m.modelName,
		&m.serverVer, &m.createdAt, &m.updatedAt, &m.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func assistantMessageRowView(m *assistantMessageRow) learningRowView {
	if m == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: m.serverVer, updatedAt: m.updatedAt}
}

func assistantMessageRecordJSON(m *assistantMessageRow) json.RawMessage {
	var citations *string
	if m.citations != nil && *m.citations != "" {
		citations = m.citations
	}
	var mode *string
	if m.mode != "" && m.mode != "text" {
		mode = &m.mode
	}
	var evidence *string
	if m.visualEvid != nil && *m.visualEvid != "" {
		evidence = m.visualEvid
	}
	var answer *string
	if m.answer != nil && *m.answer != "" {
		answer = m.answer
	}
	var model *string
	if m.modelName != "" {
		model = &m.modelName
	}
	b, _ := json.Marshal(assistantMessageRecord{
		EntityType: EntityAssistantMessage, ID: m.id.String(),
		ThreadID: m.threadID.String(), Role: m.role, Text: m.text,
		Citations:       citations,
		ScopeMaterial:   optUUIDString(m.scopeMatID),
		ScopeSession:    optUUIDString(m.scopeSessID),
		ScopePageNumber: m.scopePageNum,
		Mode:            mode,
		VisualEvidence:  evidence,
		Answer:          answer,
		ModelName:       model,
		ServerVersion:   m.serverVer, Deleted: m.deletedAt != nil,
	})
	return b
}

// --- Apply: material --------------------------------------------------------------

var validMaterialKinds = map[string]bool{
	"lecture": true, "homework": true, "lab": true,
	"reading": true, "exam": true, "other": true,
}

var validMaterialFormats = map[string]bool{
	"pdf": true, "text": true, "markdown": true, "image": true, "other": true,
}

var validMaterialExtraction = map[string]bool{
	"pending": true, "completed": true, "partial": true, "failed": true, "unsupported": true,
}

var validMaterialDigest = map[string]bool{
	"pending": true, "completed": true, "partial": true, "failed": true,
}

func (s *Service) applyMaterial(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchMaterial(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			// Children follow the material into the tombstone (pages +
			// annotations); the file is reaped post-commit.
			if err := s.cascadeDeleteMaterialChildren(ctx, q, userID, item.EntityID); err != nil {
				return nil, err
			}
		}
		res, err := s.applyLearningDelete(ctx, q, userID, item, EntityMaterial, "course_materials",
			obj != nil, obj != nil && obj.deletedAt != nil, materialRowView(obj))
		if err != nil {
			return nil, err
		}
		if res.Status == "accepted" && s.attachments != nil && obj != nil && obj.sourceAttachment == nil {
			// File cleanup runs only after the enclosing push transaction
			// commits (see withPostCommit); a rollback discards the hook.
			// Materials borrowing a classroom attachment's files have no
			// file of their own — nothing to reap.
			deferFileCleanup(ctx, func() {
				if err := s.attachments.DeleteFiles(userID, item.EntityID); err != nil {
					slog.Error("material file cleanup failed", "user_id", userID, "material_id", item.EntityID, "err", err.Error())
				}
			})
		}
		return res, nil
	}

	// Validate the enum-ish fields (unknown values are schema rejects —
	// the client maps its local-only states before pushing, so an unknown
	// value here is a broken client, not a state to store).
	kind, format := "other", "other"
	if p.MaterialKind != nil && *p.MaterialKind != "" {
		kind = *p.MaterialKind
	}
	if p.MaterialFormat != nil && *p.MaterialFormat != "" {
		format = *p.MaterialFormat
	}
	if !validMaterialKinds[kind] || !validMaterialFormats[format] {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	extraction, digestStatus := "pending", "pending"
	if p.MaterialExtraction != nil && *p.MaterialExtraction != "" {
		extraction = *p.MaterialExtraction
	}
	if p.MaterialDigestStatus != nil && *p.MaterialDigestStatus != "" {
		digestStatus = *p.MaterialDigestStatus
	}
	if !validMaterialExtraction[extraction] || !validMaterialDigest[digestStatus] {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// Effective values for insert; merge decisions for update. Identity
	// fields (hash/size/pageCount/mime) apply on INSERT only.
	var fileSize int64
	var pageCount int
	mime, fileName, hash := "", "", ""
	if p.MaterialFileSize != nil {
		fileSize = *p.MaterialFileSize
	}
	if p.MaterialPageCount != nil {
		pageCount = *p.MaterialPageCount
	}
	if p.MaterialMime != nil {
		mime = *p.MaterialMime
	}
	if p.MaterialFileName != nil {
		fileName = *p.MaterialFileName
	}
	if p.MaterialHash != nil {
		hash = *p.MaterialHash
	}
	title := ""
	if p.Title != nil {
		title = *p.Title
	}
	digestModel, digestSrcHash := "", ""
	var digestAt *time.Time
	if p.MaterialDigestModel != nil {
		digestModel = *p.MaterialDigestModel
	}
	if p.MaterialDigestSrcHash != nil {
		digestSrcHash = *p.MaterialDigestSrcHash
	}
	if p.MaterialDigestAt != nil {
		digestAt = p.MaterialDigestAt
	}
	var digest *string
	if p.MaterialDigest != nil && *p.MaterialDigest != "" {
		digest = p.MaterialDigest
	}
	var lastReadPage int
	var lastOpenedAt *time.Time
	if p.MaterialLastReadPage != nil {
		lastReadPage = *p.MaterialLastReadPage
	}
	if p.MaterialLastOpenedAt != nil {
		lastOpenedAt = p.MaterialLastOpenedAt
	}
	courseID := refOrNil(p.CourseID)
	sessionID := refOrNil(p.SessionID)
	sourceAttachment := refOrNil(p.SourceAttachmentID)
	occurrenceKey := ""
	if p.ScheduleOccurrenceKey != nil {
		occurrenceKey = *p.ScheduleOccurrenceKey
	}

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO course_materials
				(id, user_id, course_id, session_id, occurrence_key,
				 title, original_file_name, mime_type, kind, format,
				 file_size, content_hash, page_count, source_attachment_id,
				 extraction_status, digest_status, digest,
				 digest_model, digest_generated_at, digest_source_hash,
				 last_read_page, last_opened_at,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5,
			        $6, $7, $8, $9, $10,
			        $11, $12, $13, $14,
			        $15, $16, $17::jsonb,
			        $18, $19, $20,
			        $21, $22,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, courseID, sessionID, occurrenceKey,
			title, fileName, mime, kind, format,
			fileSize, hash, pageCount, sourceAttachment,
			extraction, digestStatus, digest,
			digestModel, digestAt, digestSrcHash,
			lastReadPage, lastOpenedAt,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityMaterial, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, materialRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, materialRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// Merge: present pointers win; absent keeps the stored value. Reading
	// position and digest fields ride full desired state on every upsert
	// (the iOS payload always sends them).
	if title == "" {
		title = obj.title
	}
	if digest == nil {
		digest = obj.digest
	}
	if digestModel == "" {
		digestModel = obj.digestModel
	}
	if digestSrcHash == "" {
		digestSrcHash = obj.digestSourceHash
	}
	if digestAt == nil {
		digestAt = obj.digestGenerated
	}
	if lastOpenedAt == nil {
		lastOpenedAt = obj.lastOpenedAt
	}
	courseID = mergeRef(obj.courseID, p.CourseID)
	sessionID = mergeRef(obj.sessionID, p.SessionID)
	sourceAttachment = mergeRef(obj.sourceAttachment, p.SourceAttachmentID)
	// occurrence_key rides FULL desired state ('' = no link); absent on
	// the wire keeps the stored value only for inserts (the default).

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE course_materials
		SET course_id = $3, session_id = $4, occurrence_key = $5,
		    title = $6, kind = $7, format = $8,
		    extraction_status = $9, digest_status = $10, digest = $11::jsonb,
		    digest_model = $12, digest_generated_at = $13, digest_source_hash = $14,
		    last_read_page = $15, last_opened_at = $16,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, courseID, sessionID, occurrenceKey,
		title, kind, format,
		extraction, digestStatus, digest,
		digestModel, digestAt, digestSrcHash,
		lastReadPage, lastOpenedAt,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityMaterial, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: material page -----------------------------------------------------------

var validMaterialOCRStatus = map[string]bool{
	"none": true, "pending": true, "done": true, "failed": true,
}

func (s *Service) applyMaterialPage(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchMaterialPage(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityMaterialPage, "material_pages",
			obj != nil, obj != nil && obj.deletedAt != nil, materialPageRowView(obj))
	}

	// The parent material and page number are required (a page row without
	// its material is meaningless server-side).
	materialID := p.MaterialID
	if materialID == nil || *materialID == uuid.Nil || p.MaterialPageNumber == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	pageNumber := *p.MaterialPageNumber
	if pageNumber < 1 {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	ocrStatus := "none"
	if p.MaterialPageOCRState != nil && *p.MaterialPageOCRState != "" {
		ocrStatus = *p.MaterialPageOCRState
	}
	if !validMaterialOCRStatus[ocrStatus] {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	extractedText, ocrText := "", ""
	if p.MaterialPageText != nil {
		extractedText = *p.MaterialPageText
	}
	if p.MaterialPageOCR != nil {
		ocrText = *p.MaterialPageOCR
	}

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO material_pages
				(id, user_id, material_id, page_number,
				 extracted_text, ocr_text, ocr_status,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, *materialID, pageNumber,
			extractedText, ocrText, ocrStatus,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityMaterialPage, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, materialPageRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, materialPageRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// Merge: present pointers win; absent keeps (a page-only-OCR push
	// leaves the text layer untouched and vice versa).
	if extractedText == "" {
		extractedText = obj.extractedText
	}
	if ocrText == "" {
		ocrText = obj.ocrText
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE material_pages
		SET page_number = $3, extracted_text = $4, ocr_text = $5, ocr_status = $6,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, pageNumber, extractedText, ocrText, ocrStatus,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityMaterialPage, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: material annotation ------------------------------------------------------

func (s *Service) applyMaterialAnnotation(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchMaterialAnnotation(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityMaterialAnnotation, "material_annotations",
			obj != nil, obj != nil && obj.deletedAt != nil, materialAnnotationRowView(obj))
	}

	materialID := p.MaterialID
	kind := "note"
	if p.MaterialAnnotationKind != nil && *p.MaterialAnnotationKind != "" {
		kind = *p.MaterialAnnotationKind
	}
	if materialID == nil || *materialID == uuid.Nil || p.MaterialPageNumber == nil ||
		(kind != "note" && kind != "bookmark") {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	pageNumber := *p.MaterialPageNumber
	noteText := ""
	if p.NoteText != nil {
		noteText = *p.NoteText
	}

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO material_annotations
				(id, user_id, material_id, page_number, kind, note_text,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, *materialID, pageNumber, kind, noteText,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityMaterialAnnotation, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, materialAnnotationRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, materialAnnotationRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if noteText == "" && obj.kind == "note" {
		// An empty note body on a NOTE row keeps the stored text (a blank
		// push must not silently wipe the user's note); bookmarks have no
		// body to keep.
		noteText = obj.noteText
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE material_annotations
		SET material_id = $3, page_number = $4, kind = $5, note_text = $6,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, *materialID, pageNumber, kind, noteText,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityMaterialAnnotation, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: assistant thread / message ------------------------------------------------

func (s *Service) applyAssistantThread(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchAssistantThread(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			// Messages follow the thread into the tombstone.
			if err := s.cascadeDeleteAssistantMessages(ctx, q, userID, item.EntityID); err != nil {
				return nil, err
			}
		}
		return s.applyLearningDelete(ctx, q, userID, item, EntityAssistantThread, "assistant_threads",
			obj != nil, obj != nil && obj.deletedAt != nil, assistantThreadRowView(obj))
	}

	if p.Title == nil || *p.Title == "" {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	title := *p.Title
	courseID := refOrNil(p.CourseID)

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO assistant_threads
				(id, user_id, course_id, title, server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, courseID, title,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityAssistantThread, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, assistantThreadRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, assistantThreadRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	courseID = mergeRef(obj.courseID, p.CourseID)

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE assistant_threads
		SET course_id = $3, title = $4,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, courseID, title,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityAssistantThread, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) applyAssistantMessage(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchAssistantMessage(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityAssistantMessage, "assistant_messages",
			obj != nil, obj != nil && obj.deletedAt != nil, assistantMessageRowView(obj))
	}

	threadID := p.ThreadID
	role := "user"
	if p.AssistantRole != nil && *p.AssistantRole != "" {
		role = *p.AssistantRole
	}
	if threadID == nil || *threadID == uuid.Nil || (role != "user" && role != "assistant") {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	text := ""
	if p.AssistantText != nil {
		text = *p.AssistantText
	}
	var citations *string
	if p.AssistantCitations != nil && *p.AssistantCitations != "" {
		citations = p.AssistantCitations
	}
	// Visual Q&A (00011): mode must be a known value; the evidence and
	// answer payloads must be valid JSON (they land in JSONB columns —
	// invalid JSON would abort the whole batch's transaction).
	mode := "text"
	if p.AssistantMode != nil && *p.AssistantMode != "" {
		mode = *p.AssistantMode
	}
	if mode != "text" && mode != "visual" {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	visualEvidence := validJSONPayload(p.AssistantEvidence)
	if p.AssistantEvidence != nil && *p.AssistantEvidence != "" && visualEvidence == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	answer := validJSONPayload(p.AssistantAnswer)
	if p.AssistantAnswer != nil && *p.AssistantAnswer != "" && answer == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	modelName := ""
	if p.AssistantModel != nil {
		modelName = *p.AssistantModel
	}
	scopeMaterial := refOrNil(p.MaterialID)
	scopeSession := refOrNil(p.SessionID)
	scopePage := 0
	if p.MaterialPageNumber != nil {
		scopePage = *p.MaterialPageNumber
	}

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO assistant_messages
				(id, user_id, thread_id, role, text, citations,
				 scope_material_id, scope_session_id, scope_page_number,
				 mode, visual_evidence, answer, model_name,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9,
			        $10, $11::jsonb, $12::jsonb, $13, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, *threadID, role, text, citations,
			scopeMaterial, scopeSession, scopePage,
			mode, visualEvidence, answer, modelName,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityAssistantMessage, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, assistantMessageRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, assistantMessageRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// Merge: present pointers win (messages are append-only in practice;
	// the merge path only guards a same-id race). Absent/empty evidence /
	// answer / model KEEP the stored value — a rebase never blanks a
	// complete visual answer with "".
	if text == "" {
		text = obj.text
	}
	if citations == nil {
		citations = obj.citations
	}
	if p.AssistantMode == nil {
		mode = obj.mode
	}
	if visualEvidence == nil {
		visualEvidence = obj.visualEvid
	}
	if answer == nil {
		answer = obj.answer
	}
	if modelName == "" {
		modelName = obj.modelName
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE assistant_messages
		SET thread_id = $3, role = $4, text = $5, citations = $6::jsonb,
		    scope_material_id = $7, scope_session_id = $8, scope_page_number = $9,
		    mode = $10, visual_evidence = $11::jsonb, answer = $12::jsonb,
		    model_name = $13,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, *threadID, role, text, citations,
		scopeMaterial, scopeSession, scopePage,
		mode, visualEvidence, answer, modelName,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityAssistantMessage, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// validJSONPayload returns the payload when it is non-empty AND valid
// JSON (JSONB columns reject anything else and would abort the batch's
// transaction); nil otherwise.
func validJSONPayload(p *string) *string {
	if p == nil || *p == "" || !json.Valid([]byte(*p)) {
		return nil
	}
	return p
}

// --- Cascades --------------------------------------------------------------------------

// cascadeDeleteMaterialChildren tombstones a material's pages and
// annotations (the material delete's cascade, mirroring the schedule →
// exceptions cascade). Every bumped child is logged so other devices
// drop their local rows too.
func (s *Service) cascadeDeleteMaterialChildren(ctx context.Context, q store.Q, userID, materialID uuid.UUID) error {
	for _, target := range []struct {
		table  string
		entity string
	}{
		{"material_pages", EntityMaterialPage},
		{"material_annotations", EntityMaterialAnnotation},
	} {
		rows, err := q.Query(ctx, `
			UPDATE `+target.table+`
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE user_id = $1 AND material_id = $2 AND deleted_at IS NULL
			RETURNING id, server_version`, userID, materialID)
		if err != nil {
			return err
		}
		type bumped struct {
			id uuid.UUID
			v  int
		}
		var kids []bumped
		for rows.Next() {
			b := bumped{}
			if err := rows.Scan(&b.id, &b.v); err != nil {
				rows.Close()
				return err
			}
			kids = append(kids, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, b := range kids {
			if err := logChange(ctx, q, userID, target.entity, b.id, "delete", b.v); err != nil {
				return err
			}
		}
	}
	return nil
}

// cascadeDeleteAssistantMessages tombstones a thread's messages (the
// thread delete's cascade).
func (s *Service) cascadeDeleteAssistantMessages(ctx context.Context, q store.Q, userID, threadID uuid.UUID) error {
	rows, err := q.Query(ctx, `
		UPDATE assistant_messages
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND thread_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, threadID)
	if err != nil {
		return err
	}
	type bumped struct {
		id uuid.UUID
		v  int
	}
	var kids []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		kids = append(kids, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range kids {
		if err := logChange(ctx, q, userID, EntityAssistantMessage, b.id, "delete", b.v); err != nil {
			return err
		}
	}
	return nil
}

// detachCourseFromMaterials clears course_id on a deleted course's
// materials (转入未归类 — materials survive course deletion). Every bumped
// row is logged so other devices drop the reference too.
func (s *Service) detachCourseFromMaterials(ctx context.Context, q store.Q, userID, courseID uuid.UUID) error {
	rows, err := q.Query(ctx, `
		UPDATE course_materials
		SET course_id = NULL, server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND course_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, courseID)
	if err != nil {
		return err
	}
	type bumped struct {
		id uuid.UUID
		v  int
	}
	var items []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		items = append(items, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range items {
		if err := logChange(ctx, q, userID, EntityMaterial, b.id, "upsert", b.v); err != nil {
			return err
		}
	}
	return nil
}

// detachCourseFromAssistantThreads clears course_id on a deleted course's
// assistant threads (they become 未归类 threads).
func (s *Service) detachCourseFromAssistantThreads(ctx context.Context, q store.Q, userID, courseID uuid.UUID) error {
	rows, err := q.Query(ctx, `
		UPDATE assistant_threads
		SET course_id = NULL, server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND course_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, courseID)
	if err != nil {
		return err
	}
	type bumped struct {
		id uuid.UUID
		v  int
	}
	var items []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		items = append(items, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range items {
		if err := logChange(ctx, q, userID, EntityAssistantThread, b.id, "upsert", b.v); err != nil {
			return err
		}
	}
	return nil
}

// MaterialMeta is the file-transfer route's view of one material row.
type MaterialMeta struct {
	Deleted           bool
	MimeType          string
	FileSize          int64
	ContentHash       string
	BorrowsAttachment bool
}

// GetMaterialMeta loads the ownership/contract fields of one material for
// the file-transfer route (the attachment meta pattern). Nil means the
// row does not exist for this user.
func GetMaterialMeta(ctx context.Context, q store.Q, userID, id uuid.UUID) (*MaterialMeta, error) {
	var deletedAt *time.Time
	var mime, hash string
	var size int64
	var sourceAttachment *uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT mime_type, file_size, content_hash, source_attachment_id, deleted_at
		FROM course_materials WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&mime, &size, &hash, &sourceAttachment, &deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &MaterialMeta{
		Deleted:           deletedAt != nil,
		MimeType:          mime,
		FileSize:          size,
		ContentHash:       hash,
		BorrowsAttachment: sourceAttachment != nil,
	}, nil
}
