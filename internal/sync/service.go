package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/internal/config"
	attachmentstore "livetranslate/server/internal/storage"
	"livetranslate/server/internal/store"
)

// --- post-commit hooks -------------------------------------------------------
//
// Attachment deletes tombstone the row inside the push transaction but
// must only reap files AFTER the commit (a rollback must leave files
// alone). The collector rides the request context, so concurrent pushes
// each carry their own.

type postCommitKey struct{}

type postCommitHooks struct {
	mu  sync.Mutex
	fns []func()
}

func withPostCommit(ctx context.Context) context.Context {
	return context.WithValue(ctx, postCommitKey{}, &postCommitHooks{})
}

func runPostCommit(ctx context.Context) {
	h, ok := ctx.Value(postCommitKey{}).(*postCommitHooks)
	if !ok {
		return
	}
	h.mu.Lock()
	fns := h.fns
	h.fns = nil
	h.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func deferFileCleanup(ctx context.Context, fn func()) {
	if h, ok := ctx.Value(postCommitKey{}).(*postCommitHooks); ok {
		h.mu.Lock()
		h.fns = append(h.fns, fn)
		h.mu.Unlock()
	}
}

// Service is the sync engine. All public methods take the resolved user ID;
// every SQL statement filters on it (user isolation is structural).
type Service struct {
	cfg *config.Config
	db  *store.DB
	// attachments is the optional file backend used to reap files after a
	// tombstone commit (best-effort, outside the push transaction).
	attachments *attachmentstore.Store
}

func NewService(cfg *config.Config, db *store.DB) *Service {
	return &Service{cfg: cfg, db: db}
}

// SetAttachmentStore wires the optional file backend used for post-commit
// file reaping after attachment tombstones. Nil (the default) disables
// file cleanup; metadata still tombstones correctly.
func (s *Service) SetAttachmentStore(store *attachmentstore.Store) {
	s.attachments = store
}

// --- Row readers ------------------------------------------------------------

type sessionRow struct {
	id        uuid.UUID
	userID    uuid.UUID
	title     string
	startedAt time.Time
	endedAt   *time.Time
	duration  float64
	srcLang   string
	tgtLang   string
	status    string
	abnormal  bool
	courseID  *uuid.UUID
	serverVer int
	createdAt time.Time
	updatedAt time.Time
	deletedAt *time.Time
}

const sessionCols = `id, user_id, title, started_at, ended_at, duration,
	source_language, target_language, session_status, abnormal_termination,
	course_id, server_version, created_at, updated_at, deleted_at`

func scanSession(r interface{ Scan(...any) error }) (*sessionRow, error) {
	s := &sessionRow{}
	err := r.Scan(&s.id, &s.userID, &s.title, &s.startedAt, &s.endedAt, &s.duration,
		&s.srcLang, &s.tgtLang, &s.status, &s.abnormal, &s.courseID, &s.serverVer,
		&s.createdAt, &s.updatedAt, &s.deletedAt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func fetchSession(ctx context.Context, q store.Q, userID, id uuid.UUID) (*sessionRow, error) {
	r := q.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM classroom_sessions WHERE id = $1 AND user_id = $2`, id, userID)
	s, err := scanSession(r)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return s, err
}

func sessionRecordJSON(s *sessionRow) json.RawMessage {
	var courseID *string
	if s.courseID != nil {
		cid := s.courseID.String()
		courseID = &cid
	}
	b, _ := json.Marshal(sessionRecord{
		EntityType: EntitySession, ID: s.id.String(), Title: s.title,
		StartedAt: &s.startedAt, EndedAt: s.endedAt, Duration: s.duration,
		SessionStatus: s.status, AbnormalTermination: s.abnormal,
		CourseID: courseID, ServerVersion: s.serverVer, Deleted: s.deletedAt != nil,
	})
	return b
}

type entryRow struct {
	id          uuid.UUID
	sessionID   uuid.UUID
	sequenceID  int
	startOffset float64
	endOffset   float64
	russian     string
	chinese     *string
	status      string
	timeSource  string
	serverVer   int
	updatedAt   time.Time
	deletedAt   *time.Time
}

func fetchEntry(ctx context.Context, q store.Q, userID, id uuid.UUID) (*entryRow, error) {
	e := &entryRow{}
	err := q.QueryRow(ctx, `
		SELECT id, session_id, sequence_id, start_offset, end_offset,
		       russian_text, chinese_text, translation_status, time_source,
		       server_version, updated_at, deleted_at
		FROM transcript_entries WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&e.id, &e.sessionID, &e.sequenceID, &e.startOffset, &e.endOffset,
		&e.russian, &e.chinese, &e.status, &e.timeSource,
		&e.serverVer, &e.updatedAt, &e.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

func entryRecordJSON(e *entryRow) json.RawMessage {
	b, _ := json.Marshal(entryRecord{
		EntityType: EntityEntry, ID: e.id.String(), SessionID: e.sessionID.String(),
		SequenceID: e.sequenceID, StartOffset: e.startOffset, EndOffset: e.endOffset,
		RussianText: e.russian, ChineseText: e.chinese, TranslationStatus: e.status,
		TimeSource:    e.timeSource,
		ServerVersion: e.serverVer, Deleted: e.deletedAt != nil,
	})
	return b
}

// --- Transcript corrections -----------------------------------------------------

type correctionRow struct {
	id           uuid.UUID // == transcript_entries.id
	userID       uuid.UUID
	sessionID    uuid.UUID
	russian      string
	chinese      *string
	modifiedAt   time.Time
	needsRetrans bool
	serverVer    int
	createdAt    time.Time
	updatedAt    time.Time
	deletedAt    *time.Time
}

func fetchCorrection(ctx context.Context, q store.Q, userID, id uuid.UUID) (*correctionRow, error) {
	c := &correctionRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, session_id, russian_text, chinese_text,
		       modified_at, needs_retranslation,
		       server_version, created_at, updated_at, deleted_at
		FROM transcript_corrections WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&c.id, &c.userID, &c.sessionID, &c.russian, &c.chinese,
		&c.modifiedAt, &c.needsRetrans,
		&c.serverVer, &c.createdAt, &c.updatedAt, &c.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func correctionRecordJSON(c *correctionRow) json.RawMessage {
	b, _ := json.Marshal(transcriptCorrectionRecord{
		EntityType: EntityTranscriptCorrection, ID: c.id.String(),
		SessionID:   c.sessionID.String(),
		RussianText: c.russian, ChineseText: c.chinese,
		ModifiedAt: c.modifiedAt, NeedsRetrans: c.needsRetrans,
		ServerVersion: c.serverVer, Deleted: c.deletedAt != nil,
	})
	return b
}

type bookmarkRow struct {
	id           uuid.UUID
	sessionID    uuid.UUID
	entryID      uuid.UUID
	isBookmarked bool
	serverVer    int
	updatedAt    time.Time
	deletedAt    *time.Time
}

func fetchBookmark(ctx context.Context, q store.Q, userID, id uuid.UUID) (*bookmarkRow, error) {
	b := &bookmarkRow{}
	err := q.QueryRow(ctx, `
		SELECT id, session_id, entry_id, is_bookmarked, server_version, updated_at, deleted_at
		FROM bookmarks WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&b.id, &b.sessionID, &b.entryID, &b.isBookmarked, &b.serverVer, &b.updatedAt, &b.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func bookmarkRecordJSON(b *bookmarkRow) json.RawMessage {
	bs, _ := json.Marshal(bookmarkRecord{
		EntityType: EntityBookmark, ID: b.id.String(), SessionID: b.sessionID.String(),
		EntryID: b.entryID.String(), IsBookmarked: b.isBookmarked,
		ServerVersion: b.serverVer, Deleted: b.deletedAt != nil,
	})
	return bs
}

type favoriteRow struct {
	id         uuid.UUID
	sessionID  uuid.UUID
	isFavorite bool
	serverVer  int
	updatedAt  time.Time
	deletedAt  *time.Time
}

func fetchFavorite(ctx context.Context, q store.Q, userID, id uuid.UUID) (*favoriteRow, error) {
	f := &favoriteRow{}
	err := q.QueryRow(ctx, `
		SELECT id, session_id, is_favorite, server_version, updated_at, deleted_at
		FROM favorite_sessions WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&f.id, &f.sessionID, &f.isFavorite, &f.serverVer, &f.updatedAt, &f.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func favoriteRecordJSON(f *favoriteRow) json.RawMessage {
	b, _ := json.Marshal(favoriteRecord{
		EntityType: EntityFavorite, ID: f.id.String(), SessionID: f.sessionID.String(),
		IsFavorite: f.isFavorite, ServerVersion: f.serverVer, Deleted: f.deletedAt != nil,
	})
	return b
}

type courseRow struct {
	id         uuid.UUID
	userID     uuid.UUID
	name       string
	teacher    string
	location   string
	colorIndex int
	isArchived bool
	serverVer  int
	createdAt  time.Time
	updatedAt  time.Time
	deletedAt  *time.Time
}

func fetchCourse(ctx context.Context, q store.Q, userID, id uuid.UUID) (*courseRow, error) {
	c := &courseRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, name, teacher, location, color_index, is_archived,
		       server_version, created_at, updated_at, deleted_at
		FROM courses WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&c.id, &c.userID, &c.name, &c.teacher, &c.location, &c.colorIndex,
		&c.isArchived, &c.serverVer, &c.createdAt, &c.updatedAt, &c.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func courseRecordJSON(c *courseRow) json.RawMessage {
	b, _ := json.Marshal(courseRecord{
		EntityType: EntityCourse, ID: c.id.String(), Title: c.name,
		Teacher: c.teacher, Location: c.location, ColorIndex: c.colorIndex,
		IsArchived: c.isArchived, ServerVersion: c.serverVer, Deleted: c.deletedAt != nil,
	})
	return b
}

type noteRow struct {
	id          uuid.UUID
	userID      uuid.UUID
	sessionID   uuid.UUID
	anchorEntry *uuid.UUID
	noteText    string
	timeOffset  *float64
	serverVer   int
	createdAt   time.Time
	updatedAt   time.Time
	deletedAt   *time.Time
}

func fetchNote(ctx context.Context, q store.Q, userID, id uuid.UUID) (*noteRow, error) {
	n := &noteRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, session_id, anchor_entry_id, note_text, time_offset,
		       server_version, created_at, updated_at, deleted_at
		FROM session_notes WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&n.id, &n.userID, &n.sessionID, &n.anchorEntry, &n.noteText, &n.timeOffset,
		&n.serverVer, &n.createdAt, &n.updatedAt, &n.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return n, nil
}

func noteRecordJSON(n *noteRow) json.RawMessage {
	var anchor *string
	if n.anchorEntry != nil {
		a := n.anchorEntry.String()
		anchor = &a
	}
	b, _ := json.Marshal(noteRecord{
		EntityType: EntityNote, ID: n.id.String(), SessionID: n.sessionID.String(),
		AnchorEntryID: anchor, NoteText: n.noteText, TimeOffset: n.timeOffset,
		ServerVersion: n.serverVer, Deleted: n.deletedAt != nil,
	})
	return b
}

type studyReviewRow struct {
	id             uuid.UUID // == session id
	userID         uuid.UUID
	sessionID      uuid.UUID
	status         string
	content        string
	generated      string
	reviewModel    string
	generatedAt    *time.Time
	sourceUpdateAt *time.Time
	serverVer      int
	createdAt      time.Time
	updatedAt      time.Time
	deletedAt      *time.Time
}

func fetchStudyReview(ctx context.Context, q store.Q, userID, id uuid.UUID) (*studyReviewRow, error) {
	r := &studyReviewRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, session_id, status, content, generated_content,
		       review_model, generated_at, source_updated_at,
		       server_version, created_at, updated_at, deleted_at
		FROM study_reviews WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&r.id, &r.userID, &r.sessionID, &r.status, &r.content, &r.generated,
		&r.reviewModel, &r.generatedAt, &r.sourceUpdateAt,
		&r.serverVer, &r.createdAt, &r.updatedAt, &r.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func studyReviewRecordJSON(r *studyReviewRow) json.RawMessage {
	b, _ := json.Marshal(studyReviewRecord{
		EntityType: EntityStudyReview, ID: r.id.String(), SessionID: r.sessionID.String(),
		Status: r.status, Content: r.content, GeneratedContent: r.generated,
		ReviewModel: r.reviewModel, GeneratedAt: r.generatedAt, SourceUpdatedAt: r.sourceUpdateAt,
		ServerVersion: r.serverVer, Deleted: r.deletedAt != nil,
	})
	return b
}

type attachmentRow struct {
	id          uuid.UUID
	userID      uuid.UUID
	sessionID   uuid.UUID
	courseID    *uuid.UUID
	anchorEntry *uuid.UUID
	capturedAt  time.Time
	title       string
	caption     string
	kind        string
	mime        string
	width       int
	height      int
	fileSize    int64
	hash        string
	sortIndex   int
	transform   string
	analysisSt  string
	analysis    []byte
	ocrText     string
	serverVer   int
	createdAt   time.Time
	updatedAt   time.Time
	deletedAt   *time.Time
}

func fetchAttachment(ctx context.Context, q store.Q, userID, id uuid.UUID) (*attachmentRow, error) {
	a := &attachmentRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, session_id, course_id, anchor_entry_id, captured_at,
		       title, caption, kind, mime_type, pixel_width, pixel_height,
		       file_size, content_hash, sort_index, transform_json,
		       analysis_status, analysis, ocr_text,
		       server_version, created_at, updated_at, deleted_at
		FROM session_attachments WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&a.id, &a.userID, &a.sessionID, &a.courseID, &a.anchorEntry, &a.capturedAt,
		&a.title, &a.caption, &a.kind, &a.mime, &a.width, &a.height,
		&a.fileSize, &a.hash, &a.sortIndex, &a.transform,
		&a.analysisSt, &a.analysis, &a.ocrText,
		&a.serverVer, &a.createdAt, &a.updatedAt, &a.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func attachmentRecordJSON(a *attachmentRow) json.RawMessage {
	var courseID, anchor *string
	if a.courseID != nil {
		cid := a.courseID.String()
		courseID = &cid
	}
	if a.anchorEntry != nil {
		aid := a.anchorEntry.String()
		anchor = &aid
	}
	var analysis *string
	if len(a.analysis) > 0 {
		s := string(a.analysis)
		analysis = &s
	}
	b, _ := json.Marshal(attachmentRecord{
		EntityType: EntityAttachment, ID: a.id.String(), SessionID: a.sessionID.String(),
		CourseID: courseID, AnchorEntryID: anchor,
		CapturedAt: a.capturedAt, Title: a.title, Caption: a.caption,
		Kind: a.kind, MimeType: a.mime, Width: a.width, Height: a.height,
		FileSize: a.fileSize, ContentHash: a.hash, SortIndex: a.sortIndex,
		Transform:     a.transform,
		AnalysisState: a.analysisSt, Analysis: analysis, OcrText: a.ocrText,
		ServerVersion: a.serverVer, Deleted: a.deletedAt != nil,
	})
	return b
}

// AttachmentMeta is the read-only projection the /v1/attachments routes
// need for ownership and byte-contract checks.
type AttachmentMeta struct {
	Deleted     bool
	MimeType    string
	FileSize    int64
	ContentHash string
}

// GetAttachmentMeta loads the ownership/contract fields of one
// attachment. Nil means the row does not exist for this user.

// --- Learning entities: term / study_card / study_task ----------------------------

type termRow struct {
	id            uuid.UUID
	userID        uuid.UUID
	courseID      *uuid.UUID
	sessionID     *uuid.UUID
	sourceReview  *uuid.UUID
	sourceEntry   *uuid.UUID
	sourceAttach  *uuid.UUID
	sourceSessIDs string
	russian       string
	chinese       string
	explanation   string
	partOfSpeech  string
	userNote      string
	favorite      bool
	status        string
	serverVer     int
	createdAt     time.Time
	updatedAt     time.Time
	deletedAt     *time.Time
}

func fetchTerm(ctx context.Context, q store.Q, userID, id uuid.UUID) (*termRow, error) {
	t := &termRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, course_id, session_id, source_review_id,
		       source_entry_id, source_attachment_id, source_session_ids,
		       russian, chinese, explanation, part_of_speech, user_note,
		       is_favorite, status,
		       server_version, created_at, updated_at, deleted_at
		FROM glossary_terms WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&t.id, &t.userID, &t.courseID, &t.sessionID, &t.sourceReview,
		&t.sourceEntry, &t.sourceAttach, &t.sourceSessIDs,
		&t.russian, &t.chinese, &t.explanation, &t.partOfSpeech, &t.userNote,
		&t.favorite, &t.status,
		&t.serverVer, &t.createdAt, &t.updatedAt, &t.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func termRecordJSON(t *termRow) json.RawMessage {
	courseID, sessionID, review, entry, attach := optUUIDString(t.courseID), optUUIDString(t.sessionID), optUUIDString(t.sourceReview), optUUIDString(t.sourceEntry), optUUIDString(t.sourceAttach)
	b, _ := json.Marshal(termRecord{
		EntityType: EntityTerm, ID: t.id.String(),
		CourseID: courseID, SessionID: sessionID,
		SourceReview: review, SourceEntry: entry, SourceAttach: attach,
		SourceSessIDs: t.sourceSessIDs,
		Russian:       t.russian, Chinese: t.chinese, Explanation: t.explanation,
		PartOfSpeech: t.partOfSpeech, UserNote: t.userNote,
		Favorite: t.favorite, Status: t.status,
		ServerVersion: t.serverVer, Deleted: t.deletedAt != nil,
	})
	return b
}

type studyCardRow struct {
	id            uuid.UUID
	userID        uuid.UUID
	courseID      *uuid.UUID
	sessionID     *uuid.UUID
	sourceEntry   *uuid.UUID
	sourceAttach  *uuid.UUID
	sourceTerm    *uuid.UUID
	front         string
	back          string
	cardType      string
	userNote      string
	origin        string
	stage         string
	reviewCount   int
	intervalHours int
	dueAt         *time.Time
	lastReviewed  *time.Time
	lastGrade     string
	serverVer     int
	createdAt     time.Time
	updatedAt     time.Time
	deletedAt     *time.Time
}

func fetchStudyCard(ctx context.Context, q store.Q, userID, id uuid.UUID) (*studyCardRow, error) {
	c := &studyCardRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, course_id, session_id, source_entry_id,
		       source_attachment_id, source_term_id,
		       front, back, card_type, user_note, origin,
		       stage, review_count, interval_hours, due_at, last_reviewed_at, last_grade,
		       server_version, created_at, updated_at, deleted_at
		FROM study_cards WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&c.id, &c.userID, &c.courseID, &c.sessionID, &c.sourceEntry,
		&c.sourceAttach, &c.sourceTerm,
		&c.front, &c.back, &c.cardType, &c.userNote, &c.origin,
		&c.stage, &c.reviewCount, &c.intervalHours, &c.dueAt, &c.lastReviewed, &c.lastGrade,
		&c.serverVer, &c.createdAt, &c.updatedAt, &c.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func studyCardRecordJSON(c *studyCardRow) json.RawMessage {
	courseID, sessionID, entry, attach, term := optUUIDString(c.courseID), optUUIDString(c.sessionID), optUUIDString(c.sourceEntry), optUUIDString(c.sourceAttach), optUUIDString(c.sourceTerm)
	b, _ := json.Marshal(studyCardRecord{
		EntityType: EntityStudyCard, ID: c.id.String(),
		CourseID: courseID, SessionID: sessionID,
		SourceEntry: entry, SourceAttach: attach, SourceTerm: term,
		Front: c.front, Back: c.back, CardType: c.cardType, UserNote: c.userNote,
		Origin: c.origin, Stage: c.stage,
		ReviewCount: c.reviewCount, IntervalHrs: c.intervalHours,
		DueAt: c.dueAt, LastReviewed: c.lastReviewed, LastGrade: c.lastGrade,
		ServerVersion: c.serverVer, Deleted: c.deletedAt != nil,
	})
	return b
}

type studyTaskRow struct {
	id           uuid.UUID
	userID       uuid.UUID
	courseID     *uuid.UUID
	sessionID    *uuid.UUID
	sourceReview *uuid.UUID
	sourceEntry  *uuid.UUID
	sourceAttach *uuid.UUID
	title        string
	detail       string
	dueAt        *time.Time
	priority     string
	status       string
	origin       string
	uncertainty  string
	userNote     string
	completedAt  *time.Time
	serverVer    int
	createdAt    time.Time
	updatedAt    time.Time
	deletedAt    *time.Time
}

func fetchStudyTask(ctx context.Context, q store.Q, userID, id uuid.UUID) (*studyTaskRow, error) {
	t := &studyTaskRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, course_id, session_id, source_review_id,
		       source_entry_id, source_attachment_id,
		       title, detail, due_at, priority, status, origin,
		       uncertainty, user_note, completed_at,
		       server_version, created_at, updated_at, deleted_at
		FROM study_tasks WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&t.id, &t.userID, &t.courseID, &t.sessionID, &t.sourceReview,
		&t.sourceEntry, &t.sourceAttach,
		&t.title, &t.detail, &t.dueAt, &t.priority, &t.status, &t.origin,
		&t.uncertainty, &t.userNote, &t.completedAt,
		&t.serverVer, &t.createdAt, &t.updatedAt, &t.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func studyTaskRecordJSON(t *studyTaskRow) json.RawMessage {
	courseID, sessionID, review, entry, attach := optUUIDString(t.courseID), optUUIDString(t.sessionID), optUUIDString(t.sourceReview), optUUIDString(t.sourceEntry), optUUIDString(t.sourceAttach)
	b, _ := json.Marshal(studyTaskRecord{
		EntityType: EntityStudyTask, ID: t.id.String(), Title: t.title,
		CourseID: courseID, SessionID: sessionID,
		SourceReview: review, SourceEntry: entry, SourceAttach: attach,
		Detail: t.detail, DueAt: t.dueAt, Priority: t.priority, Status: t.status,
		Origin: t.origin, Uncertainty: t.uncertainty, UserNote: t.userNote,
		CompletedAt:   t.completedAt,
		ServerVersion: t.serverVer, Deleted: t.deletedAt != nil,
	})
	return b
}

// optUUIDString renders a nullable UUID as an optional wire string (nil
// pointer = key omitted = "no source / unscoped").
func optUUIDString(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

// refOrNil converts a payload reference field into a row value: nil (keep
// — never reaches an INSERT) and the nil-UUID sentinel both become SQL
// NULL; a real UUID passes through. Used on INSERT paths where absence
// and "cleared" are the same thing (nothing stored to keep).
func refOrNil(p *uuid.UUID) *uuid.UUID {
	if p == nil || *p == uuid.Nil {
		return nil
	}
	return p
}

// mergeRef applies the reference-merge sentinel to an existing row value:
// absent payload field keeps the stored ref; uuid.Nil clears it; any
// other UUID assigns it.
func mergeRef(stored *uuid.UUID, p *uuid.UUID) *uuid.UUID {
	if p == nil {
		return stored
	}
	if *p == uuid.Nil {
		return nil
	}
	return p
}

// GetAttachmentMeta loads the ownership/contract fields of one
// attachment. Nil means the row does not exist for this user.
func GetAttachmentMeta(ctx context.Context, q store.Q, userID, id uuid.UUID) (*AttachmentMeta, error) {
	var deletedAt *time.Time
	var mime, hash string
	var size int64
	err := q.QueryRow(ctx, `
		SELECT mime_type, file_size, content_hash, deleted_at
		FROM session_attachments WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&mime, &size, &hash, &deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &AttachmentMeta{Deleted: deletedAt != nil, MimeType: mime, FileSize: size, ContentHash: hash}, nil
}

// --- Change log + ledger helpers ---------------------------------------------

func logChange(ctx context.Context, q store.Q, userID uuid.UUID, entityType string, entityID uuid.UUID, operation string, serverVersion int) error {
	_, err := q.Exec(ctx, `
		INSERT INTO sync_changes (user_id, entity_type, entity_id, operation, server_version, created_at)
		VALUES ($1, $2, $3, $4, $5, now())`, userID, entityType, entityID, operation, serverVersion)
	return err
}

func storeLedger(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem, result *PushItemResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO processed_operations (user_id, operation_id, entity_type, entity_id, result, created_at)
		VALUES ($1, $2, $3, $4, $5, now())`, userID, item.OperationID, item.EntityType, item.EntityID, raw)
	return err
}

func accepted(item *PushItem, version int, updatedAt time.Time) *PushItemResult {
	return &PushItemResult{
		OperationID: item.OperationID, Status: "accepted",
		ServerVersion: &version, ServerUpdatedAt: &updatedAt,
	}
}

func conflict(item *PushItem, version int, updatedAt time.Time, record json.RawMessage) *PushItemResult {
	code := "stale_base_version"
	return &PushItemResult{
		OperationID: item.OperationID, Status: "conflict",
		ServerVersion: &version, ServerUpdatedAt: &updatedAt,
		ErrorCode: &code, ServerRecord: record,
	}
}

func rejected(item *PushItem, code string) *PushItemResult {
	return &PushItemResult{OperationID: item.OperationID, Status: "rejected", ErrorCode: &code}
}

// --- Push ---------------------------------------------------------------------

// ApplyPush applies a whole batch in ONE transaction. Per item the
// semantics are identical to the Python _apply_one:
//
//  1. ledger hit → replay the stored result verbatim;
//  2. delete → tombstone (+cascade for sessions; phantom tombstones for
//     never-seen entities); idempotent re-delete returns accepted;
//  3. upsert → insert@v1 / base==ver merge / base<ver conflict / base>ver
//     rejected; deleted rows conflict (delete-wins, no resurrection).
func (s *Service) ApplyPush(ctx context.Context, userID uuid.UUID, items []PushItem) ([]PushItemResult, error) {
	results := make([]PushItemResult, 0, len(items))
	// File-reaping hooks registered during the transaction run only after
	// a successful commit; a rollback discards them.
	ctx = withPostCommit(ctx)
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		for i := range items {
			res, err := s.applyOne(ctx, q, userID, &items[i])
			if err != nil {
				return fmt.Errorf("operation %s: %w", items[i].OperationID, err)
			}
			results = append(results, *res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	runPostCommit(ctx)
	return results, nil
}

func (s *Service) applyOne(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	// 1. Idempotency ledger.
	var stored []byte
	err := q.QueryRow(ctx, `
		SELECT result FROM processed_operations
		WHERE user_id = $1 AND operation_id = $2`, userID, item.OperationID,
	).Scan(&stored)
	if err == nil {
		var res PushItemResult
		if err := json.Unmarshal(stored, &res); err == nil {
			return &res, nil
		}
		// Corrupt ledger row: fall through to re-processing (safe —
		// deterministic UUIDs make the write idempotent at the row level).
	} else if err != pgx.ErrNoRows {
		return nil, err
	}

	// 2. Structural validation (permanent rejects).
	if !validEntityType(item.EntityType) {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.Operation != "upsert" && item.Operation != "delete" {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	switch item.EntityType {
	case EntitySession:
		return s.applySession(ctx, q, userID, item)
	case EntityEntry:
		return s.applyEntry(ctx, q, userID, item)
	case EntityBookmark:
		return s.applyBookmark(ctx, q, userID, item)
	case EntityFavorite:
		return s.applyFavorite(ctx, q, userID, item)
	case EntityCourse:
		return s.applyCourse(ctx, q, userID, item)
	case EntityNote:
		return s.applyNote(ctx, q, userID, item)
	case EntityAttachment:
		return s.applyAttachment(ctx, q, userID, item)
	case EntityTerm:
		return s.applyTerm(ctx, q, userID, item)
	case EntityStudyCard:
		return s.applyStudyCard(ctx, q, userID, item)
	case EntityStudyTask:
		return s.applyStudyTask(ctx, q, userID, item)
	case EntityTranscriptCorrection:
		return s.applyCorrection(ctx, q, userID, item)
	default:
		return s.applyStudyReview(ctx, q, userID, item)
	}
}

// --- Session ------------------------------------------------------------------

func (s *Service) applySession(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchSession(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}

	if item.Operation == "delete" {
		if obj == nil {
			// Phantom tombstone: never existed server-side; record the
			// delete so offline devices learn about it.
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO classroom_sessions
					(id, user_id, title, started_at, session_status, server_version, created_at, updated_at, deleted_at)
				VALUES ($1, $2, '', $3, 'finished', 1, now(), now(), now())
				RETURNING updated_at`, item.EntityID, userID, item.ClientUpdatedAt,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntitySession, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj.deletedAt != nil {
			res := accepted(item, obj.serverVer, obj.updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		var version int
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			UPDATE classroom_sessions
			SET deleted_at = now(), session_status = 'finished',
			    server_version = server_version + 1, updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING server_version, updated_at`, item.EntityID, userID,
		).Scan(&version, &updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntitySession, item.EntityID, "delete", version); err != nil {
			return nil, err
		}
		if err := s.cascadeDeleteChildren(ctx, q, userID, item.EntityID); err != nil {
			return nil, err
		}
		res := accepted(item, version, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// upsert
	p := item.Payload
	if obj == nil {
		if p.StartedAt == nil {
			res := rejected(item, "schema")
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		title := ""
		if p.Title != nil {
			title = *p.Title
		}
		duration := 0.0
		if p.Duration != nil {
			duration = *p.Duration
		}
		src, tgt := "ru", "zh-CN"
		if p.SourceLanguage != nil {
			src = *p.SourceLanguage
		}
		if p.TargetLanguage != nil {
			tgt = *p.TargetLanguage
		}
		status := "active"
		if p.SessionStatus != nil {
			status = *p.SessionStatus
		}
		abnormal := false
		if p.AbnormalTermination != nil {
			abnormal = *p.AbnormalTermination
		}
		// Course reference: uuid.Nil never reaches the row (treated as no
		// course); any other id is stored as-is.
		var courseID *uuid.UUID
		if p.CourseID != nil && *p.CourseID != uuid.Nil {
			courseID = p.CourseID
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO classroom_sessions
				(id, user_id, title, started_at, ended_at, duration, source_language,
				 target_language, session_status, abnormal_termination, course_id,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, title, *p.StartedAt, p.EndedAt, duration,
			src, tgt, status, abnormal, courseID,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntitySession, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		// Delete-wins: deleted sessions stay deleted.
		res := conflict(item, obj.serverVer, obj.updatedAt, sessionRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, sessionRecordJSON(obj))
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

	// Merge (field rules identical to Python merge_record for sessions):
	// non-null incoming fields overwrite; abnormal termination ORs. The
	// course reference follows the payload sentinel: absent keeps the
	// stored value, uuid.Nil clears it, any other id assigns it (a
	// dangling reference is allowed — courses and sessions sync as
	// independent entities).
	title, endedAt, duration, status, abnormal := obj.title, obj.endedAt, obj.duration, obj.status, obj.abnormal
	courseID := obj.courseID
	if p.Title != nil {
		title = *p.Title
	}
	if p.EndedAt != nil {
		endedAt = p.EndedAt
	}
	if p.Duration != nil {
		duration = *p.Duration
	}
	if p.SessionStatus != nil {
		status = *p.SessionStatus
	}
	if p.AbnormalTermination != nil {
		abnormal = abnormal || *p.AbnormalTermination
	}
	if p.CourseID != nil {
		if *p.CourseID == uuid.Nil {
			courseID = nil
		} else {
			courseID = p.CourseID
		}
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE classroom_sessions
		SET title = $3, ended_at = $4, duration = $5, session_status = $6,
		    abnormal_termination = $7, course_id = $8,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, title, endedAt, duration, status, abnormal, courseID,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntitySession, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// cascadeDeleteChildren tombstones the session's live entries, bookmarks,
// favorites and notes, bumping each version and emitting a delete change.
func (s *Service) cascadeDeleteChildren(ctx context.Context, q store.Q, userID, sessionID uuid.UUID) error {
	rows, err := q.Query(ctx, `
		UPDATE transcript_entries
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND session_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, sessionID)
	if err != nil {
		return err
	}
	type bumped struct {
		id uuid.UUID
		v  int
	}
	var entries []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range entries {
		if err := logChange(ctx, q, userID, EntityEntry, b.id, "delete", b.v); err != nil {
			return err
		}
	}

	for _, table := range []string{"bookmarks", "favorite_sessions"} {
		rows, err := q.Query(ctx, `
			UPDATE `+table+`
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE user_id = $1 AND session_id = $2 AND deleted_at IS NULL
			RETURNING id, server_version`, userID, sessionID)
		if err != nil {
			return err
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
		entity := EntityBookmark
		if table == "favorite_sessions" {
			entity = EntityFavorite
		}
		for _, b := range kids {
			if err := logChange(ctx, q, userID, entity, b.id, "delete", b.v); err != nil {
				return err
			}
		}
	}

	rows, err = q.Query(ctx, `
		UPDATE session_notes
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND session_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, sessionID)
	if err != nil {
		return err
	}
	var notes []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range notes {
		if err := logChange(ctx, q, userID, EntityNote, b.id, "delete", b.v); err != nil {
			return err
		}
	}

	rows, err = q.Query(ctx, `
		UPDATE study_reviews
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND session_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, sessionID)
	if err != nil {
		return err
	}
	var reviews []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		reviews = append(reviews, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range reviews {
		if err := logChange(ctx, q, userID, EntityStudyReview, b.id, "delete", b.v); err != nil {
			return err
		}
	}

	// Attachments follow the session into the tombstone (their files are
	// reaped later by the file GC once the row's retention lapses).
	rows, err = q.Query(ctx, `
		UPDATE session_attachments
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND session_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, sessionID)
	if err != nil {
		return err
	}
	var attachments []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		attachments = append(attachments, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range attachments {
		if err := logChange(ctx, q, userID, EntityAttachment, b.id, "delete", b.v); err != nil {
			return err
		}
	}

	// Corrections follow their entries into the tombstone (a deleted
	// entry has no text left to overlay).
	rows, err = q.Query(ctx, `
		UPDATE transcript_corrections
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND session_id = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, sessionID)
	if err != nil {
		return err
	}
	var corrections []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		corrections = append(corrections, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range corrections {
		if err := logChange(ctx, q, userID, EntityTranscriptCorrection, b.id, "delete", b.v); err != nil {
			return err
		}
	}
	return nil
}

// --- Entry ---------------------------------------------------------------------

func (s *Service) applyEntry(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchEntry(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj == nil {
			// Phantom entry tombstone.
			sid := uuid.Nil
			seq := 0
			if p.SessionID != nil {
				sid = *p.SessionID
			}
			if p.SequenceID != nil {
				seq = *p.SequenceID
			}
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO transcript_entries
					(id, user_id, session_id, sequence_id, russian_text, server_version, created_at, updated_at, deleted_at)
				VALUES ($1, $2, $3, $4, '', 1, now(), now(), now())
				RETURNING updated_at`, item.EntityID, userID, sid, seq,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityEntry, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj.deletedAt != nil {
			res := accepted(item, obj.serverVer, obj.updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		var version int
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			UPDATE transcript_entries
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING server_version, updated_at`, item.EntityID, userID,
		).Scan(&version, &updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityEntry, item.EntityID, "delete", version); err != nil {
			return nil, err
		}
		res := accepted(item, version, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// upsert: sessionId/sequenceId/russianText required.
	if p.SessionID == nil || p.SequenceID == nil || p.RussianText == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	// Parent session must exist and be live, owned by the user.
	parent, err := fetchSession(ctx, q, userID, *p.SessionID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.deletedAt != nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		start, end := 0.0, 0.0
		if p.StartOffset != nil {
			start = *p.StartOffset
		}
		if p.EndOffset != nil {
			end = *p.EndOffset
		}
		status := "pending"
		if p.TranslationStatus != nil {
			status = *p.TranslationStatus
		}
		timeSource := "legacy"
		if p.TimeSource != nil && *p.TimeSource == "audio" {
			timeSource = "audio"
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO transcript_entries
				(id, user_id, session_id, sequence_id, start_offset, end_offset,
				 russian_text, chinese_text, translation_status, time_source,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, *p.SessionID, *p.SequenceID, start, end,
			*p.RussianText, p.ChineseText, status, timeSource,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityEntry, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, entryRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, entryRecordJSON(obj))
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

	// Russian immutability: a differing incoming russian → conflict (the
	// server keeps its text). Identical text may proceed.
	if p.RussianText != nil && *p.RussianText != obj.russian {
		res := conflict(item, obj.serverVer, obj.updatedAt, entryRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	// Merge: non-empty incoming chinese wins; empty/None keeps server
	// (Python rules — no clearChinese flag exists in protocol v1).
	chinese := obj.chinese
	if p.ChineseText != nil && *p.ChineseText != "" {
		chinese = p.ChineseText
	}
	status := obj.status
	if p.TranslationStatus != nil && *p.TranslationStatus != "" {
		status = *p.TranslationStatus
	}
	start, end := obj.startOffset, obj.endOffset
	if p.StartOffset != nil && obj.startOffset == 0 {
		start = *p.StartOffset
	}
	if p.EndOffset != nil && obj.endOffset == 0 {
		end = *p.EndOffset
	}
	// Time provenance: an explicit "audio" marks sample-timeline offsets;
	// anything else keeps the stored marker (legacy is the default).
	timeSource := obj.timeSource
	if p.TimeSource != nil && *p.TimeSource == "audio" {
		timeSource = "audio"
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE transcript_entries
		SET start_offset = $3, end_offset = $4, chinese_text = $5,
		    translation_status = $6, time_source = $7,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, start, end, chinese, status, timeSource,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityEntry, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Transcript correction --------------------------------------------------------

// applyCorrection: the user's correction overlay for one entry (entity id
// == entry id). Semantics:
//
//   - delete = revert to the model's original. The tombstone bumps the
//     version, so a stale upsert from an old device hits the delete-wins
//     conflict path — a reverted correction cannot resurrect. Phantom
//     tombstones (never seen server-side) follow the shared pattern.
//   - upsert requires a live parent entry owned by the user (corrections
//     without an entry are rejected — nothing to correct).
//   - merge rule on base==ver: the side with the NEWER correctionModifiedAt
//     wins the TEXT fields. A differing older push still bumps the version
//     but keeps the stored texts (the newer edit survives); the losing
//     side's device sees the conflict result and keeps its copy locally.
//   - chinese_text preserves nil-vs-empty: nil = never corrected, ” =
//     a deliberate blank. The payload pointer carries both faithfully.
func (s *Service) applyCorrection(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchCorrection(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj == nil {
			// Phantom tombstone: the entry may exist even though the
			// correction never did — record the delete so offline devices
			// learn the correction is gone (or was never there).
			sid := uuid.Nil
			if p.SessionID != nil {
				sid = *p.SessionID
			}
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO transcript_corrections
					(id, user_id, session_id, russian_text, modified_at,
					 server_version, created_at, updated_at, deleted_at)
				VALUES ($1, $2, $3, '', $4, 1, now(), now(), now())
				RETURNING updated_at`, item.EntityID, userID, sid, item.ClientUpdatedAt,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityTranscriptCorrection, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj.deletedAt != nil {
			res := accepted(item, obj.serverVer, obj.updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		var version int
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			UPDATE transcript_corrections
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING server_version, updated_at`, item.EntityID, userID,
		).Scan(&version, &updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityTranscriptCorrection, item.EntityID, "delete", version); err != nil {
			return nil, err
		}
		res := accepted(item, version, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// upsert: the parent entry must exist and be live.
	parent, err := fetchEntry(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.deletedAt != nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	modifiedAt := time.Now()
	if p.CorrectionModifiedAt != nil {
		modifiedAt = *p.CorrectionModifiedAt
	}
	needsRetrans := false
	if p.CorrectionNeedsRetranslation != nil {
		needsRetrans = *p.CorrectionNeedsRetranslation
	}

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO transcript_corrections
				(id, user_id, session_id, russian_text, chinese_text,
				 modified_at, needs_retranslation,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, parent.sessionID,
			derefString(p.CorrectionRussian), p.CorrectionChinese,
			modifiedAt, needsRetrans,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityTranscriptCorrection, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		// Delete-wins: a reverted correction stays reverted.
		res := conflict(item, obj.serverVer, obj.updatedAt, correctionRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, correctionRecordJSON(obj))
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

	// Merge: the NEWER correctionModifiedAt wins the text fields. An older
	// push still bumps the version (so the newer state propagates) but
	// keeps the stored texts.
	russian := obj.russian
	chinese := obj.chinese
	if !modifiedAt.Before(obj.modifiedAt) {
		russian = derefString(p.CorrectionRussian)
		chinese = p.CorrectionChinese
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE transcript_corrections
		SET russian_text = $3, chinese_text = $4, modified_at = $5,
		    needs_retranslation = $6,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, russian, chinese,
		maxTime(obj.modifiedAt, modifiedAt), needsRetrans,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityTranscriptCorrection, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// derefString: nil-safe string pointer dereference (nil → "").
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// maxTime returns the later of two timestamps.
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// --- Bookmark / Favorite ---------------------------------------------------------

func (s *Service) applyBookmark(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchBookmark(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			var version int
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				UPDATE bookmarks
				SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
				WHERE id = $1 AND user_id = $2
				RETURNING server_version, updated_at`, item.EntityID, userID,
			).Scan(&version, &updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityBookmark, item.EntityID, "delete", version); err != nil {
				return nil, err
			}
			res := accepted(item, version, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj == nil {
			sid := uuid.Nil
			if p.SessionID != nil {
				sid = *p.SessionID
			}
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO bookmarks (id, user_id, session_id, entry_id, is_bookmarked,
					server_version, updated_at, deleted_at)
				VALUES ($1, $2, $3, $4::uuid, false, 1, now(), now())
				RETURNING updated_at`, item.EntityID, userID, sid, "00000000-0000-0000-0000-000000000000",
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityBookmark, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		// Already tombstoned: idempotent accepted.
		res := accepted(item, obj.serverVer, obj.updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if p.EntryID == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if obj == nil {
		if item.BaseVersion != 0 {
			// Update for something never seen: treat as permanent reject
			// (Python behavior — client pulls the record first).
			res := rejected(item, "schema")
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		sid := uuid.Nil
		if p.SessionID != nil {
			sid = *p.SessionID
		}
		is := true
		if p.IsBookmarked != nil {
			is = *p.IsBookmarked
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO bookmarks (id, user_id, session_id, entry_id, is_bookmarked,
				server_version, updated_at)
			VALUES ($1, $2, $3, $4, $5, 1, now())
			RETURNING updated_at`, item.EntityID, userID, sid, *p.EntryID, is,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityBookmark, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil || item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, bookmarkRecordJSON(obj))
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
	is := obj.isBookmarked
	if p.IsBookmarked != nil {
		is = *p.IsBookmarked
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE bookmarks SET is_bookmarked = $3,
			server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`, item.EntityID, userID, is,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityBookmark, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) applyFavorite(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchFavorite(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			var version int
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				UPDATE favorite_sessions
				SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
				WHERE id = $1 AND user_id = $2
				RETURNING server_version, updated_at`, item.EntityID, userID,
			).Scan(&version, &updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityFavorite, item.EntityID, "delete", version); err != nil {
				return nil, err
			}
			res := accepted(item, version, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj == nil {
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO favorite_sessions (id, user_id, session_id, is_favorite,
					server_version, updated_at, deleted_at)
				VALUES ($1, $2, $3, false, 1, now(), now())
				RETURNING updated_at`, item.EntityID, userID, uuid.Nil,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityFavorite, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		res := accepted(item, obj.serverVer, obj.updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		if item.BaseVersion != 0 || p.IsFavorite == nil {
			res := rejected(item, "schema")
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		// Python: session_id = entityId when payload omits sessionId.
		sid := item.EntityID
		if p.SessionID != nil {
			sid = *p.SessionID
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO favorite_sessions (id, user_id, session_id, is_favorite,
				server_version, updated_at)
			VALUES ($1, $2, $3, $4, 1, now())
			RETURNING updated_at`, item.EntityID, userID, sid, *p.IsFavorite,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityFavorite, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil || item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, favoriteRecordJSON(obj))
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
	is := obj.isFavorite
	if p.IsFavorite != nil {
		is = *p.IsFavorite
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE favorite_sessions SET is_favorite = $3,
			server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`, item.EntityID, userID, is,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityFavorite, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Course / Note ---------------------------------------------------------------

// applyCourse mirrors the other entities: delete → tombstone (plus
// nullifying course references on the user's live sessions so no session
// keeps pointing at a deleted course), upsert → insert@v1 / base==ver
// merge / base<ver conflict / base>ver rejected.
func (s *Service) applyCourse(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchCourse(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj == nil {
			// Phantom tombstone: never existed server-side.
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO courses
					(id, user_id, name, server_version, created_at, updated_at, deleted_at)
				VALUES ($1, $2, '', 1, now(), now(), now())
				RETURNING updated_at`, item.EntityID, userID,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityCourse, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj.deletedAt != nil {
			res := accepted(item, obj.serverVer, obj.updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		var version int
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			UPDATE courses
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING server_version, updated_at`, item.EntityID, userID,
		).Scan(&version, &updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityCourse, item.EntityID, "delete", version); err != nil {
			return nil, err
		}
		// Sessions survive a course deletion: their course_id is cleared
		// (they become standalone) and each bumped session is logged so
		// other devices drop the reference too.
		if err := s.detachCourseFromSessions(ctx, q, userID, item.EntityID); err != nil {
			return nil, err
		}
		// Learning material survives too — terms/cards/tasks keep their
		// rows and schedule, only the course scoping reference is cleared.
		if err := s.detachCourseFromStudyData(ctx, q, userID, item.EntityID); err != nil {
			return nil, err
		}
		res := accepted(item, version, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// upsert: title (course name) required.
	if p.Title == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		teacher, location, colorIndex, archived := "", "", 0, false
		if p.Teacher != nil {
			teacher = *p.Teacher
		}
		if p.Location != nil {
			location = *p.Location
		}
		if p.ColorIndex != nil {
			colorIndex = *p.ColorIndex
		}
		if p.IsArchived != nil {
			archived = *p.IsArchived
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO courses
				(id, user_id, name, teacher, location, color_index, is_archived,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, *p.Title, teacher, location, colorIndex, archived,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityCourse, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		// Delete-wins: deleted courses stay deleted.
		res := conflict(item, obj.serverVer, obj.updatedAt, courseRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, courseRecordJSON(obj))
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

	// Merge: non-nil incoming fields overwrite (isArchived is an explicit
	// boolean, so un-archiving is expressible).
	name, teacher, location, colorIndex, archived :=
		obj.name, obj.teacher, obj.location, obj.colorIndex, obj.isArchived
	if p.Title != nil {
		name = *p.Title
	}
	if p.Teacher != nil {
		teacher = *p.Teacher
	}
	if p.Location != nil {
		location = *p.Location
	}
	if p.ColorIndex != nil {
		colorIndex = *p.ColorIndex
	}
	if p.IsArchived != nil {
		archived = *p.IsArchived
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE courses
		SET name = $3, teacher = $4, location = $5, color_index = $6, is_archived = $7,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, name, teacher, location, colorIndex, archived,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityCourse, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// detachCourseFromSessions clears the course reference on every live
// session of the user that pointed at the deleted course, bumping each
// version and emitting an upsert change (sessions are never deleted by a
// course deletion).
func (s *Service) detachCourseFromSessions(ctx context.Context, q store.Q, userID, courseID uuid.UUID) error {
	rows, err := q.Query(ctx, `
		UPDATE classroom_sessions
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
	var sessions []bumped
	for rows.Next() {
		b := bumped{}
		if err := rows.Scan(&b.id, &b.v); err != nil {
			rows.Close()
			return err
		}
		sessions = append(sessions, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range sessions {
		if err := logChange(ctx, q, userID, EntitySession, b.id, "upsert", b.v); err != nil {
			return err
		}
	}
	return nil
}

// detachCourseFromStudyData clears the course reference on the user's
// live learning material (terms/cards/tasks) that pointed at the deleted
// course. The rows themselves are NEVER deleted by a course deletion —
// learned cards and terms outlive their course; only the scoping
// reference goes, matching the iOS-side semantics.
func (s *Service) detachCourseFromStudyData(ctx context.Context, q store.Q, userID, courseID uuid.UUID) error {
	for _, pair := range []struct{ table, entity string }{
		{"glossary_terms", EntityTerm},
		{"study_cards", EntityStudyCard},
		{"study_tasks", EntityStudyTask},
	} {
		rows, err := q.Query(ctx, `
			UPDATE `+pair.table+`
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
			if err := logChange(ctx, q, userID, pair.entity, b.id, "upsert", b.v); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyNote: user-typed note text for one session. noteText merges like
// chineseText (non-empty incoming wins — notes are never cleared to empty,
// they are deleted); the anchor follows the uuid.Nil-clears sentinel.
func (s *Service) applyNote(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchNote(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj == nil {
			// Phantom tombstone.
			sid := uuid.Nil
			if p.SessionID != nil {
				sid = *p.SessionID
			}
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO session_notes
					(id, user_id, session_id, note_text, server_version,
					 created_at, updated_at, deleted_at)
				VALUES ($1, $2, $3, '', 1, now(), now(), now())
				RETURNING updated_at`, item.EntityID, userID, sid,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityNote, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj.deletedAt != nil {
			res := accepted(item, obj.serverVer, obj.updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		var version int
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			UPDATE session_notes
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING server_version, updated_at`, item.EntityID, userID,
		).Scan(&version, &updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityNote, item.EntityID, "delete", version); err != nil {
			return nil, err
		}
		res := accepted(item, version, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// upsert: sessionId + noteText required.
	if p.SessionID == nil || p.NoteText == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	// Parent session must exist and be live, owned by the user.
	parent, err := fetchSession(ctx, q, userID, *p.SessionID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.deletedAt != nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		var anchor *uuid.UUID
		if p.AnchorEntry != nil && *p.AnchorEntry != uuid.Nil {
			anchor = p.AnchorEntry
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO session_notes
				(id, user_id, session_id, anchor_entry_id, note_text, time_offset,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, *p.SessionID, anchor, *p.NoteText, p.NoteTimeOffset,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityNote, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, noteRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, noteRecordJSON(obj))
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

	// Merge: non-empty note text wins (an empty incoming text means
	// "nothing new to say", matching chineseText rules); the anchor is
	// overwritten when present, uuid.Nil clearing it.
	text := obj.noteText
	if *p.NoteText != "" {
		text = *p.NoteText
	}
	anchor := obj.anchorEntry
	if p.AnchorEntry != nil {
		if *p.AnchorEntry == uuid.Nil {
			anchor = nil
		} else {
			anchor = p.AnchorEntry
		}
	}
	// The note's classroom-relative position: absent payload keeps the
	// stored offset (never clearable — an approximation stays better than
	// nothing), present overwrites.
	timeOffset := obj.timeOffset
	if p.NoteTimeOffset != nil {
		timeOffset = p.NoteTimeOffset
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE session_notes
		SET anchor_entry_id = $3, note_text = $4, time_offset = $5,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, anchor, text, timeOffset,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityNote, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// applyStudyReview: the post-class AI study review of one session. Entity
// id == session id (one review per session, structurally). Content merges
// like chineseText — non-empty incoming wins; there is no product flow
// that clears a non-empty review back to empty (regeneration keeps the
// previous result until the new one succeeds). Chunk-level generation
// progress never syncs — only the structured result does.
func (s *Service) applyStudyReview(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchStudyReview(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj == nil {
			// Phantom tombstone (session_id is the entity id itself).
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO study_reviews
					(id, user_id, session_id, status, server_version,
					 created_at, updated_at, deleted_at)
				VALUES ($1, $2, $3, 'failed', 1, now(), now(), now())
				RETURNING updated_at`, item.EntityID, userID, item.EntityID,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityStudyReview, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj.deletedAt != nil {
			res := accepted(item, obj.serverVer, obj.updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		var version int
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			UPDATE study_reviews
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING server_version, updated_at`, item.EntityID, userID,
		).Scan(&version, &updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityStudyReview, item.EntityID, "delete", version); err != nil {
			return nil, err
		}
		res := accepted(item, version, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// upsert: status required.
	if p.ReviewStatus == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	// The owning session must exist and be live (id == session id).
	parent, err := fetchSession(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.deletedAt != nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		content := ""
		if p.ReviewContent != nil {
			content = *p.ReviewContent
		}
		generated := ""
		if p.ReviewGenerated != nil {
			generated = *p.ReviewGenerated
		}
		reviewModel := ""
		if p.ReviewModel != nil {
			reviewModel = *p.ReviewModel
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO study_reviews
				(id, user_id, session_id, status, content, generated_content,
				 review_model, generated_at, source_updated_at,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, item.EntityID, *p.ReviewStatus,
			content, generated, reviewModel, p.ReviewGeneratedAt, p.ReviewSourceAt,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityStudyReview, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		// Delete-wins: deleted reviews stay deleted.
		res := conflict(item, obj.serverVer, obj.updatedAt, studyReviewRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyReviewRecordJSON(obj))
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

	// Merge: non-empty incoming fields win (status/content/generated/
	// model); timestamps overwrite when present.
	status := obj.status
	if *p.ReviewStatus != "" {
		status = *p.ReviewStatus
	}
	content := obj.content
	if p.ReviewContent != nil && *p.ReviewContent != "" {
		content = *p.ReviewContent
	}
	generated := obj.generated
	if p.ReviewGenerated != nil && *p.ReviewGenerated != "" {
		generated = *p.ReviewGenerated
	}
	reviewModel := obj.reviewModel
	if p.ReviewModel != nil && *p.ReviewModel != "" {
		reviewModel = *p.ReviewModel
	}
	generatedAt := obj.generatedAt
	if p.ReviewGeneratedAt != nil {
		generatedAt = p.ReviewGeneratedAt
	}
	sourceAt := obj.sourceUpdateAt
	if p.ReviewSourceAt != nil {
		sourceAt = p.ReviewSourceAt
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE study_reviews
		SET status = $3, content = $4, generated_content = $5, review_model = $6,
		    generated_at = $7, source_updated_at = $8,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, status, content, generated, reviewModel,
		generatedAt, sourceAt,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityStudyReview, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// applyAttachment: classroom image metadata + structured analysis. Files
// never travel through this path (the /v1/attachments routes carry them);
// file_size/content_hash here are the contract the upload route verifies
// against, so the row always describes its files. title/caption/kind/ocr
// merge like noteText (non-empty incoming wins — the product has no
// clear-to-empty flow; removal is a delete), the anchor and course
// reference follow the uuid.Nil-clears sentinel. Analysis status is
// always overwritten when present, the analysis object only when
// present and non-empty (a failed regeneration keeps the last good
// result, matching study review content rules).
func (s *Service) applyAttachment(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchAttachment(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj == nil {
			// Phantom tombstone.
			sid := uuid.Nil
			if p.SessionID != nil {
				sid = *p.SessionID
			}
			var updatedAt time.Time
			err := q.QueryRow(ctx, `
				INSERT INTO session_attachments
					(id, user_id, session_id, server_version, created_at, updated_at, deleted_at)
				VALUES ($1, $2, $3, 1, now(), now(), now())
				RETURNING updated_at`, item.EntityID, userID, sid,
			).Scan(&updatedAt)
			if err != nil {
				return nil, err
			}
			if err := logChange(ctx, q, userID, EntityAttachment, item.EntityID, "delete", 1); err != nil {
				return nil, err
			}
			res := accepted(item, 1, updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		if obj.deletedAt != nil {
			res := accepted(item, obj.serverVer, obj.updatedAt)
			if err := storeLedger(ctx, q, userID, item, res); err != nil {
				return nil, err
			}
			return res, nil
		}
		var version int
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			UPDATE session_attachments
			SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
			WHERE id = $1 AND user_id = $2
			RETURNING server_version, updated_at`, item.EntityID, userID,
		).Scan(&version, &updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityAttachment, item.EntityID, "delete", version); err != nil {
			return nil, err
		}
		if s.attachments != nil {
			// File cleanup runs only after the enclosing push transaction
			// commits (see withPostCommit); a rollback discards the hook.
			deferFileCleanup(ctx, func() {
				if err := s.attachments.DeleteFiles(userID, item.EntityID); err != nil {
					slog.Error("attachment file cleanup failed", "user_id", userID, "attachment_id", item.EntityID, "err", err.Error())
				}
			})
		}
		res := accepted(item, version, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	// upsert: sessionId + attachmentCapturedAt required (the capture time
	// is what orders the timeline).
	if p.SessionID == nil || p.AttachmentCapturedAt == nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	// Parent session must exist and be live, owned by the user.
	parent, err := fetchSession(ctx, q, userID, *p.SessionID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.deletedAt != nil {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		title, caption, kind, mime := "", "", "other", ""
		if p.Title != nil {
			title = *p.Title
		}
		if p.AttachmentCaption != nil {
			caption = *p.AttachmentCaption
		}
		if p.AttachmentKind != nil && *p.AttachmentKind != "" {
			kind = *p.AttachmentKind
		}
		if p.AttachmentMime != nil {
			mime = *p.AttachmentMime
		}
		var width, height, sortIndex int
		var fileSize int64
		var hash, ocr, analysisSt string
		if p.AttachmentWidth != nil {
			width = *p.AttachmentWidth
		}
		if p.AttachmentHeight != nil {
			height = *p.AttachmentHeight
		}
		if p.AttachmentFileSize != nil {
			fileSize = *p.AttachmentFileSize
		}
		if p.AttachmentHash != nil {
			hash = *p.AttachmentHash
		}
		if p.AttachmentSortIndex != nil {
			sortIndex = *p.AttachmentSortIndex
		}
		transform := ""
		if p.AttachmentTransform != nil {
			transform = *p.AttachmentTransform
		}
		if p.AttachmentOcrText != nil {
			ocr = *p.AttachmentOcrText
		}
		if p.AttachmentAnalysisState != nil && *p.AttachmentAnalysisState != "" {
			analysisSt = *p.AttachmentAnalysisState
		} else {
			analysisSt = "pending"
		}
		var analysis string
		if p.AttachmentAnalysis != nil && *p.AttachmentAnalysis != "" {
			analysis = *p.AttachmentAnalysis
		}
		// Sentinel refs: uuid.Nil never reaches the row.
		var courseID, anchor *uuid.UUID
		if p.CourseID != nil && *p.CourseID != uuid.Nil {
			courseID = p.CourseID
		}
		if p.AnchorEntry != nil && *p.AnchorEntry != uuid.Nil {
			anchor = p.AnchorEntry
		}
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO session_attachments
				(id, user_id, session_id, course_id, anchor_entry_id, captured_at,
				 title, caption, kind, mime_type, pixel_width, pixel_height,
				 file_size, content_hash, sort_index, transform_json,
				 analysis_status, analysis, ocr_text,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			        $13, $14, $15, $16, $17, NULLIF($18, '')::jsonb, $19, 1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, *p.SessionID, courseID, anchor, *p.AttachmentCapturedAt,
			title, caption, kind, mime, width, height,
			fileSize, hash, sortIndex, transform, analysisSt, analysis, ocr,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityAttachment, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		// Delete-wins: deleted attachments stay deleted.
		res := conflict(item, obj.serverVer, obj.updatedAt, attachmentRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, attachmentRecordJSON(obj))
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

	// Merge.
	title, caption, kind, mime := obj.title, obj.caption, obj.kind, obj.mime
	if p.Title != nil && *p.Title != "" {
		title = *p.Title
	}
	if p.AttachmentCaption != nil && *p.AttachmentCaption != "" {
		caption = *p.AttachmentCaption
	}
	if p.AttachmentKind != nil && *p.AttachmentKind != "" {
		kind = *p.AttachmentKind
	}
	if p.AttachmentMime != nil && *p.AttachmentMime != "" {
		mime = *p.AttachmentMime
	}
	// Immutable after creation: the files and their identity (hash, size,
	// dimensions, mime) describe the stored bytes; an edit produces a new
	// attachment. Present-but-differing values are silently ignored (iOS
	// never rewrites them after the first push).
	width, height, sortIndex := obj.width, obj.height, obj.sortIndex
	if p.AttachmentSortIndex != nil {
		sortIndex = *p.AttachmentSortIndex
	}
	courseID := obj.courseID
	if p.CourseID != nil {
		if *p.CourseID == uuid.Nil {
			courseID = nil
		} else {
			courseID = p.CourseID
		}
	}
	anchor := obj.anchorEntry
	if p.AnchorEntry != nil {
		if *p.AnchorEntry == uuid.Nil {
			anchor = nil
		} else {
			anchor = p.AnchorEntry
		}
	}
	capturedAt := obj.capturedAt
	if p.AttachmentCapturedAt != nil {
		capturedAt = *p.AttachmentCapturedAt
	}
	ocr := obj.ocrText
	if p.AttachmentOcrText != nil && *p.AttachmentOcrText != "" {
		ocr = *p.AttachmentOcrText
	}
	analysisSt := obj.analysisSt
	if p.AttachmentAnalysisState != nil && *p.AttachmentAnalysisState != "" {
		analysisSt = *p.AttachmentAnalysisState
	}
	transform := obj.transform
	if p.AttachmentTransform != nil {
		transform = *p.AttachmentTransform
	}
	analysis := obj.analysis
	if p.AttachmentAnalysis != nil && *p.AttachmentAnalysis != "" {
		analysis = []byte(*p.AttachmentAnalysis)
	}
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE session_attachments
		SET title = $3, caption = $4, kind = $5, mime_type = $6,
		    pixel_width = $7, pixel_height = $8, sort_index = $9,
		    course_id = $10, anchor_entry_id = $11, captured_at = $12,
		    ocr_text = $13, analysis_status = $14, transform_json = $15,
		    analysis = CASE WHEN $16 = '' THEN NULL ELSE $16::jsonb END,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, title, caption, kind, mime,
		width, height, sortIndex, courseID, anchor, capturedAt,
		ocr, analysisSt, transform, string(analysis),
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityAttachment, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// applyTerm: glossary term. Text fields merge like noteText (non-empty
// incoming wins — the product has no clear-to-empty flow; removal is a
// delete); favorite/status overwrite when present; source references
// follow the uuid.Nil-clears sentinel. The server does NOT enforce
// per-course term uniqueness — dedup/merge is a client-side,
// user-confirmed flow.
func (s *Service) applyTerm(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchTerm(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	// Delete path (phantom / idempotent / live), shared helper below.
	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityTerm, "glossary_terms",
			obj != nil, obj != nil && obj.deletedAt != nil, termRowView(obj))
	}

	// upsert: the Russian word is the term's identity — required.
	if p.TermRussian == nil || *p.TermRussian == "" {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		chinese, explanation, pos, note := "", "", "", ""
		if p.TermChinese != nil {
			chinese = *p.TermChinese
		}
		if p.TermExplanation != nil {
			explanation = *p.TermExplanation
		}
		if p.TermPartOfSpeech != nil {
			pos = *p.TermPartOfSpeech
		}
		if p.TermUserNote != nil {
			note = *p.TermUserNote
		}
		sourceSessions := ""
		if p.TermSourceSessions != nil {
			sourceSessions = *p.TermSourceSessions
		}
		favorite := false
		if p.TermFavorite != nil {
			favorite = *p.TermFavorite
		}
		status := "new"
		if p.TermStatus != nil && *p.TermStatus != "" {
			status = *p.TermStatus
		}
		courseID := refOrNil(p.CourseID)
		sessionID := refOrNil(p.SessionID)
		reviewID := refOrNil(p.SourceReviewID)
		entryID := refOrNil(p.EntryID)
		attachID := refOrNil(p.SourceAttachmentID)
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO glossary_terms
				(id, user_id, course_id, session_id, source_review_id,
				 source_entry_id, source_attachment_id, source_session_ids,
				 russian, chinese, explanation, part_of_speech, user_note,
				 is_favorite, status,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			        $9, $10, $11, $12, $13, $14, $15,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, courseID, sessionID, reviewID, entryID, attachID, sourceSessions,
			*p.TermRussian, chinese, explanation, pos, note, favorite, status,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityTerm, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		// Delete-wins: deleted terms stay deleted.
		res := conflict(item, obj.serverVer, obj.updatedAt, termRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, termRecordJSON(obj))
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

	// Merge: non-empty incoming text wins (user-edited explanations are
	// never silently cleared by a stale push); refs follow the sentinel.
	chinese, explanation, pos, note := obj.chinese, obj.explanation, obj.partOfSpeech, obj.userNote
	if p.TermChinese != nil && *p.TermChinese != "" {
		chinese = *p.TermChinese
	}
	if p.TermExplanation != nil && *p.TermExplanation != "" {
		explanation = *p.TermExplanation
	}
	if p.TermPartOfSpeech != nil && *p.TermPartOfSpeech != "" {
		pos = *p.TermPartOfSpeech
	}
	if p.TermUserNote != nil && *p.TermUserNote != "" {
		note = *p.TermUserNote
	}
	russian := obj.russian
	if *p.TermRussian != "" {
		russian = *p.TermRussian
	}
	sourceSessions := obj.sourceSessIDs
	if p.TermSourceSessions != nil {
		sourceSessions = *p.TermSourceSessions
	}
	favorite := obj.favorite
	if p.TermFavorite != nil {
		favorite = *p.TermFavorite
	}
	status := obj.status
	if p.TermStatus != nil && *p.TermStatus != "" {
		status = *p.TermStatus
	}
	courseID := mergeRef(obj.courseID, p.CourseID)
	sessionID := mergeRef(obj.sessionID, p.SessionID)
	reviewID := mergeRef(obj.sourceReview, p.SourceReviewID)
	entryID := mergeRef(obj.sourceEntry, p.EntryID)
	attachID := mergeRef(obj.sourceAttach, p.SourceAttachmentID)
	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE glossary_terms
		SET course_id = $3, session_id = $4, source_review_id = $5,
		    source_entry_id = $6, source_attachment_id = $7, source_session_ids = $8,
		    russian = $9, chinese = $10, explanation = $11, part_of_speech = $12,
		    user_note = $13, is_favorite = $14, status = $15,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, courseID, sessionID, reviewID, entryID, attachID, sourceSessions,
		russian, chinese, explanation, pos, note, favorite, status,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityTerm, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// applyStudyCard: flashcard content + spaced-repetition state. Content
// fields merge like term text (non-empty incoming wins). Review-state
// fields (stage/reviewCount/intervalHours/dueAt/lastReviewedAt/lastGrade)
// have their own rule: the side with the NEWER lastReviewedAt wins for
// all of them, so a device that reviewed later never has its schedule
// clobbered by one that reviewed earlier. An incoming state without
// lastReviewedAt (a pure content edit) never touches the schedule.
func (s *Service) applyStudyCard(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchStudyCard(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityStudyCard, "study_cards",
			obj != nil, obj != nil && obj.deletedAt != nil, cardRowView(obj))
	}

	// upsert: front must be present (the card's face is its identity).
	if p.CardFront == nil || *p.CardFront == "" {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		back, cardType, note, origin, stage := "", "qa", "", "manual", "new"
		var reviewCount, intervalHours int
		var dueAt, lastReviewed *time.Time
		lastGrade := ""
		if p.CardBack != nil {
			back = *p.CardBack
		}
		if p.CardType != nil && *p.CardType != "" {
			cardType = *p.CardType
		}
		if p.CardUserNote != nil {
			note = *p.CardUserNote
		}
		if p.CardOrigin != nil && *p.CardOrigin != "" {
			origin = *p.CardOrigin
		}
		if p.CardStage != nil && *p.CardStage != "" {
			stage = *p.CardStage
		}
		if p.CardReviewCount != nil {
			reviewCount = *p.CardReviewCount
		}
		if p.CardIntervalHours != nil {
			intervalHours = *p.CardIntervalHours
		}
		if p.CardDueAt != nil {
			dueAt = p.CardDueAt
		}
		if p.CardLastReviewedAt != nil {
			lastReviewed = p.CardLastReviewedAt
		}
		if p.CardLastGrade != nil {
			lastGrade = *p.CardLastGrade
		}
		courseID := refOrNil(p.CourseID)
		sessionID := refOrNil(p.SessionID)
		entryID := refOrNil(p.EntryID)
		attachID := refOrNil(p.SourceAttachmentID)
		termID := refOrNil(p.SourceTermID)
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO study_cards
				(id, user_id, course_id, session_id, source_entry_id,
				 source_attachment_id, source_term_id,
				 front, back, card_type, user_note, origin,
				 stage, review_count, interval_hours, due_at, last_reviewed_at, last_grade,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        $8, $9, $10, $11, $12,
			        $13, $14, $15, $16, $17, $18,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, courseID, sessionID, entryID, attachID, termID,
			*p.CardFront, back, cardType, note, origin,
			stage, reviewCount, intervalHours, dueAt, lastReviewed, lastGrade,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityStudyCard, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyCardRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyCardRecordJSON(obj))
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

	// Content merge.
	back, cardType, note := obj.back, obj.cardType, obj.userNote
	if p.CardBack != nil && *p.CardBack != "" {
		back = *p.CardBack
	}
	if p.CardType != nil && *p.CardType != "" {
		cardType = *p.CardType
	}
	if p.CardUserNote != nil && *p.CardUserNote != "" {
		note = *p.CardUserNote
	}
	front := obj.front
	if *p.CardFront != "" {
		front = *p.CardFront
	}
	courseID := mergeRef(obj.courseID, p.CourseID)
	sessionID := mergeRef(obj.sessionID, p.SessionID)
	entryID := mergeRef(obj.sourceEntry, p.EntryID)
	attachID := mergeRef(obj.sourceAttach, p.SourceAttachmentID)
	termID := mergeRef(obj.sourceTerm, p.SourceTermID)

	// Review-state merge: newer lastReviewedAt wins wholesale.
	stage, reviewCount, intervalHours, dueAt, lastReviewed, lastGrade :=
		obj.stage, obj.reviewCount, obj.intervalHours, obj.dueAt, obj.lastReviewed, obj.lastGrade
	if p.CardLastReviewedAt != nil &&
		(lastReviewed == nil || !p.CardLastReviewedAt.Before(*lastReviewed)) {
		if p.CardStage != nil && *p.CardStage != "" {
			stage = *p.CardStage
		}
		if p.CardReviewCount != nil {
			reviewCount = *p.CardReviewCount
		}
		if p.CardIntervalHours != nil {
			intervalHours = *p.CardIntervalHours
		}
		dueAt = p.CardDueAt
		lastReviewed = p.CardLastReviewedAt
		if p.CardLastGrade != nil {
			lastGrade = *p.CardLastGrade
		}
	}
	// A pure content edit (no review timestamps) resets nothing — the
	// stored schedule stands. A stale review (older lastReviewedAt) is
	// ignored: the local device keeps the newer schedule.

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE study_cards
		SET course_id = $3, session_id = $4, source_entry_id = $5,
		    source_attachment_id = $6, source_term_id = $7,
		    front = $8, back = $9, card_type = $10, user_note = $11,
		    stage = $12, review_count = $13, interval_hours = $14,
		    due_at = $15, last_reviewed_at = $16, last_grade = $17,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, courseID, sessionID, entryID, attachID, termID,
		front, back, cardType, note,
		stage, reviewCount, intervalHours, dueAt, lastReviewed, lastGrade,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityStudyCard, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// applyStudyTask: assignment/todo. Title rides the shared payload field.
// Status merges with a sticky-done rule: once a task is completed, an
// incoming non-done status cannot resurrect it server-side — a stale
// device must go through the conflict path to reopen a task. Uncertainty
// and origin are AI-provenance metadata (non-empty wins); completed_at
// follows the status.
func (s *Service) applyStudyTask(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchStudyTask(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityStudyTask, "study_tasks",
			obj != nil, obj != nil && obj.deletedAt != nil, taskRowView(obj))
	}

	// upsert: the title is required.
	title := ""
	if p.Title != nil {
		title = *p.Title
	}
	if title == "" {
		res := rejected(item, "schema")
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj == nil {
		detail, priority, status, origin, uncertainty, note := "", "normal", "pending", "manual", "", ""
		var dueAt, completedAt *time.Time
		if p.TaskDetail != nil {
			detail = *p.TaskDetail
		}
		if p.TaskDueAt != nil {
			dueAt = p.TaskDueAt
		}
		if p.TaskPriority != nil && *p.TaskPriority != "" {
			priority = *p.TaskPriority
		}
		if p.TaskStatus != nil && *p.TaskStatus != "" {
			status = *p.TaskStatus
		}
		if p.TaskOrigin != nil && *p.TaskOrigin != "" {
			origin = *p.TaskOrigin
		}
		if p.TaskUncertainty != nil {
			uncertainty = *p.TaskUncertainty
		}
		if p.TaskUserNote != nil {
			note = *p.TaskUserNote
		}
		if p.TaskCompletedAt != nil {
			completedAt = p.TaskCompletedAt
		}
		courseID := refOrNil(p.CourseID)
		sessionID := refOrNil(p.SessionID)
		reviewID := refOrNil(p.SourceReviewID)
		entryID := refOrNil(p.EntryID)
		attachID := refOrNil(p.SourceAttachmentID)
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO study_tasks
				(id, user_id, course_id, session_id, source_review_id,
				 source_entry_id, source_attachment_id,
				 title, detail, due_at, priority, status, origin,
				 uncertainty, user_note, completed_at,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        $8, $9, $10, $11, $12, $13,
			        $14, $15, $16,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, courseID, sessionID, reviewID, entryID, attachID,
			title, detail, dueAt, priority, status, origin,
			uncertainty, note, completedAt,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityStudyTask, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyTaskRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyTaskRecordJSON(obj))
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

	// Content merge.
	detail, note := obj.detail, obj.userNote
	if p.TaskDetail != nil && *p.TaskDetail != "" {
		detail = *p.TaskDetail
	}
	if p.TaskUserNote != nil && *p.TaskUserNote != "" {
		note = *p.TaskUserNote
	}
	// title was validated non-empty above.
	priority, origin, uncertainty := obj.priority, obj.origin, obj.uncertainty
	if p.TaskPriority != nil && *p.TaskPriority != "" {
		priority = *p.TaskPriority
	}
	if p.TaskOrigin != nil && *p.TaskOrigin != "" {
		origin = *p.TaskOrigin
	}
	if p.TaskUncertainty != nil && *p.TaskUncertainty != "" {
		uncertainty = *p.TaskUncertainty
	}
	dueAt := obj.dueAt
	if p.TaskDueAt != nil {
		dueAt = p.TaskDueAt
	}
	courseID := mergeRef(obj.courseID, p.CourseID)
	sessionID := mergeRef(obj.sessionID, p.SessionID)
	reviewID := mergeRef(obj.sourceReview, p.SourceReviewID)
	entryID := mergeRef(obj.sourceEntry, p.EntryID)
	attachID := mergeRef(obj.sourceAttach, p.SourceAttachmentID)

	// Status merge with sticky-done.
	status := obj.status
	completedAt := obj.completedAt
	if p.TaskStatus != nil && *p.TaskStatus != "" && *p.TaskStatus != obj.status {
		if obj.status == "done" && *p.TaskStatus != "done" {
			// Sticky done: a stale device cannot reopen a completed task
			// through the merge path (it conflicts first anyway); keep
			// done + completed_at.
		} else {
			status = *p.TaskStatus
			if status == "done" {
				if p.TaskCompletedAt != nil {
					completedAt = p.TaskCompletedAt
				} else if completedAt == nil {
					now := time.Now()
					completedAt = &now
				}
			} else {
				completedAt = nil
			}
		}
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE study_tasks
		SET course_id = $3, session_id = $4, source_review_id = $5,
		    source_entry_id = $6, source_attachment_id = $7,
		    title = $8, detail = $9, due_at = $10, priority = $11,
		    status = $12, origin = $13, uncertainty = $14, user_note = $15,
		    completed_at = $16,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, courseID, sessionID, reviewID, entryID, attachID,
		title, detail, dueAt, priority, status, origin, uncertainty, note,
		completedAt,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityStudyTask, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// learningRowView is the minimal projection applyLearningDelete needs
// from a fetched row (nil-safe: every helper below accepts a nil row).
type learningRowView struct {
	serverVer int
	updatedAt time.Time
}

func termRowView(t *termRow) learningRowView {
	if t == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: t.serverVer, updatedAt: t.updatedAt}
}

func cardRowView(c *studyCardRow) learningRowView {
	if c == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: c.serverVer, updatedAt: c.updatedAt}
}

func taskRowView(t *studyTaskRow) learningRowView {
	if t == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: t.serverVer, updatedAt: t.updatedAt}
}

// applyLearningDelete handles the delete path shared by the learning
// entities (term/card/task): a phantom tombstone for a row the server
// has never seen (all non-key columns carry defaults, so a stub insert
// works), an idempotent re-delete of an already-tombstoned row, and the
// live delete (bump version, log change). Sessions and courses are NOT
// cascaded here — learning material survives its sources; course deletes
// clear the reference instead (detachCourseFromStudyData).
func (s *Service) applyLearningDelete(
	ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem,
	entity, table string, exists, deleted bool, row learningRowView,
) (*PushItemResult, error) {
	if !exists {
		// Phantom tombstone: never existed server-side; record the delete
		// so offline devices learn about it.
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO `+table+`
				(id, user_id, server_version, created_at, updated_at, deleted_at)
			VALUES ($1, $2, 1, now(), now(), now())
			RETURNING updated_at`, item.EntityID, userID,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, entity, item.EntityID, "delete", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if deleted {
		res := accepted(item, row.serverVer, row.updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	var version int
	var updatedAt time.Time
	err := q.QueryRow(ctx, `
		UPDATE `+table+`
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`, item.EntityID, userID,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, entity, item.EntityID, "delete", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Pull -------------------------------------------------------------------------

func (s *Service) Pull(ctx context.Context, userID uuid.UUID, cursor int64, limit int) (*PullResponse, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	q := s.db.Q()
	rs, err := q.Query(ctx, `
		SELECT change_sequence, entity_type, entity_id, operation, server_version
		FROM sync_changes
		WHERE user_id = $1 AND change_sequence > $2
		ORDER BY change_sequence ASC
		LIMIT $3`, userID, cursor, limit+1)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	type changeRow struct {
		seq     int64
		etype   string
		eid     uuid.UUID
		op      string
		version int
	}
	var rows []changeRow
	for rs.Next() {
		r := changeRow{}
		if err := rs.Scan(&r.seq, &r.etype, &r.eid, &r.op, &r.version); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	resp := &PullResponse{
		SchemaVersion: s.cfg.SchemaVersion,
		HasMore:       hasMore,
		ServerTime:    time.Now(),
		Changes:       []PullChange{},
	}
	for _, r := range rows {
		change := PullChange{
			ChangeSequence: r.seq,
			EntityType:     r.etype,
			EntityID:       r.eid.String(),
			Operation:      r.op,
			ServerVersion:  r.version,
		}
		if r.op == "upsert" {
			rec, err := s.loadRecord(ctx, q, userID, r.etype, r.eid)
			if err != nil {
				return nil, err
			}
			change.Record = rec
		}
		resp.Changes = append(resp.Changes, change)
	}
	if len(rows) > 0 {
		resp.NextCursor = rows[len(rows)-1].seq
	} else {
		resp.NextCursor = cursor
	}
	return resp, nil
}

func (s *Service) loadRecord(ctx context.Context, q store.Q, userID uuid.UUID, entityType string, id uuid.UUID) (json.RawMessage, error) {
	switch entityType {
	case EntitySession:
		obj, err := fetchSession(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return sessionRecordJSON(obj), nil
	case EntityEntry:
		obj, err := fetchEntry(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return entryRecordJSON(obj), nil
	case EntityBookmark:
		obj, err := fetchBookmark(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return bookmarkRecordJSON(obj), nil
	case EntityFavorite:
		obj, err := fetchFavorite(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return favoriteRecordJSON(obj), nil
	case EntityCourse:
		obj, err := fetchCourse(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return courseRecordJSON(obj), nil
	case EntityNote:
		obj, err := fetchNote(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return noteRecordJSON(obj), nil
	case EntityStudyReview:
		obj, err := fetchStudyReview(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return studyReviewRecordJSON(obj), nil
	case EntityAttachment:
		obj, err := fetchAttachment(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return attachmentRecordJSON(obj), nil
	case EntityTerm:
		obj, err := fetchTerm(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return termRecordJSON(obj), nil
	case EntityStudyCard:
		obj, err := fetchStudyCard(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return studyCardRecordJSON(obj), nil
	case EntityStudyTask:
		obj, err := fetchStudyTask(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return studyTaskRecordJSON(obj), nil
	case EntityTranscriptCorrection:
		obj, err := fetchCorrection(ctx, q, userID, id)
		if err != nil || obj == nil {
			return nil, err
		}
		return correctionRecordJSON(obj), nil
	}
	return nil, nil
}

// --- Status -----------------------------------------------------------------------

func (s *Service) Status(ctx context.Context, userID uuid.UUID) (*SyncStatusResponse, error) {
	q := s.db.Q()
	var tail, sessions, entries, courses, notes, reviews, attachments, terms, cards, tasks, corrections int
	err := q.QueryRow(ctx, `
		SELECT
			COALESCE((SELECT max(change_sequence) FROM sync_changes WHERE user_id = $1), 0),
			(SELECT count(*) FROM classroom_sessions WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM transcript_entries WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM courses WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM session_notes WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM study_reviews WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM session_attachments WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM glossary_terms WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM study_cards WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM study_tasks WHERE user_id = $1 AND deleted_at IS NULL),
			(SELECT count(*) FROM transcript_corrections WHERE user_id = $1 AND deleted_at IS NULL)`,
		userID).Scan(&tail, &sessions, &entries, &courses, &notes, &reviews, &attachments, &terms, &cards, &tasks, &corrections)
	if err != nil {
		return nil, err
	}
	return &SyncStatusResponse{
		SchemaVersion:             s.cfg.SchemaVersion,
		MinClientSchemaVersion:    s.cfg.MinClientSchemaVersion,
		MaxClientSchemaVersion:    s.cfg.MaxClientSchemaVersion,
		ChangeLogTail:             int64(tail),
		SessionCount:              sessions,
		EntryCount:                entries,
		CourseCount:               courses,
		NoteCount:                 notes,
		ReviewCount:               reviews,
		AttachmentCount:           attachments,
		TermCount:                 terms,
		StudyCardCount:            cards,
		StudyTaskCount:            tasks,
		TranscriptCorrectionCount: corrections,
		PendingCount:              0,
		ServerTime:                time.Now(),
	}, nil
}
