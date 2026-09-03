// Package admin is the operations backend: session-cookie auth with CSRF,
// Argon2id admin accounts, user management (status actions, forced
// logouts), invitations and the audit log. It renders html/template pages
// — no JS framework — and NEVER exposes classroom transcript content
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

// --- Admin-facing user views (no transcript content) ---------------------------

// UserSummary is one row of the user list.
type UserSummary struct {
	ID            uuid.UUID
	Email         *string
	DisplayName   string
	Status        string
	EmailVerified bool
	CreatedAt     time.Time
	LastLoginAt   *time.Time
	DeletedAt     *time.Time
	SessionCount  int
	EntryCount    int
}

// UserDetail adds device/session metadata for the detail page.
type UserDetail struct {
	UserSummary
	Devices []DeviceSummary
}

type DeviceSummary struct {
	ID         uuid.UUID
	DeviceName string
	AppVersion string
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

// ListUsers returns a page of users with aggregate counts. Search matches
// email/display name prefix. NO transcript text is ever selected here.
func ListUsers(ctx context.Context, q store.Q, search string, limit, offset int) ([]UserSummary, error) {
	rows, err := q.Query(ctx, `
		SELECT u.id, u.email, u.display_name, u.status,
			(u.email_verified_at IS NOT NULL) AS email_verified,
			u.created_at, u.last_login_at, u.deleted_at,
			(SELECT count(*) FROM refresh_tokens rt
				JOIN devices d ON d.id = rt.device_id
				WHERE rt.user_id = u.id AND rt.revoked_at IS NULL AND rt.expires_at > now()) AS session_count,
			(SELECT count(*) FROM transcript_entries te
				WHERE te.user_id = u.id AND te.deleted_at IS NULL) AS entry_count
		FROM users u
		WHERE ($1 = '' OR u.normalized_email LIKE $1 || '%' OR u.display_name ILIKE '%' || $1 || '%')
		ORDER BY u.created_at DESC
		LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var u UserSummary
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Status, &u.EmailVerified,
			&u.CreatedAt, &u.LastLoginAt, &u.DeletedAt, &u.SessionCount, &u.EntryCount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func CountUsers(ctx context.Context, q store.Q, search string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM users
		WHERE ($1 = '' OR normalized_email LIKE $1 || '%' OR display_name ILIKE '%' || $1 || '%')`,
		search).Scan(&n)
	return n, err
}

// GetUserDetail loads the user row plus device/session metadata. Counts
// only — transcript content is deliberately out of scope for admins.
func GetUserDetail(ctx context.Context, q store.Q, id uuid.UUID) (*UserDetail, error) {
	row := q.QueryRow(ctx, `
		SELECT u.id, u.email, u.display_name, u.status,
			(u.email_verified_at IS NOT NULL) AS email_verified,
			u.created_at, u.last_login_at, u.deleted_at,
			(SELECT count(*) FROM transcript_entries te
				WHERE te.user_id = u.id AND te.deleted_at IS NULL)
		FROM users u WHERE u.id = $1`, id)
	d := &UserDetail{}
	if err := row.Scan(&d.ID, &d.Email, &d.DisplayName, &d.Status, &d.EmailVerified,
		&d.CreatedAt, &d.LastLoginAt, &d.DeletedAt, &d.EntryCount); err != nil {
		if err == pgx.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
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
	return d, devs.Err()
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
