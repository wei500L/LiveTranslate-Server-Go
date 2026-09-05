// Interpreter sync entities (随身翻译): saved face-to-face errand
// dialogs — interpreter_conversations and their turns (migration 00014).
//
// Semantics follow the established entity conventions:
//   - conversation delete: phantom tombstone / idempotent re-delete /
//     tombstone + CASCADE to its turns (server-side cascade, not an FK);
//   - turn delete: plain applyLearningDelete row delete;
//   - upsert: insert@v1 / base==ver merge / base<ver conflict / base>ver
//     rejected; deleted rows conflict (delete-wins);
//   - turns are append-style: the same-version merge resolves user edits
//     by modified_at (newer wins — the correction convention); absent
//     pointers keep the stored value.
package sync

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/internal/store"
)

// --- Enum allowlists ------------------------------------------------------------

var validInterpreterScenes = map[string]bool{
	"general": true, "school": true, "dorm": true, "bank": true,
	"hospital": true, "migration": true, "telecom": true, "post": true,
}

// draft|discarded stay wire-legal for forward tolerance: the client keeps
// drafts device-local, so the server should never see them — but it
// stores what it is told without interpreting it.
var validInterpreterStatuses = map[string]bool{
	"draft": true, "saved": true, "discarded": true,
}

var validInterpreterSpeakers = map[string]bool{
	"counterpart": true, "user": true,
}

var validInterpreterDirections = map[string]bool{
	"ru2zh": true, "zh2ru": true,
}

var validInterpreterInputMethods = map[string]bool{
	"audio": true, "text": true,
}

// --- Row readers ----------------------------------------------------------------

type interpreterConversationRow struct {
	id          uuid.UUID
	userID      uuid.UUID
	title       string
	scene       string
	contextNote string
	status      string
	startedAt   time.Time
	endedAt     *time.Time
	serverVer   int
	createdAt   time.Time
	updatedAt   time.Time
	deletedAt   *time.Time
}

func fetchInterpreterConversation(ctx context.Context, q store.Q, userID, id uuid.UUID) (*interpreterConversationRow, error) {
	c := &interpreterConversationRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, title, scene, context_note, status,
		       started_at, ended_at,
		       server_version, created_at, updated_at, deleted_at
		FROM interpreter_conversations WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&c.id, &c.userID, &c.title, &c.scene, &c.contextNote, &c.status,
		&c.startedAt, &c.endedAt,
		&c.serverVer, &c.createdAt, &c.updatedAt, &c.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func interpreterConversationRowView(c *interpreterConversationRow) learningRowView {
	if c == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: c.serverVer, updatedAt: c.updatedAt}
}

func interpreterConversationRecordJSON(c *interpreterConversationRow) json.RawMessage {
	b, _ := json.Marshal(interpreterConversationRecord{
		EntityType: EntityInterpreterConversation, ID: c.id.String(),
		Title: c.title, Scene: c.scene, ContextNote: c.contextNote,
		Status: c.status, StartedAt: c.startedAt, EndedAt: c.endedAt,
		ServerVersion: c.serverVer, Deleted: c.deletedAt != nil,
	})
	return b
}

type interpreterTurnRow struct {
	id              uuid.UUID
	userID          uuid.UUID
	conversationID  uuid.UUID
	speaker         string
	direction       string
	inputMethod     string
	sequence        int
	sourceText      string
	plainRussian    string
	stressedRussian string
	chineseText     string
	backTranslation string
	details         *string
	modifiedAt      *time.Time
	serverVer       int
	createdAt       time.Time
	updatedAt       time.Time
	deletedAt       *time.Time
}

func fetchInterpreterTurn(ctx context.Context, q store.Q, userID, id uuid.UUID) (*interpreterTurnRow, error) {
	t := &interpreterTurnRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, conversation_id, speaker, direction, input_method,
		       sequence, source_text, plain_russian, stressed_russian,
		       chinese_text, back_translation, details::text, modified_at,
		       server_version, created_at, updated_at, deleted_at
		FROM interpreter_turns WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&t.id, &t.userID, &t.conversationID, &t.speaker, &t.direction, &t.inputMethod,
		&t.sequence, &t.sourceText, &t.plainRussian, &t.stressedRussian,
		&t.chineseText, &t.backTranslation, &t.details, &t.modifiedAt,
		&t.serverVer, &t.createdAt, &t.updatedAt, &t.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func interpreterTurnRowView(t *interpreterTurnRow) learningRowView {
	if t == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: t.serverVer, updatedAt: t.updatedAt}
}

func interpreterTurnRecordJSON(t *interpreterTurnRow) json.RawMessage {
	var details *string
	if t.details != nil && *t.details != "" {
		details = t.details
	}
	b, _ := json.Marshal(interpreterTurnRecord{
		EntityType: EntityInterpreterTurn, ID: t.id.String(),
		ConversationID: t.conversationID.String(),
		Speaker:        t.speaker, Direction: t.direction, InputMethod: t.inputMethod,
		Sequence: t.sequence, SourceText: t.sourceText,
		PlainRussian: t.plainRussian, StressedRussian: t.stressedRussian,
		ChineseText: t.chineseText, BackTranslation: t.backTranslation,
		Details: details, ModifiedAt: t.modifiedAt,
		ServerVersion: t.serverVer, Deleted: t.deletedAt != nil,
	})
	return b
}

// --- Apply: conversation ------------------------------------------------------------

func (s *Service) applyInterpreterConversation(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchInterpreterConversation(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			// Turns follow the conversation into the tombstone.
			if err := tombstoneChildren(ctx, q, userID, "interpreter_turns",
				EntityInterpreterTurn, "conversation_id", item.EntityID); err != nil {
				return nil, err
			}
		}
		return s.applyLearningDelete(ctx, q, userID, item,
			EntityInterpreterConversation, "interpreter_conversations",
			obj != nil, obj != nil && obj.deletedAt != nil,
			interpreterConversationRowView(obj))
	}

	// Validation: started_at required (a conversation is anchored in time);
	// enum-ish fields must parse before any write. Title may be empty (the
	// client generates it locally, never as an AI save precondition).
	scene, status := "general", "saved"
	if p.InterpreterScene != nil && *p.InterpreterScene != "" {
		scene = *p.InterpreterScene
	}
	if p.InterpreterStatus != nil && *p.InterpreterStatus != "" {
		status = *p.InterpreterStatus
	}
	if !validInterpreterScenes[scene] || !validInterpreterStatuses[status] {
		return rejectInterpreterItem(ctx, q, userID, item)
	}
	if p.InterpreterStarted == nil {
		return rejectInterpreterItem(ctx, q, userID, item)
	}
	startedAt := *p.InterpreterStarted
	endedAt := p.InterpreterEnded
	title, contextNote := "", ""
	if p.Title != nil {
		title = *p.Title
	}
	if p.InterpreterContext != nil {
		contextNote = *p.InterpreterContext
	}

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO interpreter_conversations
				(id, user_id, title, scene, context_note, status,
				 started_at, ended_at,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6,
			        $7, $8,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, title, scene, contextNote, status,
			startedAt, endedAt,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityInterpreterConversation, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, interpreterConversationRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, interpreterConversationRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectInterpreterItem(ctx, q, userID, item)
	}

	// Merge: present pointers win; absent keeps. context_note is full
	// desired state (empty string clears — the shared_text convention).
	if p.InterpreterContext == nil {
		contextNote = obj.contextNote
	}
	if title == "" {
		title = obj.title
	}
	if endedAt == nil {
		endedAt = obj.endedAt
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE interpreter_conversations
		SET title = $3, scene = $4, context_note = $5, status = $6,
		    started_at = $7, ended_at = $8,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, title, scene, contextNote, status,
		startedAt, endedAt,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityInterpreterConversation, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: turn ------------------------------------------------------------

func (s *Service) applyInterpreterTurn(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchInterpreterTurn(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityInterpreterTurn, "interpreter_turns",
			obj != nil, obj != nil && obj.deletedAt != nil, interpreterTurnRowView(obj))
	}

	speaker, direction, inputMethod := "counterpart", "ru2zh", "audio"
	if p.TurnSpeaker != nil && *p.TurnSpeaker != "" {
		speaker = *p.TurnSpeaker
	}
	if p.TurnDirection != nil && *p.TurnDirection != "" {
		direction = *p.TurnDirection
	}
	if p.TurnInputMethod != nil && *p.TurnInputMethod != "" {
		inputMethod = *p.TurnInputMethod
	}
	if !validInterpreterSpeakers[speaker] || !validInterpreterDirections[direction] ||
		!validInterpreterInputMethods[inputMethod] {
		return rejectInterpreterItem(ctx, q, userID, item)
	}
	// The conversation reference is required (the entry/session convention:
	// a turn may arrive before its conversation, but it must name one).
	if p.ConversationId == nil || *p.ConversationId == uuid.Nil {
		return rejectInterpreterItem(ctx, q, userID, item)
	}
	conversationID := *p.ConversationId
	sequence := 0
	if p.TurnSequence != nil {
		sequence = *p.TurnSequence
	}
	sourceText, plainRussian, stressedRussian := "", "", ""
	chineseText, backTranslation := "", ""
	if p.TurnSource != nil {
		sourceText = *p.TurnSource
	}
	if p.TurnPlainRussian != nil {
		plainRussian = *p.TurnPlainRussian
	}
	if p.TurnStressedRussian != nil {
		stressedRussian = *p.TurnStressedRussian
	}
	if p.TurnChinese != nil {
		chineseText = *p.TurnChinese
	}
	if p.TurnBackTranslation != nil {
		backTranslation = *p.TurnBackTranslation
	}
	// The details snapshot must be valid JSON (JSONB column — invalid JSON
	// would abort the whole batch's transaction).
	details := validJSONPayload(p.TurnDetails)
	if p.TurnDetails != nil && *p.TurnDetails != "" && details == nil {
		return rejectInterpreterItem(ctx, q, userID, item)
	}
	modifiedAt := p.TurnModifiedAt

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO interpreter_turns
				(id, user_id, conversation_id, speaker, direction, input_method,
				 sequence, source_text, plain_russian, stressed_russian,
				 chinese_text, back_translation, details, modified_at,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6,
			        $7, $8, $9, $10,
			        $11, $12, $13::jsonb, $14,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, conversationID, speaker, direction, inputMethod,
			sequence, sourceText, plainRussian, stressedRussian,
			chineseText, backTranslation, details, modifiedAt,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityInterpreterTurn, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, interpreterTurnRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, interpreterTurnRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectInterpreterItem(ctx, q, userID, item)
	}

	// Merge: append-style row. User edits of the same turn resolve by
	// modified_at (newer wins — the correction convention; absent keeps).
	if modifiedAt == nil || (obj.modifiedAt != nil && obj.modifiedAt.After(*modifiedAt)) {
		modifiedAt = obj.modifiedAt
	}
	if sequence == 0 {
		sequence = obj.sequence
	}
	if sourceText == "" {
		sourceText = obj.sourceText
	}
	if plainRussian == "" {
		plainRussian = obj.plainRussian
	}
	if stressedRussian == "" {
		stressedRussian = obj.stressedRussian
	}
	if chineseText == "" {
		chineseText = obj.chineseText
	}
	if backTranslation == "" {
		backTranslation = obj.backTranslation
	}
	if details == nil {
		details = obj.details
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE interpreter_turns
		SET speaker = $3, direction = $4, input_method = $5,
		    sequence = $6, source_text = $7, plain_russian = $8,
		    stressed_russian = $9, chinese_text = $10, back_translation = $11,
		    details = $12::jsonb, modified_at = $13,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, speaker, direction, inputMethod,
		sequence, sourceText, plainRussian, stressedRussian,
		chineseText, backTranslation, details, modifiedAt,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityInterpreterTurn, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// rejectInterpreterItem is the shared schema-reject path for the
// interpreter family (recorded in the ledger like every permanent reject).
func rejectInterpreterItem(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	res := rejected(item, "schema")
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}
