// Errand-case sync entities (办事事项): a continuing real-world errand
// and its checklist items — errand_cases and errand_case_items
// (migration 00015).
//
// Semantics follow the established entity conventions:
//   - case delete: phantom tombstone / idempotent re-delete /
//     tombstone + CASCADE to its items (server-side cascade, not an
//     FK — the interpreter convention);
//   - item delete: plain applyLearningDelete row delete;
//   - upsert: insert@v1 / base==ver merge / base<ver conflict / base>ver
//     rejected; deleted rows conflict (delete-wins);
//   - items are independent rows with independent server_versions:
//     checking one item on device A never overwrites a different item
//     on device B (no whole-checklist payloads);
//   - same-version item merges resolve user edits by modified_at
//     (newer wins — the correction/turn convention); terminal item
//     statuses (done/skipped) are sticky unless the incoming edit is
//     genuinely newer (the plan-item convention);
//   - the case status machine is CLIENT-managed: the server validates
//     the enum and stores it, never derives or interprets it;
//   - local source links never ride the wire — only the content-free
//     ErrandHasLocalSources flag does (the round-17 boundary).
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

// The scene allowlist is SHARED with interpreter_conversations (the
// errand scene selector IS the interpreter scene selector, reused
// verbatim — see validInterpreterScenes).

// draft stays wire-legal for forward tolerance: the client keeps drafts
// device-local, so the server should never see them — but it stores
// what it is told without interpreting it (the interpreter convention).
var validErrandCaseStatuses = map[string]bool{
	"draft": true, "preparing": true, "scheduled": true,
	"waitingForResult": true, "needsFollowUp": true,
	"completed": true, "cancelled": true, "archived": true,
}

var validErrandItemKinds = map[string]bool{
	"requiredDocument": true, "action": true, "question": true,
	"payment": true, "appointment": true, "deadline": true,
	"followUp": true,
}

var validErrandItemStatuses = map[string]bool{
	"unconfirmed": true, "pending": true, "done": true, "skipped": true,
}

var validErrandItemOrigins = map[string]bool{
	"manual": true, "ai": true,
}

// --- Row readers ----------------------------------------------------------------

type errandCaseRow struct {
	id               uuid.UUID
	userID           uuid.UUID
	title            string
	scene            string
	status           string
	purpose          string
	userNote         string
	timezone         string
	location         string
	contact          string
	expectedResultAt *time.Time
	pinned           bool
	hasLocalSources  bool
	serverVer        int
	createdAt        time.Time
	updatedAt        time.Time
	deletedAt        *time.Time
}

func fetchErrandCase(ctx context.Context, q store.Q, userID, id uuid.UUID) (*errandCaseRow, error) {
	c := &errandCaseRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, title, scene, status, purpose, user_note,
		       timezone, location, contact, expected_result_at,
		       pinned, has_local_sources,
		       server_version, created_at, updated_at, deleted_at
		FROM errand_cases WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&c.id, &c.userID, &c.title, &c.scene, &c.status, &c.purpose, &c.userNote,
		&c.timezone, &c.location, &c.contact, &c.expectedResultAt,
		&c.pinned, &c.hasLocalSources,
		&c.serverVer, &c.createdAt, &c.updatedAt, &c.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func errandCaseRowView(c *errandCaseRow) learningRowView {
	if c == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: c.serverVer, updatedAt: c.updatedAt}
}

func errandCaseRecordJSON(c *errandCaseRow) json.RawMessage {
	b, _ := json.Marshal(errandCaseRecord{
		EntityType: EntityErrandCase, ID: c.id.String(),
		Title: c.title, Scene: c.scene, Status: c.status,
		Purpose: c.purpose, UserNote: c.userNote,
		Timezone: c.timezone, Location: c.location, Contact: c.contact,
		ExpectedResultAt: c.expectedResultAt,
		Pinned:           c.pinned, HasLocalSources: c.hasLocalSources,
		ServerVersion: c.serverVer, Deleted: c.deletedAt != nil,
	})
	return b
}

type errandCaseItemRow struct {
	id            uuid.UUID
	userID        uuid.UUID
	caseID        uuid.UUID
	title         string
	kind          string
	status        string
	sequence      int
	detail        string
	dueAt         *time.Time
	dateText      string
	dateRelative  bool
	dateUncertain bool
	origin        string
	confirmed     bool
	feeText       string
	feeAmount     *float64
	feeCurrency   string
	modifiedAt    *time.Time
	completedAt   *time.Time
	serverVer     int
	createdAt     time.Time
	updatedAt     time.Time
	deletedAt     *time.Time
}

func fetchErrandCaseItem(ctx context.Context, q store.Q, userID, id uuid.UUID) (*errandCaseItemRow, error) {
	i := &errandCaseItemRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, case_id, title, kind, status, sequence, detail,
		       due_at, date_text, is_relative_date, date_uncertain,
		       origin, confirmed, fee_text, fee_amount, fee_currency,
		       modified_at, completed_at,
		       server_version, created_at, updated_at, deleted_at
		FROM errand_case_items WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&i.id, &i.userID, &i.caseID, &i.title, &i.kind, &i.status, &i.sequence, &i.detail,
		&i.dueAt, &i.dateText, &i.dateRelative, &i.dateUncertain,
		&i.origin, &i.confirmed, &i.feeText, &i.feeAmount, &i.feeCurrency,
		&i.modifiedAt, &i.completedAt,
		&i.serverVer, &i.createdAt, &i.updatedAt, &i.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return i, nil
}

func errandCaseItemRowView(i *errandCaseItemRow) learningRowView {
	if i == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: i.serverVer, updatedAt: i.updatedAt}
}

func errandCaseItemRecordJSON(i *errandCaseItemRow) json.RawMessage {
	b, _ := json.Marshal(errandCaseItemRecord{
		EntityType: EntityErrandCaseItem, ID: i.id.String(),
		CaseID: i.caseID.String(), Title: i.title,
		Kind: i.kind, Status: i.status, Sequence: i.sequence,
		Detail: i.detail, DueAt: i.dueAt,
		DateText: i.dateText, DateRelative: i.dateRelative, DateUncertain: i.dateUncertain,
		Origin: i.origin, Confirmed: i.confirmed,
		FeeText: i.feeText, FeeAmount: i.feeAmount, FeeCurrency: i.feeCurrency,
		ModifiedAt: i.modifiedAt, CompletedAt: i.completedAt,
		ServerVersion: i.serverVer, Deleted: i.deletedAt != nil,
	})
	return b
}

// --- Apply: case ------------------------------------------------------------
func (s *Service) applyErrandCase(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchErrandCase(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			// Items follow the case into the tombstone.
			if err := tombstoneChildren(ctx, q, userID, "errand_case_items",
				EntityErrandCaseItem, "case_id", item.EntityID); err != nil {
				return nil, err
			}
		}
		return s.applyLearningDelete(ctx, q, userID, item,
			EntityErrandCase, "errand_cases",
			obj != nil, obj != nil && obj.deletedAt != nil,
			errandCaseRowView(obj))
	}

	// Validation before any write: enum fields must parse; the timezone,
	// when present, must be a real IANA id (the course-schedule
	// convention — a typo'd zone never persists).
	scene, status := "general", "preparing"
	if p.ErrandScene != nil && *p.ErrandScene != "" {
		scene = *p.ErrandScene
	}
	if p.ErrandStatus != nil && *p.ErrandStatus != "" {
		status = *p.ErrandStatus
	}
	if !validInterpreterScenes[scene] || !validErrandCaseStatuses[status] {
		return rejectErrandItem(ctx, q, userID, item)
	}
	timezone := ""
	if p.ErrandTimezone != nil {
		timezone = *p.ErrandTimezone
	}
	if timezone != "" {
		if _, err := time.LoadLocation(timezone); err != nil {
			return rejectErrandItem(ctx, q, userID, item)
		}
	}
	title, purpose, note, location, contact := "", "", "", "", ""
	if p.Title != nil {
		title = *p.Title
	}
	if p.ErrandPurpose != nil {
		purpose = *p.ErrandPurpose
	}
	if p.ErrandNote != nil {
		note = *p.ErrandNote
	}
	if p.ErrandLocation != nil {
		location = *p.ErrandLocation
	}
	if p.ErrandContact != nil {
		contact = *p.ErrandContact
	}
	expectedAt := p.ErrandExpectedResultAt
	pinned := p.ErrandPinned != nil && *p.ErrandPinned
	hasLocalSources := p.ErrandHasLocalSources != nil && *p.ErrandHasLocalSources

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO errand_cases
				(id, user_id, title, scene, status, purpose, user_note,
				 timezone, location, contact, expected_result_at,
				 pinned, has_local_sources,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        $8, $9, $10, $11,
			        $12, $13,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, title, scene, status, purpose, note,
			timezone, location, contact, expectedAt,
			pinned, hasLocalSources,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityErrandCase, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, errandCaseRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, errandCaseRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectErrandItem(ctx, q, userID, item)
	}

	// Merge: present pointers win; absent keeps. purpose/user_note are
	// full desired state ('' clears — the context_note convention); an
	// empty title keeps the stored one (a merge must never blank the
	// case's name — the interpreter convention).
	if p.ErrandPurpose == nil {
		purpose = obj.purpose
	}
	if p.ErrandNote == nil {
		note = obj.userNote
	}
	if title == "" {
		title = obj.title
	}
	if p.ErrandLocation == nil {
		location = obj.location
	}
	if p.ErrandContact == nil {
		contact = obj.contact
	}
	if p.ErrandTimezone == nil {
		timezone = obj.timezone
	}
	if p.ErrandExpectedResultAt == nil {
		expectedAt = obj.expectedResultAt
	}
	if p.ErrandPinned == nil {
		pinned = obj.pinned
	}
	if p.ErrandHasLocalSources == nil {
		hasLocalSources = obj.hasLocalSources
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE errand_cases
		SET title = $3, scene = $4, status = $5, purpose = $6, user_note = $7,
		    timezone = $8, location = $9, contact = $10, expected_result_at = $11,
		    pinned = $12, has_local_sources = $13,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, title, scene, status, purpose, note,
		timezone, location, contact, expectedAt,
		pinned, hasLocalSources,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityErrandCase, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: item ------------------------------------------------------------
func (s *Service) applyErrandCaseItem(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchErrandCaseItem(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityErrandCaseItem, "errand_case_items",
			obj != nil, obj != nil && obj.deletedAt != nil, errandCaseItemRowView(obj))
	}

	kind, status, origin := "action", "pending", "manual"
	if p.ErrandItemKind != nil && *p.ErrandItemKind != "" {
		kind = *p.ErrandItemKind
	}
	if p.ErrandItemStatus != nil && *p.ErrandItemStatus != "" {
		status = *p.ErrandItemStatus
	}
	if p.ErrandItemOrigin != nil && *p.ErrandItemOrigin != "" {
		origin = *p.ErrandItemOrigin
	}
	if !validErrandItemKinds[kind] || !validErrandItemStatuses[status] ||
		!validErrandItemOrigins[origin] {
		return rejectErrandItem(ctx, q, userID, item)
	}
	// The case reference is required (the conversation/term convention:
	// an item may arrive before its case, but it must name one).
	if p.CaseID == nil || *p.CaseID == uuid.Nil {
		return rejectErrandItem(ctx, q, userID, item)
	}
	caseID := *p.CaseID
	sequence := 0
	if p.ErrandItemSequence != nil {
		sequence = *p.ErrandItemSequence
	}
	if sequence < 0 {
		return rejectErrandItem(ctx, q, userID, item)
	}
	title, detail, dateText, feeText, feeCurrency := "", "", "", "", ""
	if p.Title != nil {
		title = *p.Title
	}
	if p.ErrandItemDetail != nil {
		detail = *p.ErrandItemDetail
	}
	if p.ErrandItemDateText != nil {
		dateText = *p.ErrandItemDateText
	}
	if p.ErrandItemFeeText != nil {
		feeText = *p.ErrandItemFeeText
	}
	if p.ErrandItemFeeCurrency != nil {
		feeCurrency = *p.ErrandItemFeeCurrency
	}
	if len(feeCurrency) > 8 {
		return rejectErrandItem(ctx, q, userID, item)
	}
	dueAt := p.ErrandItemDueAt
	dateRelative := p.ErrandItemDateRelative != nil && *p.ErrandItemDateRelative
	dateUncertain := p.ErrandItemDateUncertain != nil && *p.ErrandItemDateUncertain
	feeAmount := p.ErrandItemFeeAmount
	modifiedAt := p.ErrandItemModifiedAt
	completedAt := p.ErrandItemCompletedAt

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO errand_case_items
				(id, user_id, case_id, title, kind, status, sequence, detail,
				 due_at, date_text, is_relative_date, date_uncertain,
				 origin, confirmed, fee_text, fee_amount, fee_currency,
				 modified_at, completed_at,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			        $9, $10, $11, $12,
			        $13, $14, $15, $16, $17,
			        $18, $19,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, caseID, title, kind, status, sequence, detail,
			dueAt, dateText, dateRelative, dateUncertain,
			origin, p.ErrandItemConfirmed != nil && *p.ErrandItemConfirmed,
			feeText, feeAmount, feeCurrency,
			modifiedAt, completedAt,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityErrandCaseItem, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, errandCaseItemRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, errandCaseItemRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectErrandItem(ctx, q, userID, item)
	}

	// Merge: present pointers win; absent keeps. User-edited text
	// (title/detail) resolves by modified_at — a stale replay carrying
	// an older timestamp never overwrites the newer stored text (the
	// turn convention).
	staleEdit := obj.modifiedAt != nil &&
		(modifiedAt == nil || obj.modifiedAt.After(*modifiedAt))
	// A genuinely newer edit: the incoming modified_at is present and
	// strictly newer than anything stored (nil stored = first stamp).
	// Computed BEFORE the absent-pointer backfill below.
	newerEdit := modifiedAt != nil &&
		(obj.modifiedAt == nil || modifiedAt.After(*obj.modifiedAt))
	if modifiedAt == nil {
		modifiedAt = obj.modifiedAt
	}
	if p.Title == nil || staleEdit {
		title = obj.title
	}
	if title == "" {
		title = obj.title
	}
	if p.ErrandItemDetail == nil || staleEdit {
		detail = obj.detail
	}
	if p.ErrandItemDateText == nil || staleEdit {
		dateText = obj.dateText
	}
	if p.ErrandItemFeeText == nil || staleEdit {
		feeText = obj.feeText
	}
	if p.ErrandItemFeeCurrency == nil {
		feeCurrency = obj.feeCurrency
	}
	if p.ErrandItemFeeAmount == nil {
		feeAmount = obj.feeAmount
	}
	if p.ErrandItemDueAt == nil {
		dueAt = obj.dueAt
	}
	if p.ErrandItemDateRelative == nil {
		dateRelative = obj.dateRelative
	}
	if p.ErrandItemDateUncertain == nil {
		dateUncertain = obj.dateUncertain
	}
	if p.ErrandItemOrigin == nil {
		origin = obj.origin
	}
	if sequence == 0 {
		sequence = obj.sequence
	}
	// Confirmed is sticky: once the user confirmed an AI candidate, a
	// stale replay never un-confirms it.
	confirmed := (p.ErrandItemConfirmed != nil && *p.ErrandItemConfirmed) || obj.confirmed
	// Terminal statuses are sticky (the plan-item convention): a stored
	// done/skipped item only reopens on a genuinely newer user edit —
	// a push with no modified_at (or an older one) never reopens it.
	if (obj.status == "done" || obj.status == "skipped") &&
		(status == "pending" || status == "unconfirmed") && !newerEdit {
		status = obj.status
	}
	if status == "done" && completedAt == nil {
		completedAt = obj.completedAt
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE errand_case_items
		SET kind = $3, status = $4, sequence = $5, detail = $6,
		    due_at = $7, date_text = $8, is_relative_date = $9, date_uncertain = $10,
		    origin = $11, confirmed = $12, fee_text = $13, fee_amount = $14, fee_currency = $15,
		    modified_at = $16, completed_at = $17,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, kind, status, sequence, detail,
		dueAt, dateText, dateRelative, dateUncertain,
		origin, confirmed, feeText, feeAmount, feeCurrency,
		modifiedAt, completedAt,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityErrandCaseItem, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// rejectErrandItem is the shared schema-reject path for the errand
// family (recorded in the ledger like every permanent reject).
func rejectErrandItem(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	res := rejected(item, "schema")
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}
