// Exam-center sync entities: exams, their knowledge topics, study plans,
// plan items and study activities (migration 00012).
//
// Semantics follow the established entity conventions:
//   - delete: phantom tombstone / idempotent re-delete / tombstone +
//     cascade (exam → topics + plans (plans → items); plan → items);
//     study activities DETACH instead (exam_id / plan_item_id cleared —
//     the real learning-time history never dies with its exam);
//   - upsert: insert@v1 / base==ver merge / base<ver conflict / base>ver
//     rejected; deleted rows conflict (delete-wins);
//   - study_plan_item STATUS merges by status_changed_at (newer wins)
//     with done/skipped sticky — a stale pending push can neither
//     resurrect a skipped item nor blank progress (mirrors the task
//     sticky-done rule; a deliberate reopen rides a NEWER timestamp and
//     propagates);
//   - study_activity is append-style: completed/abandoned are terminal
//     and sticky; duration_seconds merges by MAX (accumulated minutes
//     from another device must never be lost).
package sync

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/internal/store"
)

// --- Row readers ----------------------------------------------------------------

type examRow struct {
	id          uuid.UUID
	userID      uuid.UUID
	courseID    *uuid.UUID
	title       string
	kind        string
	examDate    string // DATE as YYYY-MM-DD (read via to_char)
	startSecs   int
	endSecs     int
	location    string
	scopeText   string
	note        string
	targetScore string
	status      string
	origin      string
	source      *string // JSONB as string
	serverVer   int
	createdAt   time.Time
	updatedAt   time.Time
	deletedAt   *time.Time
}

func fetchExam(ctx context.Context, q store.Q, userID, id uuid.UUID) (*examRow, error) {
	e := &examRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, course_id, title, kind, to_char(exam_date, 'YYYY-MM-DD'),
		       start_secs, end_secs, location, scope_text, note, target_score,
		       status, origin, source::text,
		       server_version, created_at, updated_at, deleted_at
		FROM exams WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&e.id, &e.userID, &e.courseID, &e.title, &e.kind, &e.examDate,
		&e.startSecs, &e.endSecs, &e.location, &e.scopeText, &e.note, &e.targetScore,
		&e.status, &e.origin, &e.source,
		&e.serverVer, &e.createdAt, &e.updatedAt, &e.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

func examRowView(e *examRow) learningRowView {
	if e == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: e.serverVer, updatedAt: e.updatedAt}
}

func examRecordJSON(e *examRow) json.RawMessage {
	var source *string
	if e.source != nil && *e.source != "" {
		source = e.source
	}
	b, _ := json.Marshal(examRecord{
		EntityType: EntityExam, ID: e.id.String(),
		Title: e.title, CourseID: optUUIDString(e.courseID),
		Kind: e.kind, Date: e.examDate,
		StartSecs: e.startSecs, EndSecs: e.endSecs,
		Location: e.location, Scope: e.scopeText, Note: e.note,
		TargetScore: e.targetScore, Status: e.status, Origin: e.origin,
		Source:        source,
		ServerVersion: e.serverVer, Deleted: e.deletedAt != nil,
	})
	return b
}

type examTopicRow struct {
	id         uuid.UUID
	userID     uuid.UUID
	examID     uuid.UUID
	title      string
	detail     string
	importance string
	selfRating string
	status     string
	source     *string
	userEdited bool
	serverVer  int
	createdAt  time.Time
	updatedAt  time.Time
	deletedAt  *time.Time
}

func fetchExamTopic(ctx context.Context, q store.Q, userID, id uuid.UUID) (*examTopicRow, error) {
	t := &examTopicRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, exam_id, title, detail, importance, self_rating,
		       status, source::text, user_edited,
		       server_version, created_at, updated_at, deleted_at
		FROM exam_topics WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&t.id, &t.userID, &t.examID, &t.title, &t.detail, &t.importance,
		&t.selfRating, &t.status, &t.source, &t.userEdited,
		&t.serverVer, &t.createdAt, &t.updatedAt, &t.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func examTopicRowView(t *examTopicRow) learningRowView {
	if t == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: t.serverVer, updatedAt: t.updatedAt}
}

func examTopicRecordJSON(t *examTopicRow) json.RawMessage {
	var source *string
	if t.source != nil && *t.source != "" {
		source = t.source
	}
	b, _ := json.Marshal(examTopicRecord{
		EntityType: EntityExamTopic, ID: t.id.String(),
		ExamID: t.examID.String(), Title: t.title,
		Detail: t.detail, Importance: t.importance,
		SelfRating: t.selfRating, Status: t.status,
		Source:        source,
		UserEdited:    t.userEdited,
		ServerVersion: t.serverVer, Deleted: t.deletedAt != nil,
	})
	return b
}

type studyPlanRow struct {
	id               uuid.UUID
	userID           uuid.UUID
	examID           uuid.UUID
	title            string
	startDate        string
	endDate          string
	weekdayMinutes   int
	weekendMinutes   int
	restDays         *string
	finishEarlyDays  int
	includeCards     bool
	includeTasks     bool
	includeMaterials bool
	includeSessions  bool
	focusTopics      *string
	blockedTimes     *string
	status           string
	serverVer        int
	createdAt        time.Time
	updatedAt        time.Time
	deletedAt        *time.Time
}

func fetchStudyPlan(ctx context.Context, q store.Q, userID, id uuid.UUID) (*studyPlanRow, error) {
	p := &studyPlanRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, exam_id, title,
		       to_char(start_date, 'YYYY-MM-DD'), to_char(end_date, 'YYYY-MM-DD'),
		       weekday_minutes, weekend_minutes, rest_days::text,
		       finish_early_days, include_cards, include_tasks,
		       include_materials, include_sessions, focus_topics::text,
		       blocked_times::text, status,
		       server_version, created_at, updated_at, deleted_at
		FROM study_plans WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&p.id, &p.userID, &p.examID, &p.title,
		&p.startDate, &p.endDate,
		&p.weekdayMinutes, &p.weekendMinutes, &p.restDays,
		&p.finishEarlyDays, &p.includeCards, &p.includeTasks,
		&p.includeMaterials, &p.includeSessions, &p.focusTopics,
		&p.blockedTimes, &p.status,
		&p.serverVer, &p.createdAt, &p.updatedAt, &p.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func studyPlanRowView(p *studyPlanRow) learningRowView {
	if p == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: p.serverVer, updatedAt: p.updatedAt}
}

func studyPlanRecordJSON(p *studyPlanRow) json.RawMessage {
	var rest, focus, blocked *string
	if p.restDays != nil && *p.restDays != "" {
		rest = p.restDays
	}
	if p.focusTopics != nil && *p.focusTopics != "" {
		focus = p.focusTopics
	}
	if p.blockedTimes != nil && *p.blockedTimes != "" {
		blocked = p.blockedTimes
	}
	b, _ := json.Marshal(studyPlanRecord{
		EntityType: EntityStudyPlan, ID: p.id.String(),
		ExamID: p.examID.String(), Title: p.title,
		StartDate: p.startDate, EndDate: p.endDate,
		WeekdayMinutes: p.weekdayMinutes, WeekendMinutes: p.weekendMinutes,
		RestDays: rest, FinishEarlyDays: p.finishEarlyDays,
		IncludeCards: p.includeCards, IncludeTasks: p.includeTasks,
		IncludeMaterials: p.includeMaterials, IncludeSessions: p.includeSessions,
		FocusTopics: focus, BlockedTimes: blocked,
		Status:        p.status,
		ServerVersion: p.serverVer, Deleted: p.deletedAt != nil,
	})
	return b
}

type studyPlanItemRow struct {
	id              uuid.UUID
	userID          uuid.UUID
	planID          uuid.UUID
	examID          *uuid.UUID
	itemDate        string
	title           string
	kind            string
	estimatedMins   int
	actualMins      int
	status          string
	statusChangedAt *time.Time
	itemOrder       int
	source          *string
	userNote        string
	userEdited      bool
	serverVer       int
	createdAt       time.Time
	updatedAt       time.Time
	deletedAt       *time.Time
}

func fetchStudyPlanItem(ctx context.Context, q store.Q, userID, id uuid.UUID) (*studyPlanItemRow, error) {
	i := &studyPlanItemRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, plan_id, exam_id, to_char(item_date, 'YYYY-MM-DD'),
		       title, kind, estimated_minutes, actual_minutes, status,
		       status_changed_at, item_order, source::text, user_note, user_edited,
		       server_version, created_at, updated_at, deleted_at
		FROM study_plan_items WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&i.id, &i.userID, &i.planID, &i.examID, &i.itemDate,
		&i.title, &i.kind, &i.estimatedMins, &i.actualMins, &i.status,
		&i.statusChangedAt, &i.itemOrder, &i.source, &i.userNote, &i.userEdited,
		&i.serverVer, &i.createdAt, &i.updatedAt, &i.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return i, nil
}

func studyPlanItemRowView(i *studyPlanItemRow) learningRowView {
	if i == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: i.serverVer, updatedAt: i.updatedAt}
}

func studyPlanItemRecordJSON(i *studyPlanItemRow) json.RawMessage {
	var source *string
	if i.source != nil && *i.source != "" {
		source = i.source
	}
	var changedAt *time.Time
	if i.statusChangedAt != nil {
		changedAt = i.statusChangedAt
	}
	b, _ := json.Marshal(studyPlanItemRecord{
		EntityType: EntityStudyPlanItem, ID: i.id.String(),
		PlanID: i.planID.String(), ExamID: optUUIDString(i.examID),
		Date: i.itemDate, Title: i.title, Kind: i.kind,
		EstimatedMinutes: i.estimatedMins, ActualMinutes: i.actualMins,
		Status: i.status, StatusChangedAt: changedAt,
		Order: i.itemOrder, Source: source,
		UserNote: i.userNote, UserEdited: i.userEdited,
		ServerVersion: i.serverVer, Deleted: i.deletedAt != nil,
	})
	return b
}

type studyActivityRow struct {
	id         uuid.UUID
	userID     uuid.UUID
	planItemID *uuid.UUID
	examID     *uuid.UUID
	courseID   *uuid.UUID
	topicID    *uuid.UUID
	startedAt  time.Time
	endedAt    *time.Time
	duration   int
	status     string
	note       string
	serverVer  int
	createdAt  time.Time
	updatedAt  time.Time
	deletedAt  *time.Time
}

func fetchStudyActivity(ctx context.Context, q store.Q, userID, id uuid.UUID) (*studyActivityRow, error) {
	a := &studyActivityRow{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, plan_item_id, exam_id, course_id, topic_id,
		       started_at, ended_at, duration_seconds, status, note,
		       server_version, created_at, updated_at, deleted_at
		FROM study_activities WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&a.id, &a.userID, &a.planItemID, &a.examID, &a.courseID, &a.topicID,
		&a.startedAt, &a.endedAt, &a.duration, &a.status, &a.note,
		&a.serverVer, &a.createdAt, &a.updatedAt, &a.deletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func studyActivityRowView(a *studyActivityRow) learningRowView {
	if a == nil {
		return learningRowView{}
	}
	return learningRowView{serverVer: a.serverVer, updatedAt: a.updatedAt}
}

func studyActivityRecordJSON(a *studyActivityRow) json.RawMessage {
	b, _ := json.Marshal(studyActivityRecord{
		EntityType: EntityStudyActivity, ID: a.id.String(),
		PlanItemID: optUUIDString(a.planItemID), ExamID: optUUIDString(a.examID),
		CourseID: optUUIDString(a.courseID), TopicID: optUUIDString(a.topicID),
		StartedAt: a.startedAt, EndedAt: a.endedAt,
		DurationSeconds: a.duration, Status: a.status, Note: a.note,
		ServerVersion: a.serverVer, Deleted: a.deletedAt != nil,
	})
	return b
}

// --- Valid values ----------------------------------------------------------------

var validExamKinds = map[string]bool{
	"midterm": true, "final": true, "quiz": true, "lab": true,
	"oral": true, "report": true, "defense": true, "custom": true,
}

// `pending` is wire-legal but never pushed by a correct client (AI
// candidates stay device-local; see migration 00012's header).
var validExamStatuses = map[string]bool{
	"pending": true, "scheduled": true, "done": true, "cancelled": true,
}

var validExamOrigins = map[string]bool{"manual": true, "ai": true}

var validTopicImportance = map[string]bool{
	"low": true, "normal": true, "high": true,
}

var validTopicSelfRatings = map[string]bool{
	"none": true, "vague": true, "basic": true, "proficient": true,
}

var validTopicStatuses = map[string]bool{
	"not_started": true, "learning": true, "needs_review": true, "mastered": true,
}

var validPlanStatuses = map[string]bool{
	"active": true, "paused": true, "archived": true,
}

var validPlanItemKinds = map[string]bool{
	"material": true, "session": true, "review": true, "task": true,
	"cards": true, "topic": true, "terms": true, "practice": true, "custom": true,
}

var validPlanItemStatuses = map[string]bool{
	"pending": true, "in_progress": true, "done": true, "skipped": true, "deferred": true,
}

var validActivityStatuses = map[string]bool{
	"in_progress": true, "completed": true, "abandoned": true,
}

// --- Apply: exam ----------------------------------------------------------------

func (s *Service) applyExam(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchExam(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			// Topics and plans (with their items) follow the exam into the
			// tombstone; study activities DETACH (the learning history
			// survives — actual minutes must never die with the exam).
			if err := s.cascadeDeleteExamChildren(ctx, q, userID, item.EntityID); err != nil {
				return nil, err
			}
			if err := s.detachExamFromStudyActivities(ctx, q, userID, item.EntityID); err != nil {
				return nil, err
			}
		}
		return s.applyLearningDelete(ctx, q, userID, item, EntityExam, "exams",
			obj != nil, obj != nil && obj.deletedAt != nil, examRowView(obj))
	}

	// Validation: title required (the course/attachment convention);
	// enum-ish fields and the date must parse before any write.
	title := ""
	if p.Title != nil {
		title = *p.Title
	}
	if title == "" {
		return rejectExamItem(ctx, q, userID, item)
	}
	kind, status, origin := "custom", "scheduled", "manual"
	if p.ExamKind != nil && *p.ExamKind != "" {
		kind = *p.ExamKind
	}
	if p.ExamStatus != nil && *p.ExamStatus != "" {
		status = *p.ExamStatus
	}
	if p.ExamOrigin != nil && *p.ExamOrigin != "" {
		origin = *p.ExamOrigin
	}
	if !validExamKinds[kind] || !validExamStatuses[status] || !validExamOrigins[origin] {
		return rejectExamItem(ctx, q, userID, item)
	}
	if p.ExamDate == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	examDate, err := parseWireDate(*p.ExamDate)
	if err != nil || examDate == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	// The source snapshot must be valid JSON (JSONB column — invalid JSON
	// would abort the whole batch's transaction).
	source := validJSONPayload(p.ExamSource)
	if p.ExamSource != nil && *p.ExamSource != "" && source == nil {
		return rejectExamItem(ctx, q, userID, item)
	}

	startSecs, endSecs, location, scope, note, target := -1, -1, "", "", "", ""
	if p.ExamStartSecs != nil {
		startSecs = *p.ExamStartSecs
	}
	if p.ExamEndSecs != nil {
		endSecs = *p.ExamEndSecs
	}
	if p.ExamLocation != nil {
		location = *p.ExamLocation
	}
	if p.ExamScope != nil {
		scope = *p.ExamScope
	}
	if p.ExamNote != nil {
		note = *p.ExamNote
	}
	if p.ExamTargetScore != nil {
		target = *p.ExamTargetScore
	}
	courseID := refOrNil(p.CourseID)

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO exams
				(id, user_id, course_id, title, kind, exam_date,
				 start_secs, end_secs, location, scope_text, note, target_score,
				 status, origin, source,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6,
			        $7, $8, $9, $10, $11, $12,
			        $13, $14, $15::jsonb,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, courseID, title, kind, *examDate,
			startSecs, endSecs, location, scope, note, target,
			status, origin, source,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityExam, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, examRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, examRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectExamItem(ctx, q, userID, item)
	}

	// Merge: present pointers win; absent keeps. Full desired state rides
	// every upsert (the iOS payload always sends these fields).
	if location == "" {
		location = obj.location
	}
	if scope == "" {
		scope = obj.scopeText
	}
	if note == "" {
		note = obj.note
	}
	if target == "" {
		target = obj.targetScore
	}
	if source == nil {
		source = obj.source
	}
	courseID = mergeRef(obj.courseID, p.CourseID)

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE exams
		SET course_id = $3, title = $4, kind = $5, exam_date = $6,
		    start_secs = $7, end_secs = $8, location = $9, scope_text = $10,
		    note = $11, target_score = $12, status = $13, origin = $14,
		    source = $15::jsonb,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, courseID, title, kind, *examDate,
		startSecs, endSecs, location, scope, note, target, status, origin,
		source,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityExam, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// rejectExamItem is the shared schema-reject path for the exam family
// (recorded in the ledger like every permanent reject).
func rejectExamItem(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	res := rejected(item, "schema")
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: exam topic -------------------------------------------------------------

func (s *Service) applyExamTopic(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchExamTopic(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityExamTopic, "exam_topics",
			obj != nil, obj != nil && obj.deletedAt != nil, examTopicRowView(obj))
	}

	title := ""
	if p.Title != nil {
		title = *p.Title
	}
	if p.ExamId == nil || *p.ExamId == uuid.Nil || title == "" {
		return rejectExamItem(ctx, q, userID, item)
	}
	examID := *p.ExamId
	importance, selfRating, status := "normal", "none", "not_started"
	if p.TopicImportance != nil && *p.TopicImportance != "" {
		importance = *p.TopicImportance
	}
	if p.TopicSelfRating != nil && *p.TopicSelfRating != "" {
		selfRating = *p.TopicSelfRating
	}
	if p.TopicStatus != nil && *p.TopicStatus != "" {
		status = *p.TopicStatus
	}
	if !validTopicImportance[importance] || !validTopicSelfRatings[selfRating] ||
		!validTopicStatuses[status] {
		return rejectExamItem(ctx, q, userID, item)
	}
	detail := ""
	if p.TopicDetail != nil {
		detail = *p.TopicDetail
	}
	source := validJSONPayload(p.TopicSource)
	if p.TopicSource != nil && *p.TopicSource != "" && source == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	userEdited := p.TopicUserEdited != nil && *p.TopicUserEdited

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO exam_topics
				(id, user_id, exam_id, title, detail, importance, self_rating,
				 status, source, user_edited,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        $8, $9::jsonb, $10,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, examID, title, detail, importance, selfRating,
			status, source, userEdited,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityExamTopic, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, examTopicRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, examTopicRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectExamItem(ctx, q, userID, item)
	}

	if detail == "" {
		detail = obj.detail
	}
	if source == nil {
		source = obj.source
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE exam_topics
		SET exam_id = $3, title = $4, detail = $5, importance = $6,
		    self_rating = $7, status = $8, source = $9::jsonb, user_edited = $10,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, examID, title, detail, importance,
		selfRating, status, source, userEdited,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityExamTopic, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: study plan -------------------------------------------------------------

func (s *Service) applyStudyPlan(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchStudyPlan(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			// Items follow the plan into the tombstone; their activities
			// detach (the learning history survives).
			if err := s.cascadeDeletePlanItems(ctx, q, userID, item.EntityID); err != nil {
				return nil, err
			}
		}
		return s.applyLearningDelete(ctx, q, userID, item, EntityStudyPlan, "study_plans",
			obj != nil, obj != nil && obj.deletedAt != nil, studyPlanRowView(obj))
	}

	title := ""
	if p.Title != nil {
		title = *p.Title
	}
	if p.ExamId == nil || *p.ExamId == uuid.Nil || title == "" {
		return rejectExamItem(ctx, q, userID, item)
	}
	examID := *p.ExamId
	if p.PlanStartDate == nil || p.PlanEndDate == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	startDate, err := parseWireDate(*p.PlanStartDate)
	if err != nil || startDate == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	endDate, err := parseWireDate(*p.PlanEndDate)
	if err != nil || endDate == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	status := "active"
	if p.PlanStatus != nil && *p.PlanStatus != "" {
		status = *p.PlanStatus
	}
	if !validPlanStatuses[status] {
		return rejectExamItem(ctx, q, userID, item)
	}
	for _, payloadJSON := range []*string{p.PlanRestDays, p.PlanFocusTopics, p.PlanBlockedTimes} {
		if payloadJSON != nil && *payloadJSON != "" && !json.Valid([]byte(*payloadJSON)) {
			return rejectExamItem(ctx, q, userID, item)
		}
	}
	restDays := validJSONPayload(p.PlanRestDays)
	focusTopics := validJSONPayload(p.PlanFocusTopics)
	blockedTimes := validJSONPayload(p.PlanBlockedTimes)

	weekdayMinutes, weekendMinutes, finishEarly := 60, 90, 0
	if p.PlanWeekdayMins != nil {
		weekdayMinutes = *p.PlanWeekdayMins
	}
	if p.PlanWeekendMins != nil {
		weekendMinutes = *p.PlanWeekendMins
	}
	if p.PlanFinishEarly != nil {
		finishEarly = *p.PlanFinishEarly
	}
	includeCards := true
	if p.PlanIncludeCards != nil {
		includeCards = *p.PlanIncludeCards
	}
	includeTasks := true
	if p.PlanIncludeTasks != nil {
		includeTasks = *p.PlanIncludeTasks
	}
	includeMaterials := true
	if p.PlanMaterials != nil {
		includeMaterials = *p.PlanMaterials
	}
	includeSessions := true
	if p.PlanIncludeSessions != nil {
		includeSessions = *p.PlanIncludeSessions
	}

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO study_plans
				(id, user_id, exam_id, title, start_date, end_date,
				 weekday_minutes, weekend_minutes, rest_days, finish_early_days,
				 include_cards, include_tasks, include_materials, include_sessions,
				 focus_topics, blocked_times, status,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6,
			        $7, $8, $9::jsonb, $10,
			        $11, $12, $13, $14,
			        $15::jsonb, $16::jsonb, $17,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, examID, title, *startDate, *endDate,
			weekdayMinutes, weekendMinutes, restDays, finishEarly,
			includeCards, includeTasks, includeMaterials, includeSessions,
			focusTopics, blockedTimes, status,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityStudyPlan, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyPlanRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyPlanRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectExamItem(ctx, q, userID, item)
	}

	if restDays == nil {
		restDays = obj.restDays
	}
	if focusTopics == nil {
		focusTopics = obj.focusTopics
	}
	if blockedTimes == nil {
		blockedTimes = obj.blockedTimes
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE study_plans
		SET exam_id = $3, title = $4, start_date = $5, end_date = $6,
		    weekday_minutes = $7, weekend_minutes = $8, rest_days = $9::jsonb,
		    finish_early_days = $10, include_cards = $11, include_tasks = $12,
		    include_materials = $13, include_sessions = $14,
		    focus_topics = $15::jsonb, blocked_times = $16::jsonb, status = $17,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, examID, title, *startDate, *endDate,
		weekdayMinutes, weekendMinutes, restDays, finishEarly,
		includeCards, includeTasks, includeMaterials, includeSessions,
		focusTopics, blockedTimes, status,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityStudyPlan, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: study plan item ---------------------------------------------------------

func (s *Service) applyStudyPlanItem(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchStudyPlanItem(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		if obj != nil && obj.deletedAt == nil {
			// The item's activities detach (plan_item_id cleared) — the
			// learning history survives the plan.
			if err := s.detachPlanItemFromStudyActivities(ctx, q, userID, item.EntityID); err != nil {
				return nil, err
			}
		}
		return s.applyLearningDelete(ctx, q, userID, item, EntityStudyPlanItem, "study_plan_items",
			obj != nil, obj != nil && obj.deletedAt != nil, studyPlanItemRowView(obj))
	}

	title := ""
	if p.Title != nil {
		title = *p.Title
	}
	if p.PlanId == nil || *p.PlanId == uuid.Nil || title == "" || p.PlanItemDate == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	planID := *p.PlanId
	itemDate, err := parseWireDate(*p.PlanItemDate)
	if err != nil || itemDate == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	kind, status := "custom", "pending"
	if p.PlanItemKind != nil && *p.PlanItemKind != "" {
		kind = *p.PlanItemKind
	}
	if p.PlanItemStatus != nil && *p.PlanItemStatus != "" {
		status = *p.PlanItemStatus
	}
	if !validPlanItemKinds[kind] || !validPlanItemStatuses[status] {
		return rejectExamItem(ctx, q, userID, item)
	}
	source := validJSONPayload(p.PlanItemSource)
	if p.PlanItemSource != nil && *p.PlanItemSource != "" && source == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	estimated, actual, order := 30, 0, 0
	if p.PlanItemEstMins != nil {
		estimated = *p.PlanItemEstMins
	}
	if p.PlanItemActual != nil {
		actual = *p.PlanItemActual
	}
	if p.PlanItemOrder != nil {
		order = *p.PlanItemOrder
	}
	userNote := ""
	if p.PlanItemUserNote != nil {
		userNote = *p.PlanItemUserNote
	}
	userEdited := p.PlanItemUserEdit != nil && *p.PlanItemUserEdit
	examID := refOrNil(p.ExamId)
	statusChangedAt := p.PlanItemStatusAt

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO study_plan_items
				(id, user_id, plan_id, exam_id, item_date, title, kind,
				 estimated_minutes, actual_minutes, status, status_changed_at,
				 item_order, source, user_note, user_edited,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        $8, $9, $10, $11,
			        $12, $13::jsonb, $14, $15,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, planID, examID, *itemDate, title, kind,
			estimated, actual, status, statusChangedAt,
			order, source, userNote, userEdited,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityStudyPlanItem, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyPlanItemRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyPlanItemRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectExamItem(ctx, q, userID, item)
	}

	// Merge. Text: present wins, absent keeps. Reference: sentinel merge.
	if userNote == "" {
		userNote = obj.userNote
	}
	if source == nil {
		source = obj.source
	}
	examID = mergeRef(obj.examID, p.ExamId)

	// STATUS merge — the round's core conflict rule: when the incoming
	// status differs, the status whose status_changed_at is NEWER wins
	// (ties: the incoming push, which carries the newer user intent).
	// done/skipped are therefore sticky against a stale pending push, and
	// a deliberate reopen propagates through its newer timestamp.
	if status != obj.status {
		incoming := statusChangedAt
		if incoming == nil {
			now := time.Now()
			incoming = &now
		}
		if obj.statusChangedAt != nil && incoming.Before(*obj.statusChangedAt) {
			status = obj.status
		}
	}
	// Actual minutes are monotonic across devices: the larger value wins.
	if actual < obj.actualMins {
		actual = obj.actualMins
	}
	if statusChangedAt == nil {
		statusChangedAt = obj.statusChangedAt
	}

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE study_plan_items
		SET plan_id = $3, exam_id = $4, item_date = $5, title = $6, kind = $7,
		    estimated_minutes = $8, actual_minutes = $9, status = $10,
		    status_changed_at = $11, item_order = $12, source = $13::jsonb,
		    user_note = $14, user_edited = $15,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, planID, examID, *itemDate, title, kind,
		estimated, actual, status, statusChangedAt, order, source,
		userNote, userEdited,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityStudyPlanItem, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Apply: study activity ----------------------------------------------------------

func (s *Service) applyStudyActivity(ctx context.Context, q store.Q, userID uuid.UUID, item *PushItem) (*PushItemResult, error) {
	obj, err := fetchStudyActivity(ctx, q, userID, item.EntityID)
	if err != nil {
		return nil, err
	}
	p := item.Payload

	if item.Operation == "delete" {
		return s.applyLearningDelete(ctx, q, userID, item, EntityStudyActivity, "study_activities",
			obj != nil, obj != nil && obj.deletedAt != nil, studyActivityRowView(obj))
	}

	status := "in_progress"
	if p.ActivityStatus != nil && *p.ActivityStatus != "" {
		status = *p.ActivityStatus
	}
	if !validActivityStatuses[status] {
		return rejectExamItem(ctx, q, userID, item)
	}
	if p.ActivityStartedAt == nil {
		return rejectExamItem(ctx, q, userID, item)
	}
	startedAt := *p.ActivityStartedAt
	endedAt := p.ActivityEndedAt
	duration := 0
	if p.ActivityDurationSeconds != nil {
		duration = int(*p.ActivityDurationSeconds)
	}
	note := ""
	if p.ActivityNote != nil {
		note = *p.ActivityNote
	}
	planItemID := refOrNil(p.PlanItemId)
	examID := refOrNil(p.ExamId)
	courseID := refOrNil(p.CourseID)
	topicID := refOrNil(p.TopicId)

	if obj == nil {
		var updatedAt time.Time
		err := q.QueryRow(ctx, `
			INSERT INTO study_activities
				(id, user_id, plan_item_id, exam_id, course_id, topic_id,
				 started_at, ended_at, duration_seconds, status, note,
				 server_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6,
			        $7, $8, $9, $10, $11,
			        1, now(), now())
			RETURNING updated_at`,
			item.EntityID, userID, planItemID, examID, courseID, topicID,
			startedAt, endedAt, duration, status, note,
		).Scan(&updatedAt)
		if err != nil {
			return nil, err
		}
		if err := logChange(ctx, q, userID, EntityStudyActivity, item.EntityID, "upsert", 1); err != nil {
			return nil, err
		}
		res := accepted(item, 1, updatedAt)
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}

	if obj.deletedAt != nil {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyActivityRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion < obj.serverVer {
		res := conflict(item, obj.serverVer, obj.updatedAt, studyActivityRecordJSON(obj))
		if err := storeLedger(ctx, q, userID, item, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if item.BaseVersion > obj.serverVer {
		return rejectExamItem(ctx, q, userID, item)
	}

	// Merge: append-style row. Terminal states are sticky (a stale
	// in_progress push can never resurrect a completed/abandoned record);
	// duration is monotonic (MAX — another device's minutes must never be
	// lost); references detach-only via the sentinel.
	if status == "in_progress" && (obj.status == "completed" || obj.status == "abandoned") {
		status = obj.status
	}
	if duration < obj.duration {
		duration = obj.duration
	}
	if endedAt == nil {
		endedAt = obj.endedAt
	} else if obj.endedAt != nil && obj.endedAt.After(*endedAt) {
		endedAt = obj.endedAt
	}
	if note == "" {
		note = obj.note
	}
	planItemID = mergeRef(obj.planItemID, p.PlanItemId)
	examID = mergeRef(obj.examID, p.ExamId)
	courseID = mergeRef(obj.courseID, p.CourseID)
	topicID = mergeRef(obj.topicID, p.TopicId)

	var version int
	var updatedAt time.Time
	err = q.QueryRow(ctx, `
		UPDATE study_activities
		SET plan_item_id = $3, exam_id = $4, course_id = $5, topic_id = $6,
		    started_at = $7, ended_at = $8, duration_seconds = $9,
		    status = $10, note = $11,
		    server_version = server_version + 1, updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING server_version, updated_at`,
		item.EntityID, userID, planItemID, examID, courseID, topicID,
		startedAt, endedAt, duration, status, note,
	).Scan(&version, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := logChange(ctx, q, userID, EntityStudyActivity, item.EntityID, "upsert", version); err != nil {
		return nil, err
	}
	res := accepted(item, version, updatedAt)
	if err := storeLedger(ctx, q, userID, item, res); err != nil {
		return nil, err
	}
	return res, nil
}

// --- Cascades / detaches --------------------------------------------------------

// cascadeDeleteExamChildren tombstones an exam's topics and plans (each
// plan cascades its items). Every bumped child is logged so other devices
// drop their local rows too.
func (s *Service) cascadeDeleteExamChildren(ctx context.Context, q store.Q, userID, examID uuid.UUID) error {
	if err := tombstoneChildren(ctx, q, userID, "exam_topics", EntityExamTopic, "exam_id", examID); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT id FROM study_plans
		WHERE user_id = $1 AND exam_id = $2 AND deleted_at IS NULL`, userID, examID)
	if err != nil {
		return err
	}
	var planIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		planIDs = append(planIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, planID := range planIDs {
		if err := s.cascadeDeletePlanItems(ctx, q, userID, planID); err != nil {
			return err
		}
	}
	return tombstoneChildren(ctx, q, userID, "study_plans", EntityStudyPlan, "exam_id", examID)
}

// cascadeDeletePlanItems tombstones a plan's items.
func (s *Service) cascadeDeletePlanItems(ctx context.Context, q store.Q, userID, planID uuid.UUID) error {
	return tombstoneChildren(ctx, q, userID, "study_plan_items", EntityStudyPlanItem, "plan_id", planID)
}

// tombstoneChildren is the shared cascade helper (the material-children
// pattern): tombstone every live child row, log each bumped row.
func tombstoneChildren(
	ctx context.Context, q store.Q, userID uuid.UUID,
	table, entity, parentColumn string, parentID uuid.UUID,
) error {
	rows, err := q.Query(ctx, `
		UPDATE `+table+`
		SET deleted_at = now(), server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND `+parentColumn+` = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, parentID)
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
		if err := logChange(ctx, q, userID, entity, b.id, "delete", b.v); err != nil {
			return err
		}
	}
	return nil
}

// detachExamFromStudyActivities clears exam_id on a deleted exam's study
// activities (the real learning-time history survives — the detach
// convention). Every bumped row is logged.
func (s *Service) detachExamFromStudyActivities(ctx context.Context, q store.Q, userID, examID uuid.UUID) error {
	return detachChildren(ctx, q, userID, "study_activities", EntityStudyActivity, "exam_id", examID)
}

// detachPlanItemFromStudyActivities clears plan_item_id on a deleted
// item's activities.
func (s *Service) detachPlanItemFromStudyActivities(ctx context.Context, q store.Q, userID, itemID uuid.UUID) error {
	return detachChildren(ctx, q, userID, "study_activities", EntityStudyActivity, "plan_item_id", itemID)
}

// detachCourseFromExams clears course_id on a deleted course's exams
// (exams transfer to 未归类 — they are never tombstoned by a course
// delete). Also detaches the course's study activities (学习历史 keeps its
// minutes, loses the course attribution).
func (s *Service) detachCourseFromExams(ctx context.Context, q store.Q, userID, courseID uuid.UUID) error {
	if err := detachChildren(ctx, q, userID, "exams", EntityExam, "course_id", courseID); err != nil {
		return err
	}
	return detachChildren(ctx, q, userID, "study_activities", EntityStudyActivity, "course_id", courseID)
}

// detachChildren is the shared detach helper (the material detach
// pattern): clear the parent column on every live child row, log each.
func detachChildren(
	ctx context.Context, q store.Q, userID uuid.UUID,
	table, entity, parentColumn string, parentID uuid.UUID,
) error {
	rows, err := q.Query(ctx, `
		UPDATE `+table+`
		SET `+parentColumn+` = NULL, server_version = server_version + 1, updated_at = now()
		WHERE user_id = $1 AND `+parentColumn+` = $2 AND deleted_at IS NULL
		RETURNING id, server_version`, userID, parentID)
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
		if err := logChange(ctx, q, userID, entity, b.id, "upsert", b.v); err != nil {
			return err
		}
	}
	return nil
}
