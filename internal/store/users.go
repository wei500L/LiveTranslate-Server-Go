package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// User mirrors the users table. Status: active | pending | suspended |
// pending_deletion | deleted (deleted is expressed via deleted_at, kept as
// a status for admin filtering).
type User struct {
	ID                  uuid.UUID
	Email               *string
	NormalizedEmail     *string
	DisplayName         string
	Status              string
	Role                string
	AppleSubject        *string
	DevName             *string
	EmailVerifiedAt     *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	LastLoginAt         *time.Time
	DeletionRequestedAt *time.Time
	DeletedAt           *time.Time
}

const userCols = `id, email, normalized_email, display_name, status, role,
	apple_subject, dev_name, email_verified_at, created_at, updated_at,
	last_login_at, deletion_requested_at, deleted_at`

func scanUser(r row) (*User, error) {
	u := &User{}
	err := r.Scan(&u.ID, &u.Email, &u.NormalizedEmail, &u.DisplayName, &u.Status,
		&u.Role, &u.AppleSubject, &u.DevName, &u.EmailVerifiedAt, &u.CreatedAt,
		&u.UpdatedAt, &u.LastLoginAt, &u.DeletionRequestedAt, &u.DeletedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// DisplayLabel mirrors the Python User.display_label: the iOS account row
// shows this. Email accounts show the email; legacy rows show their origin.
func (u *User) DisplayLabel() string {
	if u.NormalizedEmail != nil && *u.NormalizedEmail != "" {
		if u.Email != nil && *u.Email != "" {
			return *u.Email
		}
		return *u.NormalizedEmail
	}
	if u.DevName != nil && *u.DevName != "" {
		return "dev:" + *u.DevName
	}
	if u.AppleSubject != nil {
		return "apple:" + *u.AppleSubject
	}
	return u.ID.String()
}

// CanSignIn: only active (verified) accounts authenticate.
func (u *User) CanSignIn() bool { return u.Status == "active" && u.DeletedAt == nil }

func GetUserByID(ctx context.Context, q Q, id uuid.UUID) (*User, error) {
	r := q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
	u, err := scanUser(r)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return u, err
}

func GetUserByNormalizedEmail(ctx context.Context, q Q, email string) (*User, error) {
	r := q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE normalized_email = $1`, email)
	u, err := scanUser(r)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return u, err
}

func GetUserByAppleSubject(ctx context.Context, q Q, subject string) (*User, error) {
	r := q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE apple_subject = $1`, subject)
	u, err := scanUser(r)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return u, err
}

func GetUserByDevName(ctx context.Context, q Q, name string) (*User, error) {
	r := q.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE dev_name = $1 AND deleted_at IS NULL`, name)
	u, err := scanUser(r)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return u, err
}

func CreateUser(ctx context.Context, q Q, u *User) error {
	return q.QueryRow(ctx, `
		INSERT INTO users (id, email, normalized_email, display_name, status, role,
			apple_subject, dev_name, email_verified_at, created_at, updated_at,
			last_login_at, deletion_requested_at, deleted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING created_at, updated_at`,
		u.ID, u.Email, u.NormalizedEmail, u.DisplayName, u.Status, u.Role,
		u.AppleSubject, u.DevName, u.EmailVerifiedAt, u.CreatedAt, u.UpdatedAt,
		u.LastLoginAt, u.DeletionRequestedAt, u.DeletedAt,
	).Scan(&u.CreatedAt, &u.UpdatedAt)
}

func TouchLastLogin(ctx context.Context, q Q, id uuid.UUID) error {
	_, err := q.Exec(ctx,
		`UPDATE users SET last_login_at = now(), updated_at = now() WHERE id = $1`, id)
	return err
}

func UpdateUserStatus(ctx context.Context, q Q, id uuid.UUID, status string) error {
	_, err := q.Exec(ctx,
		`UPDATE users SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

func MarkEmailVerified(ctx context.Context, q Q, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE users SET email_verified_at = now(), status = 'active',
			updated_at = now()
		WHERE id = $1`, id)
	return err
}

func SoftDeleteUser(ctx context.Context, q Q, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE users SET deleted_at = now(), status = 'deleted',
			deletion_requested_at = COALESCE(deletion_requested_at, now()),
			updated_at = now()
		WHERE id = $1`, id)
	return err
}

// PurgeUserSyncData hard-deletes synced classroom data + change log +
// idempotency ledger. The account and its devices survive. Same scope as
// the Python purge_user_sync_data. Attachment AND material FILES are not
// touched here (the caller reaps them post-commit via the storage store).
func PurgeUserSyncData(ctx context.Context, q Q, userID uuid.UUID) error {
	for _, table := range []string{
		"transcript_entries", "bookmarks", "favorite_sessions", "session_notes",
		"study_reviews", "session_attachments", "classroom_sessions", "courses",
		"glossary_terms", "study_cards", "study_tasks",
		"transcript_corrections",
		"schedule_exceptions", "course_schedules",
		"material_pages", "material_annotations", "course_materials",
		"assistant_messages", "assistant_threads",
		"study_plan_items", "study_plans", "exam_topics", "exams",
		"study_activities",
		"sync_changes", "processed_operations",
	} {
		if _, err := q.Exec(ctx,
			`DELETE FROM `+table+` WHERE user_id = $1`, userID); err != nil {
			return err
		}
	}
	return nil
}
