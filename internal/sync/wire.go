// Package sync implements the /v1/sync protocol: push (idempotent,
// optimistic-concurrency batch writes), pull (change-log cursor) and
// status. Semantics are ported 1:1 from the Python service — the iOS
// client must see identical JSON, error codes and ordering.
//
// Transaction model (stronger than the Python original): each push BATCH
// runs in one transaction — entity writes, server_version bumps,
// sync_changes rows and processed_operations ledger entries commit
// together or not at all.
package sync

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Wire types. Field names are camelCase on the wire to match iOS Codable
// and the Python Pydantic schemas; extra="forbid" semantics are relaxed to
// ignore-unknown for forward tolerance EXCEPT where the Python service
// rejected (it used forbid on payload/items — we keep strict validation
// via explicit checks, not decode errors, so error shapes stay JSON).

type DeviceInfo struct {
	ClientDeviceID string `json:"clientDeviceId"`
	DisplayName    string `json:"displayName"`
	AppVersion     string `json:"appVersion"`
}

type TokenPair struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int    `json:"expiresIn"`
	UserID       string `json:"userId"`
	IsNewUser    bool   `json:"isNewUser,omitempty"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type LogoutRequest struct {
	RefreshToken string `json:"refreshToken"`
	RevokeDevice bool   `json:"revokeDevice"`
}

type MeResponse struct {
	UserID       string    `json:"userId"`
	DisplayLabel string    `json:"displayLabel"`
	CreatedAt    time.Time `json:"createdAt"`
}

type PublicUser struct {
	UserID        string  `json:"userId"`
	Email         *string `json:"email,omitempty"`
	DisplayName   string  `json:"displayName"`
	Status        string  `json:"status"`
	EmailVerified bool    `json:"emailVerified"`
}

// --- Push -------------------------------------------------------------------

type PushPayload struct {
	Title               *string    `json:"title"`
	StartedAt           *time.Time `json:"startedAt"`
	EndedAt             *time.Time `json:"endedAt"`
	Duration            *float64   `json:"duration"`
	SessionStatus       *string    `json:"sessionStatus"`
	AbnormalTermination *bool      `json:"abnormalTermination"`
	SourceLanguage      *string    `json:"sourceLanguage"`
	TargetLanguage      *string    `json:"targetLanguage"`
	SessionID           *uuid.UUID `json:"sessionId"`
	SequenceID          *int       `json:"sequenceId"`
	StartOffset         *float64   `json:"startOffset"`
	EndOffset           *float64   `json:"endOffset"`
	RussianText         *string    `json:"russianText"`
	ChineseText         *string    `json:"chineseText"`
	TranslationStatus   *string    `json:"translationStatus"`
	EntryID             *uuid.UUID `json:"entryId"`
	IsBookmarked        *bool      `json:"isBookmarked"`
	IsFavorite          *bool      `json:"isFavorite"`
	// session → course reference. Absent (nil) keeps the stored value;
	// uuid.Nil explicitly CLEARS it (a nil UUID is never a real course id).
	CourseID *uuid.UUID `json:"courseId"`
	// course fields (name rides on Title).
	Teacher     *string    `json:"teacher"`
	Location    *string    `json:"location"`
	ColorIndex  *int       `json:"colorIndex"`
	IsArchived  *bool      `json:"isArchived"`
	NoteText    *string    `json:"noteText"`
	AnchorEntry *uuid.UUID `json:"anchorEntryId"`
	// Note's classroom-relative position (live time or playback position
	// when it was taken). Absent keeps the stored value; NULL never rides
	// the wire (absence = keep; the client cannot clear an offset).
	NoteTimeOffset *float64 `json:"noteTimeOffset"`
	// study review (entity id == session id; status of the review itself).
	ReviewStatus      *string    `json:"reviewStatus"`
	ReviewContent     *string    `json:"reviewContent"`
	ReviewGenerated   *string    `json:"reviewGeneratedContent"`
	ReviewModel       *string    `json:"reviewModel"`
	ReviewGeneratedAt *time.Time `json:"reviewGeneratedAt"`
	ReviewSourceAt    *time.Time `json:"reviewSourceUpdatedAt"`
	// session attachment (classroom image). title rides Title; caption is
	// the user's own description; anchorEntryId/courseId reuse the shared
	// reference fields with the uuid.Nil-clears sentinel. attachmentAnalysis
	// is the versioned structured multimodal result as a JSON STRING (the
	// same convention as reviewContent — the row's analysis column casts it
	// into JSONB); attachmentOcrText is the separate local OCR text.
	AttachmentKind       *string    `json:"attachmentKind"`
	AttachmentMime       *string    `json:"attachmentMime"`
	AttachmentWidth      *int       `json:"attachmentWidth"`
	AttachmentHeight     *int       `json:"attachmentHeight"`
	AttachmentFileSize   *int64     `json:"attachmentFileSize"`
	AttachmentHash       *string    `json:"attachmentHash"`
	AttachmentCapturedAt *time.Time `json:"attachmentCapturedAt"`
	AttachmentCaption    *string    `json:"attachmentCaption"`
	AttachmentSortIndex  *int       `json:"attachmentSortIndex"`
	// Transcript entry time provenance: "audio" (recorded sample
	// timeline) or "legacy" (older app version, unmarked). Absent keeps
	// the stored value.
	TimeSource *string `json:"timeSource"`
	// Transcript correction (entity id == entry id; corrected texts ride
	// their own fields — the entry's model output stays immutable).
	// CorrectionChinese follows nil-vs-empty semantics: nil = the user
	// never corrected the Chinese; "" = a deliberate blank.
	CorrectionRussian            *string    `json:"correctionRussian"`
	CorrectionChinese            *string    `json:"correctionChinese"`
	CorrectionModifiedAt         *time.Time `json:"correctionModifiedAt"`
	CorrectionNeedsRetranslation *bool      `json:"correctionNeedsRetranslation"`
	// Learning entities (review center): shared reference fields ride
	// sessionId/courseId/entryId plus the sourceAttachmentId/sourceReviewId/
	// sourceTermId fields below; task title rides Title (course/attachment
	// convention). termSourceSessions is a JSON array of session UUID
	// strings (the term's accumulated classroom sources) — string-in
	// convention, same as analysis/reviewContent.
	TermRussian        *string    `json:"termRussian"`
	TermChinese        *string    `json:"termChinese"`
	TermExplanation    *string    `json:"termExplanation"`
	TermPartOfSpeech   *string    `json:"termPartOfSpeech"`
	TermUserNote       *string    `json:"termUserNote"`
	TermSourceSessions *string    `json:"termSourceSessions"`
	TermFavorite       *bool      `json:"termFavorite"`
	TermStatus         *string    `json:"termStatus"`
	CardFront          *string    `json:"cardFront"`
	CardBack           *string    `json:"cardBack"`
	CardType           *string    `json:"cardType"`
	CardUserNote       *string    `json:"cardUserNote"`
	CardOrigin         *string    `json:"cardOrigin"`
	CardStage          *string    `json:"cardStage"`
	CardReviewCount    *int       `json:"cardReviewCount"`
	CardIntervalHours  *int       `json:"cardIntervalHours"`
	CardDueAt          *time.Time `json:"cardDueAt"`
	CardLastReviewedAt *time.Time `json:"cardLastReviewedAt"`
	CardLastGrade      *string    `json:"cardLastGrade"`
	TaskDetail         *string    `json:"taskDetail"`
	TaskDueAt          *time.Time `json:"taskDueAt"`
	TaskPriority       *string    `json:"taskPriority"`
	TaskStatus         *string    `json:"taskStatus"`
	TaskOrigin         *string    `json:"taskOrigin"`
	TaskUncertainty    *string    `json:"taskUncertainty"`
	TaskUserNote       *string    `json:"taskUserNote"`
	TaskCompletedAt    *time.Time `json:"taskCompletedAt"`
	// Shared source references for the learning entities (term/card/task).
	// Absent (nil) keeps the stored value; uuid.Nil explicitly CLEARS it.
	SourceAttachmentID *uuid.UUID `json:"sourceAttachmentId"`
	SourceReviewID     *uuid.UUID `json:"sourceReviewId"`
	SourceTermID       *uuid.UUID `json:"sourceTermId"`
	// attachmentTransform is the non-destructive display transform
	// (rotation + normalized crop) as a JSON string. Absent = keep; the
	// original file bytes are never modified server-side.
	AttachmentTransform     *string `json:"attachmentTransform"`
	AttachmentAnalysisState *string `json:"attachmentAnalysisStatus"`
	AttachmentAnalysis      *string `json:"attachmentAnalysis"`
	AttachmentOcrText       *string `json:"attachmentOcrText"`
}

type PushItem struct {
	OperationID     uuid.UUID   `json:"operationId"`
	EntityType      string      `json:"entityType"`
	EntityID        uuid.UUID   `json:"entityId"`
	Operation       string      `json:"operation"`
	BaseVersion     int         `json:"baseVersion"`
	ClientUpdatedAt time.Time   `json:"clientUpdatedAt"`
	Payload         PushPayload `json:"payload"`
}

type PushRequest struct {
	SchemaVersion int        `json:"schemaVersion"`
	Operations    []PushItem `json:"operations"`
}

type PushItemResult struct {
	OperationID     uuid.UUID       `json:"operationId"`
	Status          string          `json:"status"` // accepted|conflict|rejected|retryable_error
	ServerVersion   *int            `json:"serverVersion,omitempty"`
	ServerUpdatedAt *time.Time      `json:"serverUpdatedAt,omitempty"`
	ErrorCode       *string         `json:"errorCode,omitempty"`
	ServerRecord    json.RawMessage `json:"serverRecord,omitempty"`
}

type PushResponse struct {
	SchemaVersion int              `json:"schemaVersion"`
	Results       []PushItemResult `json:"results"`
	ServerTime    time.Time        `json:"serverTime"`
}

// --- Pull -------------------------------------------------------------------

type PullChange struct {
	ChangeSequence int64           `json:"changeSequence"`
	EntityType     string          `json:"entityType"`
	EntityID       string          `json:"entityId"`
	Operation      string          `json:"operation"`
	ServerVersion  int             `json:"serverVersion"`
	Record         json.RawMessage `json:"record"`
}

type PullResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	Changes       []PullChange `json:"changes"`
	NextCursor    int64        `json:"nextCursor"`
	HasMore       bool         `json:"hasMore"`
	ServerTime    time.Time    `json:"serverTime"`
}

type SyncStatusResponse struct {
	SchemaVersion             int       `json:"schemaVersion"`
	MinClientSchemaVersion    int       `json:"minClientSchemaVersion"`
	MaxClientSchemaVersion    int       `json:"maxClientSchemaVersion"`
	ChangeLogTail             int64     `json:"changeLogTail"`
	SessionCount              int       `json:"sessionCount"`
	EntryCount                int       `json:"entryCount"`
	CourseCount               int       `json:"courseCount"`
	NoteCount                 int       `json:"noteCount"`
	ReviewCount               int       `json:"reviewCount"`
	AttachmentCount           int       `json:"attachmentCount"`
	TermCount                 int       `json:"termCount"`
	StudyCardCount            int       `json:"studyCardCount"`
	StudyTaskCount            int       `json:"studyTaskCount"`
	TranscriptCorrectionCount int       `json:"transcriptCorrectionCount"`
	PendingCount              int       `json:"pendingCount"`
	ServerTime                time.Time `json:"serverTime"`
}

// Entity constants (wire strings). "attachment" (11 chars) stays within
// the VARCHAR(16) entity_type columns of sync_changes/processed_operations.
const (
	EntitySession     = "session"
	EntityEntry       = "entry"
	EntityBookmark    = "bookmark"
	EntityFavorite    = "favorite"
	EntityCourse      = "course"
	EntityNote        = "note"
	EntityStudyReview = "study_review"
	EntityAttachment  = "attachment"
	EntityTerm        = "term"
	EntityStudyCard   = "study_card"
	EntityStudyTask   = "study_task"
	// 21 chars — fits the VARCHAR(32) entity_type columns widened by
	// migration 00008.
	EntityTranscriptCorrection = "transcript_correction"
)

func validEntityType(t string) bool {
	return t == EntitySession || t == EntityEntry ||
		t == EntityBookmark || t == EntityFavorite ||
		t == EntityCourse || t == EntityNote ||
		t == EntityStudyReview || t == EntityAttachment ||
		t == EntityTerm || t == EntityStudyCard || t == EntityStudyTask ||
		t == EntityTranscriptCorrection
}

// Record JSON builders — the exact shapes iOS SyncServerRecordDTO decodes.
type sessionRecord struct {
	EntityType          string     `json:"entityType"`
	ID                  string     `json:"id"`
	Title               string     `json:"title"`
	StartedAt           *time.Time `json:"startedAt"`
	EndedAt             *time.Time `json:"endedAt"`
	Duration            float64    `json:"duration"`
	SessionStatus       string     `json:"sessionStatus"`
	AbnormalTermination bool       `json:"abnormalTermination"`
	SourceLanguage      string     `json:"sourceLanguage,omitempty"`
	TargetLanguage      string     `json:"targetLanguage,omitempty"`
	// Nil (no course) omits the key; the client treats absence as
	// "standalone session".
	CourseID      *string `json:"courseId,omitempty"`
	ServerVersion int     `json:"serverVersion"`
	Deleted       bool    `json:"deleted"`
}

type entryRecord struct {
	EntityType        string  `json:"entityType"`
	ID                string  `json:"id"`
	SessionID         string  `json:"sessionId"`
	SequenceID        int     `json:"sequenceId"`
	StartOffset       float64 `json:"startOffset"`
	EndOffset         float64 `json:"endOffset"`
	RussianText       string  `json:"russianText"`
	ChineseText       *string `json:"chineseText"`
	TranslationStatus string  `json:"translationStatus"`
	// "audio" | "legacy" (omitted when empty for wire compatibility).
	TimeSource    string `json:"timeSource,omitempty"`
	ServerVersion int    `json:"serverVersion"`
	Deleted       bool   `json:"deleted"`
}

type bookmarkRecord struct {
	EntityType    string `json:"entityType"`
	ID            string `json:"id"`
	SessionID     string `json:"sessionId"`
	EntryID       string `json:"entryId"`
	IsBookmarked  bool   `json:"isBookmarked"`
	ServerVersion int    `json:"serverVersion"`
	Deleted       bool   `json:"deleted"`
}

type favoriteRecord struct {
	EntityType    string `json:"entityType"`
	ID            string `json:"id"`
	SessionID     string `json:"sessionId"`
	IsFavorite    bool   `json:"isFavorite"`
	ServerVersion int    `json:"serverVersion"`
	Deleted       bool   `json:"deleted"`
}

type courseRecord struct {
	EntityType    string `json:"entityType"`
	ID            string `json:"id"`
	Title         string `json:"title"`
	Teacher       string `json:"teacher"`
	Location      string `json:"location"`
	ColorIndex    int    `json:"colorIndex"`
	IsArchived    bool   `json:"isArchived"`
	ServerVersion int    `json:"serverVersion"`
	Deleted       bool   `json:"deleted"`
}

type noteRecord struct {
	EntityType    string  `json:"entityType"`
	ID            string  `json:"id"`
	SessionID     string  `json:"sessionId"`
	AnchorEntryID *string `json:"anchorEntryId,omitempty"`
	NoteText      string  `json:"noteText"`
	// Classroom-relative position when the note was taken (omitted when
	// the note has none — the client falls back to createdAt-relative).
	TimeOffset    *float64 `json:"noteTimeOffset,omitempty"`
	ServerVersion int      `json:"serverVersion"`
	Deleted       bool     `json:"deleted"`
}

type studyReviewRecord struct {
	EntityType string `json:"entityType"`
	ID         string `json:"id"`
	SessionID  string `json:"sessionId"`
	// Field names mirror the push payload so iOS decodes records and
	// conflict payloads with the same CodingKeys.
	Status           string     `json:"reviewStatus"`
	Content          string     `json:"reviewContent"`
	GeneratedContent string     `json:"reviewGeneratedContent"`
	ReviewModel      string     `json:"reviewModel"`
	GeneratedAt      *time.Time `json:"reviewGeneratedAt,omitempty"`
	SourceUpdatedAt  *time.Time `json:"reviewSourceUpdatedAt,omitempty"`
	ServerVersion    int        `json:"serverVersion"`
	Deleted          bool       `json:"deleted"`
}

type attachmentRecord struct {
	EntityType string `json:"entityType"`
	ID         string `json:"id"`
	SessionID  string `json:"sessionId"`
	// Nil omits the key (standalone / unanchored), matching session/note.
	CourseID      *string `json:"courseId,omitempty"`
	AnchorEntryID *string `json:"anchorEntryId,omitempty"`
	// Field names mirror the push payload (attachmentXxx / title) so iOS
	// decodes records and conflict payloads with the same CodingKeys.
	// Analysis is a JSON string (see PushPayload.AttachmentAnalysis).
	CapturedAt    time.Time `json:"attachmentCapturedAt"`
	Title         string    `json:"title"`
	Caption       string    `json:"attachmentCaption"`
	Kind          string    `json:"attachmentKind"`
	MimeType      string    `json:"attachmentMime"`
	Width         int       `json:"attachmentWidth"`
	Height        int       `json:"attachmentHeight"`
	FileSize      int64     `json:"attachmentFileSize"`
	ContentHash   string    `json:"attachmentHash"`
	SortIndex     int       `json:"attachmentSortIndex"`
	Transform     string    `json:"attachmentTransform,omitempty"`
	AnalysisState string    `json:"attachmentAnalysisStatus"`
	Analysis      *string   `json:"attachmentAnalysis,omitempty"`
	OcrText       string    `json:"attachmentOcrText,omitempty"`
	ServerVersion int       `json:"serverVersion"`
	Deleted       bool      `json:"deleted"`
}

type termRecord struct {
	EntityType string `json:"entityType"`
	ID         string `json:"id"`
	// Nil omits the key (no source / unscoped), matching session/note.
	CourseID      *string `json:"courseId,omitempty"`
	SessionID     *string `json:"sessionId,omitempty"`
	SourceReview  *string `json:"sourceReviewId,omitempty"`
	SourceEntry   *string `json:"sourceEntryId,omitempty"`
	SourceAttach  *string `json:"sourceAttachmentId,omitempty"`
	SourceSessIDs string  `json:"termSourceSessions,omitempty"`
	// Field names mirror the push payload so iOS decodes records and
	// conflict payloads with the same CodingKeys.
	Russian       string `json:"termRussian"`
	Chinese       string `json:"termChinese"`
	Explanation   string `json:"termExplanation"`
	PartOfSpeech  string `json:"termPartOfSpeech"`
	UserNote      string `json:"termUserNote"`
	Favorite      bool   `json:"termFavorite"`
	Status        string `json:"termStatus"`
	ServerVersion int    `json:"serverVersion"`
	Deleted       bool   `json:"deleted"`
}

type studyCardRecord struct {
	EntityType    string     `json:"entityType"`
	ID            string     `json:"id"`
	CourseID      *string    `json:"courseId,omitempty"`
	SessionID     *string    `json:"sessionId,omitempty"`
	SourceEntry   *string    `json:"sourceEntryId,omitempty"`
	SourceAttach  *string    `json:"sourceAttachmentId,omitempty"`
	SourceTerm    *string    `json:"sourceTermId,omitempty"`
	Front         string     `json:"cardFront"`
	Back          string     `json:"cardBack"`
	CardType      string     `json:"cardType"`
	UserNote      string     `json:"cardUserNote"`
	Origin        string     `json:"cardOrigin"`
	Stage         string     `json:"cardStage"`
	ReviewCount   int        `json:"cardReviewCount"`
	IntervalHrs   int        `json:"cardIntervalHours"`
	DueAt         *time.Time `json:"cardDueAt,omitempty"`
	LastReviewed  *time.Time `json:"cardLastReviewedAt,omitempty"`
	LastGrade     string     `json:"cardLastGrade"`
	ServerVersion int        `json:"serverVersion"`
	Deleted       bool       `json:"deleted"`
}

type studyTaskRecord struct {
	EntityType string `json:"entityType"`
	ID         string `json:"id"`
	// Title rides the shared "title" key (course/attachment convention).
	Title         string     `json:"title"`
	CourseID      *string    `json:"courseId,omitempty"`
	SessionID     *string    `json:"sessionId,omitempty"`
	SourceReview  *string    `json:"sourceReviewId,omitempty"`
	SourceEntry   *string    `json:"sourceEntryId,omitempty"`
	SourceAttach  *string    `json:"sourceAttachmentId,omitempty"`
	Detail        string     `json:"taskDetail"`
	DueAt         *time.Time `json:"taskDueAt,omitempty"`
	Priority      string     `json:"taskPriority"`
	Status        string     `json:"taskStatus"`
	Origin        string     `json:"taskOrigin"`
	Uncertainty   string     `json:"taskUncertainty"`
	UserNote      string     `json:"taskUserNote"`
	CompletedAt   *time.Time `json:"taskCompletedAt,omitempty"`
	ServerVersion int        `json:"serverVersion"`
	Deleted       bool       `json:"deleted"`
}

// transcriptCorrectionRecord: field names mirror the push payload (the
// correctionXxx family) so iOS decodes records and conflict payloads with
// the same CodingKeys. CorrectionChinese preserves nil-vs-empty.
type transcriptCorrectionRecord struct {
	EntityType    string    `json:"entityType"`
	ID            string    `json:"id"`
	SessionID     string    `json:"sessionId"`
	RussianText   string    `json:"correctionRussian"`
	ChineseText   *string   `json:"correctionChinese,omitempty"`
	ModifiedAt    time.Time `json:"correctionModifiedAt"`
	NeedsRetrans  bool      `json:"correctionNeedsRetranslation"`
	ServerVersion int       `json:"serverVersion"`
	Deleted       bool      `json:"deleted"`
}
