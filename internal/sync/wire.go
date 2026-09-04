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
	// Course schedule (recurring rule). The course reference rides the
	// shared CourseID sentinel field above (nil keeps; uuid.Nil clears).
	// Times are wall-clock seconds since midnight in the course timezone;
	// dates are YYYY-MM-DD strings (DATE columns). The timezone id itself
	// travels as ScheduleTimezone. Reminder lead: -1 none | 0 at start |
	// >0 minutes before start. Absent pointer = keep stored value.
	ScheduleWeekday       *int    `json:"scheduleWeekday"`
	ScheduleStartSecs     *int    `json:"scheduleStartSecs"`
	ScheduleEndSecs       *int    `json:"scheduleEndSecs"`
	ScheduleRecurrence    *string `json:"scheduleRecurrence"`
	ScheduleParityAnchor  *string `json:"scheduleParityAnchor"`
	ScheduleFirstWeekOdd  *bool   `json:"scheduleFirstWeekIsOdd"`
	ScheduleSemesterStart *string `json:"scheduleSemesterStart"`
	ScheduleSemesterEnd   *string `json:"scheduleSemesterEnd"`
	ScheduleTimezone      *string `json:"scheduleTimezone"`
	ScheduleTeacher       *string `json:"scheduleTeacher"`
	ScheduleLocation      *string `json:"scheduleLocation"`
	ScheduleNote          *string `json:"scheduleNote"`
	ScheduleReminderMins  *int    `json:"scheduleReminderMins"`
	ScheduleEnabled       *bool   `json:"scheduleEnabled"`
	ScheduleOnceDate      *string `json:"scheduleOnceDate"`
	// Session schedule linkage: the occurrence key and planned start of
	// the class a session belongs to. Absent keeps stored (historical
	// attribution never changes once set; a deleted schedule leaves the
	// session's key dangling by design).
	ScheduleOccurrenceKey *string    `json:"scheduleOccurrenceKey"`
	SchedulePlannedStart  *time.Time `json:"schedulePlannedStart"`
	// Shared schedule reference: the schedule that owns an exception, and
	// the schedule a session was started from. Absent (nil) keeps the
	// stored value; uuid.Nil explicitly CLEARS it.
	ScheduleID *uuid.UUID `json:"scheduleId"`
	// Schedule exception (one dated deviation of a schedule). originalDate
	// is "YYYY-MM-DD" ("" = ad-hoc extra occurrence). Times are wall-clock
	// seconds since midnight in the course timezone.
	ScheduleOriginalDate  *string `json:"scheduleOriginalDate"`
	ScheduleExceptionKind *string `json:"scheduleExceptionKind"`
	ScheduleChangedStart  *int    `json:"scheduleChangedStart"`
	ScheduleChangedEnd    *int    `json:"scheduleChangedEnd"`
	ScheduleMovedToDate   *string `json:"scheduleMovedToDate"`
	// Course material (teacher handout / problem set / document). Title
	// rides the shared Title; the course/session references ride the shared
	// CourseID/SessionID sentinel fields; the occurrence link reuses
	// ScheduleOccurrenceKey (an opaque grouping string server-side, same as
	// sessions); a material borrowed from a classroom image references it
	// via SourceAttachmentID. The digest (导读) rides as a JSON STRING in
	// MaterialDigest (the analysis/reviewContent convention). The ORIGINAL
	// FILE never rides push — it travels on /v1/materials/<id>/file.
	MaterialKind          *string    `json:"materialKind"`
	MaterialMime          *string    `json:"materialMime"`
	MaterialFileName      *string    `json:"materialFileName"`
	MaterialFormat        *string    `json:"materialFormat"`
	MaterialFileSize      *int64     `json:"materialFileSize"`
	MaterialHash          *string    `json:"materialHash"`
	MaterialPageCount     *int       `json:"materialPageCount"`
	MaterialSourceURL     *string    `json:"materialSourceURL"`
	MaterialSharedText    *string    `json:"materialSharedText"`
	MaterialExtraction    *string    `json:"materialExtraction"`
	MaterialDigestStatus  *string    `json:"materialDigestStatus"`
	MaterialDigest        *string    `json:"materialDigest"`
	MaterialDigestModel   *string    `json:"materialDigestModel"`
	MaterialDigestAt      *time.Time `json:"materialDigestAt"`
	MaterialDigestSrcHash *string    `json:"materialDigestSourceHash"`
	MaterialLastReadPage  *int       `json:"materialLastReadPage"`
	MaterialLastOpenedAt  *time.Time `json:"materialLastOpenedAt"`
	// Shared material reference: the parent material of a page/annotation/
	// assistant-message scope. Absent (nil) keeps the stored value;
	// uuid.Nil explicitly CLEARS it.
	MaterialID *uuid.UUID `json:"materialId"`
	// Material page: 1-based page number; the extraction layer and the OCR
	// layer ride separate fields (never merged server-side).
	MaterialPageNumber   *int    `json:"materialPageNumber"`
	MaterialPageText     *string `json:"materialPageText"`
	MaterialPageOCR      *string `json:"materialPageOCR"`
	MaterialPageOCRState *string `json:"materialPageOCRStatus"`
	// Material annotation: kind (note|bookmark); the note body rides the
	// shared NoteText; the page rides the shared MaterialPageNumber.
	MaterialAnnotationKind *string `json:"materialAnnotationKind"`
	// Course assistant: thread parent (required for messages); a message's
	// question scope rides the shared MaterialID/SessionID/MaterialPageNumber;
	// citations ride as a JSON STRING in AssistantCitations. Visual Q&A adds
	// the turn mode (text|visual), the evidence SNAPSHOT (JSON string:
	// stable source ids + normalized crop rects — never image bytes), the
	// structured answer payload (JSON string) and the answer model name.
	// Absent/empty keeps the stored value (a merge never blanks an answer).
	ThreadID           *uuid.UUID `json:"threadId"`
	AssistantRole      *string    `json:"assistantRole"`
	AssistantText      *string    `json:"assistantText"`
	AssistantCitations *string    `json:"assistantCitations"`
	AssistantMode      *string    `json:"assistantMode"`
	AssistantEvidence  *string    `json:"assistantEvidence"`
	AssistantAnswer    *string    `json:"assistantAnswer"`
	AssistantModel     *string    `json:"assistantModel"`
	// Exam center (00012). Exam title/plan title/plan-item title/topic
	// title ride the shared Title; the course reference rides the shared
	// CourseID sentinel field; the exam reference rides ExamId; the plan
	// reference rides PlanId; the plan-item reference rides PlanItemId.
	// Dates are YYYY-MM-DD strings (DATE columns, the course-schedule
	// convention); times are wall-clock seconds since midnight (-1 =
	// unknown). The candidate origin snapshot (ExamSource), the topic
	// source and the plan-item jump target ride as JSON STRINGS (the
	// citations convention) — never image bytes, file paths or raw model
	// responses.
	ExamKind          *string    `json:"examKind"`
	ExamDate          *string    `json:"examDate"`
	ExamStartSecs     *int       `json:"examStartSecs"`
	ExamEndSecs       *int       `json:"examEndSecs"`
	ExamLocation      *string    `json:"examLocation"`
	ExamScope         *string    `json:"examScope"`
	ExamNote          *string    `json:"examNote"`
	ExamTargetScore   *string    `json:"examTargetScore"`
	ExamStatus        *string    `json:"examStatus"`
	ExamOrigin        *string    `json:"examOrigin"`
	ExamSource        *string    `json:"examSource"`
	TopicDetail       *string    `json:"topicDetail"`
	TopicImportance   *string    `json:"topicImportance"`
	TopicSelfRating   *string    `json:"topicSelfRating"`
	TopicStatus       *string    `json:"topicStatus"`
	TopicSource       *string    `json:"topicSource"`
	TopicUserEdited   *bool      `json:"topicUserEdited"`
	PlanStartDate     *string    `json:"planStartDate"`
	PlanEndDate       *string    `json:"planEndDate"`
	PlanWeekdayMins   *int       `json:"planWeekdayMinutes"`
	PlanWeekendMins   *int       `json:"planWeekendMinutes"`
	PlanRestDays      *string    `json:"planRestDays"`
	PlanFinishEarly   *int       `json:"planFinishEarlyDays"`
	PlanIncludeCards  *bool      `json:"planIncludeCards"`
	PlanIncludeTasks  *bool      `json:"planIncludeTasks"`
	PlanMaterials     *bool      `json:"planIncludeMaterials"`
	PlanSessions      *bool      `json:"planIncludeSessions"`
	PlanFocusTopics   *string    `json:"planFocusTopics"`
	PlanBlockedTimes  *string    `json:"planBlockedTimes"`
	PlanStatus        *string    `json:"planStatus"`
	PlanItemDate      *string    `json:"planItemDate"`
	PlanItemKind      *string    `json:"planItemKind"`
	PlanItemEstMins   *int       `json:"planItemEstimatedMinutes"`
	PlanItemActual    *int       `json:"planItemActualMinutes"`
	PlanItemStatus    *string    `json:"planItemStatus"`
	PlanItemStatusAt  *time.Time `json:"planItemStatusChangedAt"`
	PlanItemOrder     *int       `json:"planItemOrder"`
	PlanItemSource    *string    `json:"planItemSource"`
	PlanItemUserNote  *string    `json:"planItemUserNote"`
	PlanItemUserEdit  *bool      `json:"planItemUserEdited"`
	ActivityStatus    *string    `json:"activityStatus"`
	ActivityStartedAt *time.Time `json:"activityStartedAt"`
	ActivityEndedAt   *time.Time `json:"activityEndedAt"`
	ActivityDuration  *int64     `json:"activityDurationSeconds"`
	ActivityNote      *string    `json:"activityNote"`
	// Shared exam-family references. Absent (nil) keeps the stored value;
	// uuid.Nil explicitly CLEARS it.
	ExamId     *uuid.UUID `json:"examId"`
	PlanId     *uuid.UUID `json:"planId"`
	PlanItemId *uuid.UUID `json:"planItemId"`
	TopicId    *uuid.UUID `json:"topicId"`
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
	CourseScheduleCount       int       `json:"courseScheduleCount"`
	ScheduleExceptionCount    int       `json:"scheduleExceptionCount"`
	MaterialCount             int       `json:"materialCount"`
	MaterialPageCount         int       `json:"materialPageCount"`
	MaterialAnnotationCount   int       `json:"materialAnnotationCount"`
	AssistantThreadCount      int       `json:"assistantThreadCount"`
	AssistantMessageCount     int       `json:"assistantMessageCount"`
	ExamCount                 int       `json:"examCount"`
	ExamTopicCount            int       `json:"examTopicCount"`
	StudyPlanCount            int       `json:"studyPlanCount"`
	StudyPlanItemCount        int       `json:"studyPlanItemCount"`
	StudyActivityCount        int       `json:"studyActivityCount"`
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
	// 15 / 17 chars — both fit the VARCHAR(32) entity_type columns.
	EntityCourseSchedule    = "course_schedule"
	EntityScheduleException = "schedule_exception"
	// Course-material library: 8 / 13 / 19 / 16 / 17 chars — all fit the
	// VARCHAR(32) entity_type columns widened by migration 00008.
	EntityMaterial           = "material"
	EntityMaterialPage       = "material_page"
	EntityMaterialAnnotation = "material_annotation"
	EntityAssistantThread    = "assistant_thread"
	EntityAssistantMessage   = "assistant_message"
	// Exam center (00012): 4 / 10 / 10 / 15 / 14 chars — all fit the
	// VARCHAR(32) entity_type columns.
	EntityExam          = "exam"
	EntityExamTopic     = "exam_topic"
	EntityStudyPlan     = "study_plan"
	EntityStudyPlanItem = "study_plan_item"
	EntityStudyActivity = "study_activity"
)

func validEntityType(t string) bool {
	return t == EntitySession || t == EntityEntry ||
		t == EntityBookmark || t == EntityFavorite ||
		t == EntityCourse || t == EntityNote ||
		t == EntityStudyReview || t == EntityAttachment ||
		t == EntityTerm || t == EntityStudyCard || t == EntityStudyTask ||
		t == EntityTranscriptCorrection ||
		t == EntityCourseSchedule || t == EntityScheduleException ||
		t == EntityMaterial || t == EntityMaterialPage ||
		t == EntityMaterialAnnotation ||
		t == EntityAssistantThread || t == EntityAssistantMessage ||
		t == EntityExam || t == EntityExamTopic ||
		t == EntityStudyPlan || t == EntityStudyPlanItem ||
		t == EntityStudyActivity
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
	CourseID           *string `json:"courseId,omitempty"`
	SessionID          *string `json:"sessionId,omitempty"`
	SourceReview       *string `json:"sourceReviewId,omitempty"`
	SourceEntry        *string `json:"sourceEntryId,omitempty"`
	SourceAttach       *string `json:"sourceAttachmentId,omitempty"`
	SourceMaterial     *string `json:"materialId,omitempty"`
	SourceMaterialPage int     `json:"materialPageNumber,omitempty"`
	SourceSessIDs      string  `json:"termSourceSessions,omitempty"`
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
	EntityType         string     `json:"entityType"`
	ID                 string     `json:"id"`
	CourseID           *string    `json:"courseId,omitempty"`
	SessionID          *string    `json:"sessionId,omitempty"`
	SourceEntry        *string    `json:"sourceEntryId,omitempty"`
	SourceAttach       *string    `json:"sourceAttachmentId,omitempty"`
	SourceTerm         *string    `json:"sourceTermId,omitempty"`
	SourceMaterial     *string    `json:"materialId,omitempty"`
	SourceMaterialPage int        `json:"materialPageNumber,omitempty"`
	Front              string     `json:"cardFront"`
	Back               string     `json:"cardBack"`
	CardType           string     `json:"cardType"`
	UserNote           string     `json:"cardUserNote"`
	Origin             string     `json:"cardOrigin"`
	Stage              string     `json:"cardStage"`
	ReviewCount        int        `json:"cardReviewCount"`
	IntervalHrs        int        `json:"cardIntervalHours"`
	DueAt              *time.Time `json:"cardDueAt,omitempty"`
	LastReviewed       *time.Time `json:"cardLastReviewedAt,omitempty"`
	LastGrade          string     `json:"cardLastGrade"`
	ServerVersion      int        `json:"serverVersion"`
	Deleted            bool       `json:"deleted"`
}

type studyTaskRecord struct {
	EntityType string `json:"entityType"`
	ID         string `json:"id"`
	// Title rides the shared "title" key (course/attachment convention).
	Title              string     `json:"title"`
	CourseID           *string    `json:"courseId,omitempty"`
	SessionID          *string    `json:"sessionId,omitempty"`
	SourceReview       *string    `json:"sourceReviewId,omitempty"`
	SourceEntry        *string    `json:"sourceEntryId,omitempty"`
	SourceAttach       *string    `json:"sourceAttachmentId,omitempty"`
	SourceMaterial     *string    `json:"materialId,omitempty"`
	SourceMaterialPage int        `json:"materialPageNumber,omitempty"`
	Detail             string     `json:"taskDetail"`
	DueAt              *time.Time `json:"taskDueAt,omitempty"`
	Priority           string     `json:"taskPriority"`
	Status             string     `json:"taskStatus"`
	Origin             string     `json:"taskOrigin"`
	Uncertainty        string     `json:"taskUncertainty"`
	UserNote           string     `json:"taskUserNote"`
	CompletedAt        *time.Time `json:"taskCompletedAt,omitempty"`
	ServerVersion      int        `json:"serverVersion"`
	Deleted            bool       `json:"deleted"`
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

// courseScheduleRecord / scheduleExceptionRecord: field names mirror the
// push payload (scheduleXxx family) so iOS decodes records and conflict
// payloads with the same CodingKeys. Dates are YYYY-MM-DD strings; times
// are wall-clock seconds since midnight in the schedule's timezone.
type courseScheduleRecord struct {
	EntityType     string  `json:"entityType"`
	ID             string  `json:"id"`
	CourseID       *string `json:"courseId,omitempty"`
	Weekday        int     `json:"scheduleWeekday"`
	StartSecs      int     `json:"scheduleStartSecs"`
	EndSecs        int     `json:"scheduleEndSecs"`
	Recurrence     string  `json:"scheduleRecurrence"`
	ParityAnchor   *string `json:"scheduleParityAnchor,omitempty"`
	FirstWeekIsOdd bool    `json:"scheduleFirstWeekIsOdd"`
	SemesterStart  string  `json:"scheduleSemesterStart"`
	SemesterEnd    string  `json:"scheduleSemesterEnd"`
	Timezone       string  `json:"scheduleTimezone"`
	Teacher        string  `json:"scheduleTeacher"`
	Location       string  `json:"scheduleLocation"`
	Note           string  `json:"scheduleNote"`
	ReminderMins   int     `json:"scheduleReminderMins"`
	Enabled        bool    `json:"scheduleEnabled"`
	OnceDate       *string `json:"scheduleOnceDate,omitempty"`
	ServerVersion  int     `json:"serverVersion"`
	Deleted        bool    `json:"deleted"`
}

type scheduleExceptionRecord struct {
	EntityType    string  `json:"entityType"`
	ID            string  `json:"id"`
	ScheduleID    string  `json:"scheduleId"`
	CourseID      *string `json:"courseId,omitempty"`
	OriginalDate  *string `json:"scheduleOriginalDate,omitempty"`
	ExceptionKind string  `json:"scheduleExceptionKind"`
	ChangedStart  *int    `json:"scheduleChangedStart,omitempty"`
	ChangedEnd    *int    `json:"scheduleChangedEnd,omitempty"`
	MovedToDate   *string `json:"scheduleMovedToDate,omitempty"`
	Location      string  `json:"scheduleLocation"`
	Teacher       string  `json:"scheduleTeacher"`
	Note          string  `json:"scheduleNote"`
	ServerVersion int     `json:"serverVersion"`
	Deleted       bool    `json:"deleted"`
}

// courseMaterialRecord / materialPageRecord / materialAnnotationRecord /
// assistantThreadRecord / assistantMessageRecord: field names mirror the
// push payload (materialXxx / assistantXxx families) so iOS decodes
// records and conflict payloads with the same CodingKeys. The digest is a
// JSON string (see PushPayload.MaterialDigest); citations likewise.
type courseMaterialRecord struct {
	EntityType    string  `json:"entityType"`
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	CourseID      *string `json:"courseId,omitempty"`
	SessionID     *string `json:"sessionId,omitempty"`
	OccurrenceKey string  `json:"scheduleOccurrenceKey,omitempty"`
	// A material borrowed from a classroom image references it (no file of
	// its own); nil = owns its file.
	SourceAttach  *string    `json:"sourceAttachmentId,omitempty"`
	Kind          string     `json:"materialKind"`
	MimeType      string     `json:"materialMime"`
	FileName      string     `json:"materialFileName"`
	Format        string     `json:"materialFormat"`
	FileSize      int64      `json:"materialFileSize"`
	ContentHash   string     `json:"materialHash"`
	PageCount     int        `json:"materialPageCount"`
	SourceURL     string     `json:"materialSourceURL"`
	SharedText    string     `json:"materialSharedText"`
	Extraction    string     `json:"materialExtraction"`
	DigestStatus  string     `json:"materialDigestStatus"`
	Digest        *string    `json:"materialDigest,omitempty"`
	DigestModel   string     `json:"materialDigestModel"`
	DigestAt      *time.Time `json:"materialDigestAt,omitempty"`
	DigestSrcHash string     `json:"materialDigestSourceHash"`
	LastReadPage  int        `json:"materialLastReadPage"`
	LastOpenedAt  *time.Time `json:"materialLastOpenedAt,omitempty"`
	ServerVersion int        `json:"serverVersion"`
	Deleted       bool       `json:"deleted"`
}

type materialPageRecord struct {
	EntityType    string `json:"entityType"`
	ID            string `json:"id"`
	MaterialID    string `json:"materialId"`
	PageNumber    int    `json:"materialPageNumber"`
	ExtractedText string `json:"materialPageText"`
	OcrText       string `json:"materialPageOCR"`
	OcrStatus     string `json:"materialPageOCRStatus"`
	ServerVersion int    `json:"serverVersion"`
	Deleted       bool   `json:"deleted"`
}

type materialAnnotationRecord struct {
	EntityType    string `json:"entityType"`
	ID            string `json:"id"`
	MaterialID    string `json:"materialId"`
	PageNumber    int    `json:"materialPageNumber"`
	Kind          string `json:"materialAnnotationKind"`
	NoteText      string `json:"noteText"`
	ServerVersion int    `json:"serverVersion"`
	Deleted       bool   `json:"deleted"`
}

type assistantThreadRecord struct {
	EntityType    string  `json:"entityType"`
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	CourseID      *string `json:"courseId,omitempty"`
	ServerVersion int     `json:"serverVersion"`
	Deleted       bool    `json:"deleted"`
}

type assistantMessageRecord struct {
	EntityType string  `json:"entityType"`
	ID         string  `json:"id"`
	ThreadID   string  `json:"threadId"`
	Role       string  `json:"assistantRole"`
	Text       string  `json:"assistantText"`
	Citations  *string `json:"assistantCitations,omitempty"`
	// Question scope (absent = course-wide question).
	ScopeMaterial   *string `json:"materialId,omitempty"`
	ScopeSession    *string `json:"sessionId,omitempty"`
	ScopePageNumber int     `json:"materialPageNumber,omitempty"`
	// Visual Q&A (00011): turn mode, evidence snapshot, structured answer
	// and the producing model — JSON strings on the wire, matching push.
	Mode           *string `json:"assistantMode,omitempty"`
	VisualEvidence *string `json:"assistantEvidence,omitempty"`
	Answer         *string `json:"assistantAnswer,omitempty"`
	ModelName      *string `json:"assistantModel,omitempty"`
	ServerVersion  int     `json:"serverVersion"`
	Deleted        bool    `json:"deleted"`
}

// Exam-center records (00012): field names mirror the push payload (the
// examXxx / topicXxx / planXxx / planItemXxx / activityXxx families) so
// iOS decodes records and conflict payloads with the same CodingKeys.
// Dates are YYYY-MM-DD strings; times are wall-clock seconds since
// midnight (-1 = unknown / no end). Source snapshots ride as JSON strings
// (the citations convention).
type examRecord struct {
	EntityType    string  `json:"entityType"`
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	CourseID      *string `json:"courseId,omitempty"`
	Kind          string  `json:"examKind"`
	Date          string  `json:"examDate"`
	StartSecs     int     `json:"examStartSecs"`
	EndSecs       int     `json:"examEndSecs"`
	Location      string  `json:"examLocation"`
	Scope         string  `json:"examScope"`
	Note          string  `json:"examNote"`
	TargetScore   string  `json:"examTargetScore"`
	Status        string  `json:"examStatus"`
	Origin        string  `json:"examOrigin"`
	Source        *string `json:"examSource,omitempty"`
	ServerVersion int     `json:"serverVersion"`
	Deleted       bool    `json:"deleted"`
}

type examTopicRecord struct {
	EntityType    string  `json:"entityType"`
	ID            string  `json:"id"`
	ExamID        string  `json:"examId"`
	Title         string  `json:"title"`
	Detail        string  `json:"topicDetail"`
	Importance    string  `json:"topicImportance"`
	SelfRating    string  `json:"topicSelfRating"`
	Status        string  `json:"topicStatus"`
	Source        *string `json:"topicSource,omitempty"`
	UserEdited    bool    `json:"topicUserEdited"`
	ServerVersion int     `json:"serverVersion"`
	Deleted       bool    `json:"deleted"`
}

type studyPlanRecord struct {
	EntityType       string  `json:"entityType"`
	ID               string  `json:"id"`
	ExamID           string  `json:"examId"`
	Title            string  `json:"title"`
	StartDate        string  `json:"planStartDate"`
	EndDate          string  `json:"planEndDate"`
	WeekdayMinutes   int     `json:"planWeekdayMinutes"`
	WeekendMinutes   int     `json:"planWeekendMinutes"`
	RestDays         *string `json:"planRestDays,omitempty"`
	FinishEarlyDays  int     `json:"planFinishEarlyDays"`
	IncludeCards     bool    `json:"planIncludeCards"`
	IncludeTasks     bool    `json:"planIncludeTasks"`
	IncludeMaterials bool    `json:"planIncludeMaterials"`
	IncludeSessions  bool    `json:"planIncludeSessions"`
	FocusTopics      *string `json:"planFocusTopics,omitempty"`
	BlockedTimes     *string `json:"planBlockedTimes,omitempty"`
	Status           string  `json:"planStatus"`
	ServerVersion    int     `json:"serverVersion"`
	Deleted          bool    `json:"deleted"`
}

type studyPlanItemRecord struct {
	EntityType       string     `json:"entityType"`
	ID               string     `json:"id"`
	PlanID           string     `json:"planId"`
	ExamID           *string    `json:"examId,omitempty"`
	Date             string     `json:"planItemDate"`
	Title            string     `json:"title"`
	Kind             string     `json:"planItemKind"`
	EstimatedMinutes int        `json:"planItemEstimatedMinutes"`
	ActualMinutes    int        `json:"planItemActualMinutes"`
	Status           string     `json:"planItemStatus"`
	StatusChangedAt  *time.Time `json:"planItemStatusChangedAt,omitempty"`
	Order            int        `json:"planItemOrder"`
	Source           *string    `json:"planItemSource,omitempty"`
	UserNote         string     `json:"planItemUserNote"`
	UserEdited       bool       `json:"planItemUserEdited"`
	ServerVersion    int        `json:"serverVersion"`
	Deleted          bool       `json:"deleted"`
}

type studyActivityRecord struct {
	EntityType      string     `json:"entityType"`
	ID              string     `json:"id"`
	PlanItemID      *string    `json:"planItemId,omitempty"`
	ExamID          *string    `json:"examId,omitempty"`
	CourseID        *string    `json:"courseId,omitempty"`
	TopicID         *string    `json:"topicId,omitempty"`
	StartedAt       time.Time  `json:"activityStartedAt"`
	EndedAt         *time.Time `json:"activityEndedAt,omitempty"`
	DurationSeconds int        `json:"activityDurationSeconds"`
	Status          string     `json:"activityStatus"`
	Note            string     `json:"activityNote"`
	ServerVersion   int        `json:"serverVersion"`
	Deleted         bool       `json:"deleted"`
}
