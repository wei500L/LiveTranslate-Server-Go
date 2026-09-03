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
	SchemaVersion          int       `json:"schemaVersion"`
	MinClientSchemaVersion int       `json:"minClientSchemaVersion"`
	MaxClientSchemaVersion int       `json:"maxClientSchemaVersion"`
	ChangeLogTail          int64     `json:"changeLogTail"`
	SessionCount           int       `json:"sessionCount"`
	EntryCount             int       `json:"entryCount"`
	CourseCount            int       `json:"courseCount"`
	NoteCount              int       `json:"noteCount"`
	PendingCount           int       `json:"pendingCount"`
	ServerTime             time.Time `json:"serverTime"`
}

// Entity constants (wire strings).
const (
	EntitySession  = "session"
	EntityEntry    = "entry"
	EntityBookmark = "bookmark"
	EntityFavorite = "favorite"
	EntityCourse   = "course"
	EntityNote     = "note"
)

func validEntityType(t string) bool {
	return t == EntitySession || t == EntityEntry ||
		t == EntityBookmark || t == EntityFavorite ||
		t == EntityCourse || t == EntityNote
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
	ServerVersion     int     `json:"serverVersion"`
	Deleted           bool    `json:"deleted"`
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
	ServerVersion int     `json:"serverVersion"`
	Deleted       bool    `json:"deleted"`
}
