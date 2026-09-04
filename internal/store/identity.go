package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// --- Password credentials ---------------------------------------------------

func UpsertPasswordCredential(ctx context.Context, q Q, userID uuid.UUID, hash string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO password_credentials (user_id, password_hash, password_changed_at, created_at, updated_at)
		VALUES ($1, $2, now(), now(), now())
		ON CONFLICT (user_id) DO UPDATE
			SET password_hash = EXCLUDED.password_hash,
			    password_changed_at = now(),
			    updated_at = now()`, userID, hash)
	return err
}

func GetPasswordHash(ctx context.Context, q Q, userID uuid.UUID) (string, time.Time, error) {
	var hash string
	var changed time.Time
	err := q.QueryRow(ctx,
		`SELECT password_hash, password_changed_at FROM password_credentials WHERE user_id = $1`,
		userID).Scan(&hash, &changed)
	if err == pgx.ErrNoRows {
		return "", time.Time{}, ErrNotFound
	}
	return hash, changed, err
}

// --- Email challenges (verification codes) ----------------------------------

type EmailChallenge struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	Purpose      string
	TokenHash    string
	TargetEmail  *string // change_email only: the new address being verified
	ExpiresAt    time.Time
	AttemptCount int
	ConsumedAt   *time.Time
	CreatedAt    time.Time
}

// InvalidatePendingChallenges marks all live challenges of a user+purpose
// consumed so a freshly issued code supersedes older ones.
func InvalidatePendingChallenges(ctx context.Context, q Q, userID uuid.UUID, purpose string) error {
	_, err := q.Exec(ctx, `
		UPDATE email_challenges SET consumed_at = now()
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`, userID, purpose)
	return err
}

func CreateEmailChallenge(ctx context.Context, q Q, c *EmailChallenge) error {
	return q.QueryRow(ctx, `
		INSERT INTO email_challenges (id, user_id, purpose, target_email, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		RETURNING id, created_at`,
		c.ID, c.UserID, c.Purpose, c.TargetEmail, c.TokenHash, c.ExpiresAt,
	).Scan(&c.ID, &c.CreatedAt)
}

// LatestLiveChallenge returns the newest unconsumed challenge.
func LatestLiveChallenge(ctx context.Context, q Q, userID uuid.UUID, purpose string) (*EmailChallenge, error) {
	c := &EmailChallenge{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, purpose, target_email, token_hash, expires_at, attempt_count, consumed_at, created_at
		FROM email_challenges
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, userID, purpose,
	).Scan(&c.ID, &c.UserID, &c.Purpose, &c.TargetEmail, &c.TokenHash, &c.ExpiresAt, &c.AttemptCount, &c.ConsumedAt, &c.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

// BumpChallengeAttempt increments the wrong-code counter; returns the new count.
func BumpChallengeAttempt(ctx context.Context, q Q, id uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		UPDATE email_challenges SET attempt_count = attempt_count + 1
		WHERE id = $1 RETURNING attempt_count`, id).Scan(&n)
	return n, err
}

func ConsumeChallenge(ctx context.Context, q Q, id uuid.UUID) error {
	ct, err := q.Exec(ctx, `
		UPDATE email_challenges SET consumed_at = now()
		WHERE id = $1 AND consumed_at IS NULL`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// InvalidatePasswordResetToken consumes a reset token by ID WITHOUT using
// it — the delivery-failure path calls this so a never-mailed token cannot
// linger as valid, and the per-user resend cooldown no longer applies to a
// token the user never received.
func InvalidatePasswordResetToken(ctx context.Context, q Q, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE password_reset_tokens SET consumed_at = now()
		WHERE id = $1 AND consumed_at IS NULL`, id)
	return err
}

// --- Login identities (bind/unbind, "sign-in methods") ----------------------

type AuthIdentity struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Provider        string
	ProviderSubject string
	CreatedAt       time.Time
	LastUsedAt      *time.Time
}

// ListAuthIdentities returns the user's bound login identities.
func ListAuthIdentities(ctx context.Context, q Q, userID uuid.UUID) ([]*AuthIdentity, error) {
	rs, err := q.Query(ctx, `
		SELECT id, user_id, provider, provider_subject, created_at, last_used_at
		FROM auth_identities WHERE user_id = $1 ORDER BY provider`, userID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*AuthIdentity
	for rs.Next() {
		i := &AuthIdentity{}
		if err := rs.Scan(&i.ID, &i.UserID, &i.Provider, &i.ProviderSubject, &i.CreatedAt, &i.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rs.Err()
}

// GetAuthIdentityBySubject finds an identity by (provider, subject).
func GetAuthIdentityBySubject(ctx context.Context, q Q, provider, subject string) (*AuthIdentity, error) {
	i := &AuthIdentity{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, provider, provider_subject, created_at, last_used_at
		FROM auth_identities WHERE provider = $1 AND provider_subject = $2`,
		provider, subject,
	).Scan(&i.ID, &i.UserID, &i.Provider, &i.ProviderSubject, &i.CreatedAt, &i.LastUsedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	return i, err
}

// BindAuthIdentity links a provider identity to a user (the unique
// (provider, provider_subject) constraint arbitrates races).
func BindAuthIdentity(ctx context.Context, q Q, userID uuid.UUID, provider, subject string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO auth_identities (id, user_id, provider, provider_subject, created_at, last_used_at)
		VALUES ($1, $2, $3, $4, now(), now())`,
		uuid.New(), userID, provider, subject)
	return err
}

// UnbindAuthIdentity removes one provider identity of a user.
func UnbindAuthIdentity(ctx context.Context, q Q, userID uuid.UUID, provider string) error {
	ct, err := q.Exec(ctx, `
		DELETE FROM auth_identities
		WHERE user_id = $1 AND provider = $2`, userID, provider)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// HasPasswordCredential reports whether the user can sign in with a password.
func HasPasswordCredential(ctx context.Context, q Q, userID uuid.UUID) (bool, error) {
	var one int
	err := q.QueryRow(ctx,
		`SELECT 1 FROM password_credentials WHERE user_id = $1`, userID).Scan(&one)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// UpdateUserEmail atomically replaces the login email (used by the verified
// email-change flow).
func UpdateUserEmail(ctx context.Context, q Q, userID uuid.UUID, email, normalized string) error {
	_, err := q.Exec(ctx, `
		UPDATE users SET email = $2, normalized_email = $3, updated_at = now()
		WHERE id = $1`, userID, email, normalized)
	return err
}

// UpdateDisplayName replaces the display name (length enforced by callers).
func UpdateDisplayName(ctx context.Context, q Q, userID uuid.UUID, displayName string) error {
	_, err := q.Exec(ctx, `
		UPDATE users SET display_name = $2, updated_at = now()
		WHERE id = $1`, userID, displayName)
	return err
}

// CountUserDevices returns the user's device rows (revoked included) and
// the count of devices with a live refresh token.
func CountUserDevices(ctx context.Context, q Q, userID uuid.UUID) (total, liveSessions int, err error) {
	err = q.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM devices WHERE user_id = $1),
			(SELECT count(*) FROM refresh_tokens rt
				WHERE rt.user_id = $1 AND rt.revoked_at IS NULL AND rt.expires_at > now())`,
		userID).Scan(&total, &liveSessions)
	return total, liveSessions, err
}

// --- Password reset tokens --------------------------------------------------

type PasswordResetToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  string
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}

func CreatePasswordResetToken(ctx context.Context, q Q, t *PasswordResetToken) error {
	return q.QueryRow(ctx, `
		INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, now())
		RETURNING created_at`,
		t.ID, t.UserID, t.TokenHash, t.ExpiresAt).Scan(&t.CreatedAt)
}

// PasswordResetTokenState describes a reset token for the deep-link landing
// page (valid | expired | used | unknown). The token itself is high-entropy
// and single-use, so revealing its state by direct query is safe.
type PasswordResetTokenState struct {
	Status     string // valid | expired | used | unknown
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

func LookupPasswordResetTokenState(ctx context.Context, q Q, tokenHash string) (*PasswordResetTokenState, error) {
	s := &PasswordResetTokenState{}
	err := q.QueryRow(ctx, `
		SELECT expires_at, consumed_at FROM password_reset_tokens
		WHERE token_hash = $1`, tokenHash,
	).Scan(&s.ExpiresAt, &s.ConsumedAt)
	if err == pgx.ErrNoRows {
		s.Status = "unknown"
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case s.ConsumedAt != nil:
		s.Status = "used"
	case s.ExpiresAt.Before(time.Now()):
		s.Status = "expired"
	default:
		s.Status = "valid"
	}
	return s, nil
}

func ConsumePasswordResetToken(ctx context.Context, q Q, tokenHash string) (uuid.UUID, error) {
	var userID uuid.UUID
	var expiresAt time.Time
	var consumedAt *time.Time
	err := q.QueryRow(ctx, `
		SELECT user_id, expires_at, consumed_at FROM password_reset_tokens
		WHERE token_hash = $1`, tokenHash,
	).Scan(&userID, &expiresAt, &consumedAt)
	if err == pgx.ErrNoRows {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	if consumedAt != nil {
		return uuid.Nil, ErrNotFound // single-use: replay treated as unknown
	}
	if expiresAt.Before(time.Now()) {
		return uuid.Nil, ErrTokenExpired
	}
	ct, err := q.Exec(ctx, `
		UPDATE password_reset_tokens SET consumed_at = now()
		WHERE id = (SELECT id FROM password_reset_tokens WHERE token_hash = $1)
		  AND consumed_at IS NULL`, tokenHash)
	if err != nil || ct.RowsAffected() == 0 {
		return uuid.Nil, ErrNotFound
	}
	return userID, nil
}

// --- Login events (audit) ---------------------------------------------------

func RecordLoginEvent(ctx context.Context, q Q, userID *uuid.UUID, emailHash, deviceID, ipHash, result string) error {
	var uid any
	if userID != nil {
		uid = *userID
	}
	_, err := q.Exec(ctx, `
		INSERT INTO login_events (user_id, normalized_email_hash, device_id, ip_hash, result, created_at)
		VALUES ($1, $2, $3, $4, $5, now())`, uid, emailHash, deviceID, ipHash, result)
	return err
}

// CountRecentFailuresByEmail counts failed logins for an email hash within
// the window (progressive-delay / short-block input).
func CountRecentFailuresByEmail(ctx context.Context, q Q, emailHash string, window time.Duration) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM login_events
		WHERE normalized_email_hash = $1 AND result IN ('invalid_password','unknown_email','unverified','suspended')
		  AND created_at > now() - make_interval(secs => $2)`, emailHash, window.Seconds()).Scan(&n)
	return n, err
}

func CountRecentFailuresByIP(ctx context.Context, q Q, ipHash string, window time.Duration) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM login_events
		WHERE ip_hash = $1 AND result IN ('invalid_password','unknown_email','unverified','suspended')
		  AND created_at > now() - make_interval(secs => $2)`, ipHash, window.Seconds()).Scan(&n)
	return n, err
}

// --- Invitations ------------------------------------------------------------

type Invitation struct {
	Code      string
	Note      string
	CreatedBy *uuid.UUID
	MaxUses   int
	UsedCount int
	ExpiresAt *time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

func CreateInvitation(ctx context.Context, q Q, inv *Invitation) error {
	return q.QueryRow(ctx, `
		INSERT INTO invitations (code, note, created_by, max_uses, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, now()) RETURNING created_at`,
		inv.Code, inv.Note, inv.CreatedBy, inv.MaxUses, inv.ExpiresAt).Scan(&inv.CreatedAt)
}

// ConsumeInvitation atomically increments used_count under validity checks.
func ConsumeInvitation(ctx context.Context, q Q, code string) error {
	ct, err := q.Exec(ctx, `
		UPDATE invitations SET used_count = used_count + 1
		WHERE code = $1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		  AND used_count < max_uses`, code)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func ListInvitations(ctx context.Context, q Q, limit int) ([]*Invitation, error) {
	rs, err := q.Query(ctx, `
		SELECT code, note, created_by, max_uses, used_count, expires_at, revoked_at, created_at
		FROM invitations ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*Invitation
	for rs.Next() {
		i := &Invitation{}
		if err := rs.Scan(&i.Code, &i.Note, &i.CreatedBy, &i.MaxUses, &i.UsedCount, &i.ExpiresAt, &i.RevokedAt, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rs.Err()
}

func RevokeInvitation(ctx context.Context, q Q, code string) error {
	ct, err := q.Exec(ctx,
		`UPDATE invitations SET revoked_at = now() WHERE code = $1 AND revoked_at IS NULL`, code)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
