package admin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/internal/store"
)

// --- Dashboard aggregates -------------------------------------------------------

// DashboardStats is the overview grid. Counts only — no transcript text is
// ever selected anywhere in this package.
type DashboardStats struct {
	UsersTotal        int
	UsersActive       int
	UsersPending      int
	UsersSuspended    int
	UsersUnverified   int
	RegisteredToday   int
	DeviceRows        int
	LiveSessions      int
	ClassroomSessions int
	TranscriptEntries int
	StudyReviews      int
	GlossaryTerms     int
	StudyCards        int
	StudyTasks        int
	// User corrections overlaying the model output (manual edit layer).
	TranscriptCorrections int
	// Pre-class layer: recurring schedules and dated exceptions.
	CourseSchedules    int
	ScheduleExceptions int
	// Course-material library: imported documents and the assistant's
	// conversation threads.
	CourseMaterials   int
	MaterialPages     int
	AssistantThreads  int
	AssistantMessages int
	// Exam center: exams, topics, study plans, items and activities.
	Exams           int
	ExamTopics      int
	StudyPlans      int
	StudyPlanItems  int
	StudyActivities int
	RecentSyncAt    *time.Time
	MailSent        int64 // from the in-process metrics counters
	MailFailed      int64
	APIErrors       int64
	HTTPRequests    int64
	// RegistrationTrend is per-day counts for the last 7 days, oldest first.
	RegistrationTrend []TrendPoint
	RegistrationMode  string
}

type TrendPoint struct {
	Day   time.Time
	Count int
}

func LoadDashboardStats(ctx context.Context, q store.Q) (*DashboardStats, error) {
	d := &DashboardStats{}
	err := q.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM users WHERE deleted_at IS NULL),
			(SELECT count(*) FROM users WHERE deleted_at IS NULL AND status = 'active'),
			(SELECT count(*) FROM users WHERE deleted_at IS NULL AND status = 'pending'),
			(SELECT count(*) FROM users WHERE deleted_at IS NULL AND status = 'suspended'),
			(SELECT count(*) FROM users WHERE deleted_at IS NULL AND email_verified_at IS NULL AND status <> 'deleted'),
			(SELECT count(*) FROM users WHERE deleted_at IS NULL AND created_at >= date_trunc('day', now())),
			(SELECT count(*) FROM devices),
			(SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL AND expires_at > now()),
			(SELECT count(*) FROM classroom_sessions WHERE deleted_at IS NULL),
			(SELECT count(*) FROM transcript_entries WHERE deleted_at IS NULL),
			(SELECT count(*) FROM study_reviews WHERE deleted_at IS NULL),
			(SELECT count(*) FROM glossary_terms WHERE deleted_at IS NULL),
			(SELECT count(*) FROM study_cards WHERE deleted_at IS NULL),
			(SELECT count(*) FROM study_tasks WHERE deleted_at IS NULL),
			(SELECT count(*) FROM transcript_corrections WHERE deleted_at IS NULL),
			(SELECT count(*) FROM course_schedules WHERE deleted_at IS NULL),
			(SELECT count(*) FROM schedule_exceptions WHERE deleted_at IS NULL),
			(SELECT count(*) FROM course_materials WHERE deleted_at IS NULL),
			(SELECT count(*) FROM material_pages WHERE deleted_at IS NULL),
			(SELECT count(*) FROM assistant_threads WHERE deleted_at IS NULL),
			(SELECT count(*) FROM assistant_messages WHERE deleted_at IS NULL),
			(SELECT count(*) FROM exams WHERE deleted_at IS NULL),
			(SELECT count(*) FROM exam_topics WHERE deleted_at IS NULL),
			(SELECT count(*) FROM study_plans WHERE deleted_at IS NULL),
			(SELECT count(*) FROM study_plan_items WHERE deleted_at IS NULL),
			(SELECT count(*) FROM study_activities WHERE deleted_at IS NULL),
			(SELECT max(created_at) FROM sync_changes)
	`).Scan(&d.UsersTotal, &d.UsersActive, &d.UsersPending, &d.UsersSuspended,
		&d.UsersUnverified, &d.RegisteredToday, &d.DeviceRows, &d.LiveSessions,
		&d.ClassroomSessions, &d.TranscriptEntries, &d.StudyReviews,
		&d.GlossaryTerms, &d.StudyCards, &d.StudyTasks,
		&d.TranscriptCorrections, &d.CourseSchedules, &d.ScheduleExceptions,
		&d.CourseMaterials, &d.MaterialPages, &d.AssistantThreads, &d.AssistantMessages,
		&d.Exams, &d.ExamTopics, &d.StudyPlans, &d.StudyPlanItems, &d.StudyActivities,
		&d.RecentSyncAt)
	if err != nil {
		return nil, err
	}

	rows, err := q.Query(ctx, `
		SELECT d.day::timestamptz, coalesce(c.count, 0)
		FROM generate_series(
			date_trunc('day', now()) - interval '6 days',
			date_trunc('day', now()),
			interval '1 day'
		) AS d(day)
		LEFT JOIN (
			SELECT date_trunc('day', created_at) AS day, count(*) AS count
			FROM users
			WHERE created_at >= date_trunc('day', now()) - interval '6 days'
			  AND deleted_at IS NULL
			GROUP BY 1
		) AS c ON c.day = d.day
		ORDER BY d.day`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p TrendPoint
		if err := rows.Scan(&p.Day, &p.Count); err != nil {
			return nil, err
		}
		d.RegistrationTrend = append(d.RegistrationTrend, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return d, nil
}

// --- User listing (filters, sorting, pagination) ----------------------------------

// UserQuery carries the list filters. All fields optional.
type UserQuery struct {
	Search   string // email prefix / display name / exact UUID
	Status   string // active | pending | suspended | pending_deletion | deleted | ""
	Provider string // email | apple | dev | ""
	Sort     string // created | last_login | last_sync
	Page     int
}

// UserSummary is one row of the user list.
type UserSummary struct {
	ID            uuid.UUID
	Email         *string
	DisplayName   string
	Status        string
	EmailVerified bool
	Provider      string
	CreatedAt     time.Time
	LastLoginAt   *time.Time
	LastSyncAt    *time.Time
	DeletedAt     *time.Time
	SessionCount  int
	EntryCount    int
}

const userSortCreated = "u.created_at DESC"
const userSortLastLogin = "u.last_login_at DESC NULLS LAST, u.created_at DESC"
const userSortLastSync = `latest_sync DESC NULLS LAST, u.created_at DESC`

// listUserConditions builds the WHERE fragment + args for the user list.
func listUserConditions(query UserQuery) (string, []any) {
	conds := []string{"TRUE"}
	var args []any
	if s := strings.TrimSpace(query.Search); s != "" {
		args = append(args, s)
		i := len(args)
		if _, err := uuid.Parse(s); err == nil {
			// Exact UUID search.
			conds = append(conds, fmt.Sprintf("u.id::text = $%d", i))
		} else {
			// Email prefix or display-name substring.
			conds = append(conds, fmt.Sprintf(
				"(u.normalized_email LIKE $%d || '%%' OR u.display_name ILIKE '%%' || $%d || '%%')", i, i))
		}
	}
	if query.Status != "" {
		args = append(args, query.Status)
		conds = append(conds, fmt.Sprintf("u.status = $%d", len(args)))
	}
	switch query.Provider {
	case "email":
		conds = append(conds, "u.normalized_email IS NOT NULL")
	case "apple":
		conds = append(conds, "u.apple_subject IS NOT NULL")
	case "dev":
		conds = append(conds, "u.dev_name IS NOT NULL")
	}
	return strings.Join(conds, " AND "), args
}

func userSortClause(sort string) string {
	switch sort {
	case "last_login":
		return userSortLastLogin
	case "last_sync":
		return userSortLastSync
	default:
		return userSortCreated
	}
}

const userSummarySelect = `
	u.id, u.email, u.display_name, u.status,
	(u.email_verified_at IS NOT NULL) AS email_verified,
	CASE WHEN u.normalized_email IS NOT NULL THEN 'email'
	     WHEN u.apple_subject IS NOT NULL THEN 'apple'
	     WHEN u.dev_name IS NOT NULL THEN 'dev'
	     ELSE 'unknown' END AS provider,
	u.created_at, u.last_login_at, u.deleted_at,
	(SELECT max(sc.created_at) FROM sync_changes sc WHERE sc.user_id = u.id) AS latest_sync,
	(SELECT count(*) FROM refresh_tokens rt
		WHERE rt.user_id = u.id AND rt.revoked_at IS NULL AND rt.expires_at > now()) AS session_count,
	(SELECT count(*) FROM transcript_entries te
		WHERE te.user_id = u.id AND te.deleted_at IS NULL) AS entry_count`

func scanUserSummary(r interface{ Scan(...any) error }) (*UserSummary, error) {
	u := &UserSummary{}
	err := r.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Status, &u.EmailVerified,
		&u.Provider, &u.CreatedAt, &u.LastLoginAt, &u.LastSyncAt, &u.DeletedAt,
		&u.SessionCount, &u.EntryCount)
	return u, err
}

func ListUsers(ctx context.Context, q store.Q, query UserQuery, limit, offset int) ([]*UserSummary, error) {
	where, args := listUserConditions(query)
	args = append(args, limit, offset)
	rows, err := q.Query(ctx, `
		SELECT `+userSummarySelect+`
		FROM users u
		WHERE `+where+`
		ORDER BY `+userSortClause(query.Sort)+`
		LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserSummary
	for rows.Next() {
		u, err := scanUserSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func CountUsers(ctx context.Context, q store.Q, query UserQuery) (int, error) {
	where, args := listUserConditions(query)
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM users u WHERE `+where, args...).Scan(&n)
	return n, err
}

// --- User detail (no transcript content) -------------------------------------------

// UserDetail adds device/session metadata and audit timelines.
type UserDetail struct {
	UserSummary
	Devices        []DeviceSummary
	Providers      []string
	HasPassword    bool
	AppleBound     bool
	ClassroomCount int
	DataBytes      int64
	SecurityEvents []AuditEventRow
	AdminActions   []AuditEventRow
}

type DeviceSummary struct {
	ID         uuid.UUID
	DeviceName string
	AppVersion string
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

// MaskedEmail renders the account's email for the detail page: first 2
// chars of the local part + "***" + domain. Never the full address.
func (d *UserDetail) MaskedEmail() string {
	fallback := "(无邮箱 / Apple 或开发者账号)"
	if d.Email == nil || *d.Email == "" {
		return fallback
	}
	e := *d.Email
	at := strings.LastIndex(e, "@")
	if at <= 0 {
		return "***"
	}
	local := e[:at]
	keep := 2
	if len(local) < keep {
		keep = len(local)
	}
	return local[:keep] + "***" + e[at:]
}

// GetUserDetail loads one user with counts, providers, devices and audit
// timelines. Counts only — transcript content stays out of scope.
func GetUserDetail(ctx context.Context, q store.Q, id uuid.UUID) (*UserDetail, error) {
	d := &UserDetail{}
	row := q.QueryRow(ctx, `
		SELECT `+userSummarySelect+`,
			(u.apple_subject IS NOT NULL) AS apple_bound,
			(u.dev_name IS NOT NULL) AS dev_account,
			(SELECT count(*) FROM classroom_sessions cs
				WHERE cs.user_id = u.id AND cs.deleted_at IS NULL),
			(SELECT coalesce(sum(length(te.russian_text) + coalesce(length(te.chinese_text), 0)), 0)
				FROM transcript_entries te WHERE te.user_id = u.id AND te.deleted_at IS NULL)
				+ (SELECT coalesce(sum(length(cs.title) + 512), 0)
				FROM classroom_sessions cs WHERE cs.user_id = u.id AND cs.deleted_at IS NULL)
		FROM users u WHERE u.id = $1`, id)
	var appleBound, devAccount bool
	if err := row.Scan(&d.ID, &d.Email, &d.DisplayName, &d.Status, &d.EmailVerified,
		&d.Provider, &d.CreatedAt, &d.LastLoginAt, &d.LastSyncAt, &d.DeletedAt,
		&d.SessionCount, &d.EntryCount, &appleBound, &devAccount,
		&d.ClassroomCount, &d.DataBytes); err != nil {
		if err == pgx.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	// Sign-in methods.
	if ok, _ := store.HasPasswordCredential(ctx, q, id); ok {
		d.HasPassword = true
		d.Providers = append(d.Providers, "password")
	}
	if appleBound {
		d.AppleBound = true
		d.Providers = append(d.Providers, "apple")
	}
	if devAccount {
		d.Providers = append(d.Providers, "dev")
	}

	// Devices.
	devs, err := q.Query(ctx, `
		SELECT d.id, d.display_name, d.app_version, d.last_seen_at, d.revoked_at
		FROM devices d WHERE d.user_id = $1 ORDER BY d.last_seen_at DESC`, id)
	if err != nil {
		return nil, err
	}
	defer devs.Close()
	for devs.Next() {
		var ds DeviceSummary
		if err := devs.Scan(&ds.ID, &ds.DeviceName, &ds.AppVersion, &ds.LastSeenAt, &ds.RevokedAt); err != nil {
			return nil, err
		}
		d.Devices = append(d.Devices, ds)
	}
	if err := devs.Err(); err != nil {
		return nil, err
	}
	return d, nil
}

// RevokeUserDevice revokes one device row + its refresh tokens (admin path;
// ownership is re-checked against the path's user id).
func RevokeUserDevice(ctx context.Context, q store.Q, userID, deviceID uuid.UUID) error {
	ct, err := q.Exec(ctx, `
		UPDATE devices SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, deviceID, userID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	if _, err := q.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE device_id = $1 AND revoked_at IS NULL`, deviceID); err != nil {
		return err
	}
	return nil
}
