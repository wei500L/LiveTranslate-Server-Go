// Package admin is the operations backend: session-cookie auth with CSRF,
// Argon2id admin accounts, user management (status actions, forced
// logouts, device revocation, mail issuing, deletion scheduling),
// invitations and the audit log. It renders html/template pages — no JS
// framework — and NEVER exposes classroom transcript content
// (russian/chinese text): user detail shows counts only. There is no
// account impersonation.
package admin

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/internal/store"
)

// AdminAccount is a row of admin_accounts.
type AdminAccount struct {
	ID             uuid.UUID
	Username       string
	PasswordHash   string
	TOTPSecret     *string
	FailedAttempts int
	LockedUntil    *time.Time
	CreatedAt      time.Time
	LastLoginAt    *time.Time
}

const adminCols = `id, username, password_hash, totp_secret, failed_attempts,
	locked_until, created_at, last_login_at`

func scanAdmin(r interface{ Scan(...any) error }) (*AdminAccount, error) {
	a := &AdminAccount{}
	err := r.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.TOTPSecret, &a.FailedAttempts,
		&a.LockedUntil, &a.CreatedAt, &a.LastLoginAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func GetAdminByUsername(ctx context.Context, q store.Q, username string) (*AdminAccount, error) {
	row := q.QueryRow(ctx, `SELECT `+adminCols+` FROM admin_accounts WHERE username = $1`, username)
	a, err := scanAdmin(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return a, nil
}

func GetAdminByID(ctx context.Context, q store.Q, id uuid.UUID) (*AdminAccount, error) {
	row := q.QueryRow(ctx, `SELECT `+adminCols+` FROM admin_accounts WHERE id = $1`, id)
	a, err := scanAdmin(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return a, nil
}

// CreateAdmin inserts a new admin account (used by the create-admin CLI and
// the admin UI). Fails with a unique violation when the username exists.
func CreateAdmin(ctx context.Context, q store.Q, username, passwordHash string, totpSecret *string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO admin_accounts (id, username, password_hash, totp_secret, created_at)
		VALUES ($1, $2, $3, $4, now())`,
		uuid.New(), username, passwordHash, totpSecret)
	return err
}

// UpdateAdminTOTP sets or clears the TOTP secret.
func UpdateAdminTOTP(ctx context.Context, q store.Q, id uuid.UUID, secret *string) error {
	_, err := q.Exec(ctx, `UPDATE admin_accounts SET totp_secret = $2 WHERE id = $1`, id, secret)
	return err
}

// RecordAdminLoginSuccess clears the failure counter.
func RecordAdminLoginSuccess(ctx context.Context, q store.Q, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE admin_accounts SET failed_attempts = 0, locked_until = NULL,
			last_login_at = now() WHERE id = $1`, id)
	return err
}

// RecordAdminLoginFailure increments the counter and locks the account
// progressively (1 min, 5 min, 15 min, 60 min) — administrators are few and
// targeted, so a temporary lockout is the right trade here (unlike user
// accounts, which only get progressive delays).
func RecordAdminLoginFailure(ctx context.Context, q store.Q, id uuid.UUID, attempts int) error {
	lock := time.Duration(0)
	switch {
	case attempts >= 8:
		lock = 60 * time.Minute
	case attempts >= 6:
		lock = 15 * time.Minute
	case attempts >= 4:
		lock = 5 * time.Minute
	case attempts >= 2:
		lock = time.Minute
	}
	var lockedUntil *time.Time
	if lock > 0 {
		t := time.Now().Add(lock)
		lockedUntil = &t
	}
	_, err := q.Exec(ctx, `
		UPDATE admin_accounts SET failed_attempts = failed_attempts + 1,
			locked_until = $2 WHERE id = $1`, id, lockedUntil)
	return err
}

// --- Sessions -----------------------------------------------------------------

type AdminSession struct {
	ID        uuid.UUID
	AdminID   uuid.UUID
	TokenHash string
	CSRFToken string
	IPHash    string
	UserAgent string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

func CreateAdminSession(ctx context.Context, q store.Q, s *AdminSession) error {
	return q.QueryRow(ctx, `
		INSERT INTO admin_sessions (id, admin_id, token_hash, csrf_token, ip_hash,
			user_agent, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		RETURNING created_at`,
		s.ID, s.AdminID, s.TokenHash, s.CSRFToken, s.IPHash, s.UserAgent, s.ExpiresAt,
	).Scan(&s.CreatedAt)
}

// GetLiveAdminSession fetches a session by token hash; revoked/expired rows
// count as not found.
func GetLiveAdminSession(ctx context.Context, q store.Q, tokenHash string) (*AdminSession, error) {
	row := q.QueryRow(ctx, `
		SELECT id, admin_id, token_hash, csrf_token, ip_hash, user_agent,
			expires_at, revoked_at, created_at
		FROM admin_sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`, tokenHash)
	s := &AdminSession{}
	err := row.Scan(&s.ID, &s.AdminID, &s.TokenHash, &s.CSRFToken, &s.IPHash, &s.UserAgent,
		&s.ExpiresAt, &s.RevokedAt, &s.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s, nil
}

func RevokeAdminSession(ctx context.Context, q store.Q, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE admin_sessions SET revoked_at = now() WHERE id = $1`, id)
	return err
}

func RevokeAllAdminSessions(ctx context.Context, q store.Q, adminID uuid.UUID) error {
	_, err := q.Exec(ctx,
		`UPDATE admin_sessions SET revoked_at = now() WHERE admin_id = $1 AND revoked_at IS NULL`, adminID)
	return err
}

// --- Invitations ----------------------------------------------------------------

type Invitation struct {
	Code      string
	Note      string
	MaxUses   int
	UsedCount int
	ExpiresAt *time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

func ListInvitations(ctx context.Context, q store.Q, limit int) ([]Invitation, error) {
	rows, err := q.Query(ctx, `
		SELECT code, note, max_uses, used_count, expires_at, revoked_at, created_at
		FROM invitations ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invitation
	for rows.Next() {
		var i Invitation
		if err := rows.Scan(&i.Code, &i.Note, &i.MaxUses, &i.UsedCount,
			&i.ExpiresAt, &i.RevokedAt, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func CreateInvitation(ctx context.Context, q store.Q, code, note string, maxUses int, ttl time.Duration, createdBy *uuid.UUID) error {
	var expires *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expires = &t
	}
	_, err := q.Exec(ctx, `
		INSERT INTO invitations (code, note, created_by, max_uses, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, now())`, code, note, createdBy, maxUses, expires)
	return err
}

func RevokeInvitation(ctx context.Context, q store.Q, code string) error {
	_, err := q.Exec(ctx, `UPDATE invitations SET revoked_at = now() WHERE code = $1 AND revoked_at IS NULL`, code)
	return err
}

// --- Audit feed -------------------------------------------------------------------

type AuditEventRow struct {
	ID           int64
	ActorType    string
	ActorID      *uuid.UUID
	Action       string
	TargetUserID *uuid.UUID
	Reason       string
	IPHash       string
	CreatedAt    time.Time
}

func ListAuditEvents(ctx context.Context, q store.Q, limit int) ([]AuditEventRow, error) {
	rows, err := q.Query(ctx, `
		SELECT id, actor_type, actor_id, action, target_user_id, reason, ip_hash, created_at
		FROM audit_events ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEventRow
	for rows.Next() {
		var e AuditEventRow
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorID, &e.Action, &e.TargetUserID,
			&e.Reason, &e.IPHash, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAuditEventsForTarget returns the newest events where the user is the
// target — the security timeline on the user-detail page. `actorAdmin`
// selects the admin-operations slice instead of the security slice.
func ListAuditEventsForTarget(ctx context.Context, q store.Q, userID uuid.UUID, actorAdmin bool, limit int) ([]AuditEventRow, error) {
	actor := "user"
	if actorAdmin {
		actor = "admin"
	}
	rows, err := q.Query(ctx, `
		SELECT id, actor_type, actor_id, action, target_user_id, reason, ip_hash, created_at
		FROM audit_events
		WHERE target_user_id = $1 AND actor_type = $2
		ORDER BY id DESC LIMIT $3`, userID, actor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEventRow
	for rows.Next() {
		var e AuditEventRow
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorID, &e.Action, &e.TargetUserID,
			&e.Reason, &e.IPHash, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
