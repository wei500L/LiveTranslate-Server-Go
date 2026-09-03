package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Device struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	ClientDeviceID string
	DisplayName    string
	AppVersion     string
	LastSeenAt     time.Time
	RevokedAt      *time.Time
}

// GetOrCreateDevice mirrors the Python get_or_create_device: a revoked
// device re-authenticating is un-revoked in place (its old tokens are
// already dead); an existing device's display metadata is refreshed.
// `created` reports whether the row was newly inserted (drives the
// new-device notification mail).
func GetOrCreateDevice(ctx context.Context, q Q, userID uuid.UUID, clientDeviceID, displayName, appVersion string) (*Device, bool, error) {
	d := &Device{}
	created := false
	err := q.QueryRow(ctx, `
		INSERT INTO devices (id, user_id, client_device_id, display_name, app_version, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (user_id, client_device_id) DO UPDATE
			SET revoked_at = NULL,
			    display_name = EXCLUDED.display_name,
			    app_version  = EXCLUDED.app_version,
			    last_seen_at = now()
		RETURNING id, user_id, client_device_id, display_name, app_version, last_seen_at, revoked_at,
			(xmax = 0) AS inserted`,
		uuid.New(), userID, clientDeviceID, displayName, appVersion,
	).Scan(&d.ID, &d.UserID, &d.ClientDeviceID, &d.DisplayName, &d.AppVersion, &d.LastSeenAt, &d.RevokedAt, &created)
	if err != nil {
		return nil, false, err
	}
	return d, created, nil
}

func ListUserDevices(ctx context.Context, q Q, userID uuid.UUID) ([]*Device, error) {
	rs, err := q.Query(ctx, `
		SELECT id, user_id, client_device_id, display_name, app_version, last_seen_at, revoked_at
		FROM devices WHERE user_id = $1 ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*Device
	for rs.Next() {
		d := &Device{}
		if err := rs.Scan(&d.ID, &d.UserID, &d.ClientDeviceID, &d.DisplayName, &d.AppVersion, &d.LastSeenAt, &d.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rs.Err()
}

func TouchDeviceSeen(ctx context.Context, q Q, deviceID uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE devices SET last_seen_at = now() WHERE id = $1`, deviceID)
	return err
}

// --- Refresh tokens ---------------------------------------------------------

type RefreshToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	DeviceID   uuid.UUID
	TokenHash  string
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *uuid.UUID
}

func InsertRefreshToken(ctx context.Context, q Q, rt *RefreshToken) error {
	// created_at is not kept on the struct; the column has no default, so it
	// is set here and read back only to satisfy the RETURNING contract.
	var createdAt time.Time
	return q.QueryRow(ctx, `
		INSERT INTO refresh_tokens (id, user_id, device_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, now())
		RETURNING created_at`,
		rt.ID, rt.UserID, rt.DeviceID, rt.TokenHash, rt.ExpiresAt,
	).Scan(&createdAt)
}

// LockRefreshToken loads a refresh token row FOR UPDATE. The caller's
// transaction owns all subsequent decisions.
func LockRefreshToken(ctx context.Context, q Q, tokenHash string) (*RefreshToken, error) {
	old := &RefreshToken{}
	err := q.QueryRow(ctx, `
		SELECT id, user_id, device_id, token_hash, expires_at, revoked_at, replaced_by
		FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`, tokenHash,
	).Scan(&old.ID, &old.UserID, &old.DeviceID, &old.TokenHash, &old.ExpiresAt, &old.RevokedAt, &old.ReplacedBy)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return old, nil
}

// RevokeDeviceTokens revokes every live refresh token of a device — the
// reuse-detection response. The caller MUST commit this write (see
// auth.Service.Refresh: the reuse error is surfaced only AFTER commit).
func RevokeDeviceTokens(ctx context.Context, q Q, deviceID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE device_id = $1 AND revoked_at IS NULL`, deviceID)
	return err
}

func MarkReplaced(ctx context.Context, q Q, oldID, successorID uuid.UUID) error {
	_, err := q.Exec(ctx,
		`UPDATE refresh_tokens SET replaced_by = $2 WHERE id = $1`, oldID, successorID)
	return err
}

// RevokeRefreshToken revokes a single token; revokeDevice revokes the
// token's device (tokens + device row).
func RevokeRefreshToken(ctx context.Context, q Q, tokenHash string, revokeDevice bool) error {
	var deviceID uuid.UUID
	err := q.QueryRow(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
		RETURNING device_id`, tokenHash).Scan(&deviceID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if revokeDevice {
		if _, err := q.Exec(ctx, `
			UPDATE refresh_tokens SET revoked_at = now()
			WHERE device_id = $1 AND revoked_at IS NULL`, deviceID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx,
			`UPDATE devices SET revoked_at = now() WHERE id = $1`, deviceID); err != nil {
			return err
		}
	}
	return nil
}

// RevokeAllUserRefreshTokens revokes every live token of a user (password
// change/reset, admin action, account deletion).
func RevokeAllUserRefreshTokens(ctx context.Context, q Q, userID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// RevokeAllUserRefreshTokensExceptCurrentDevice revokes every live token
// EXCEPT those of keepDeviceID (password change keeps the current device).
func RevokeAllUserRefreshTokensExceptDevice(ctx context.Context, q Q, userID, keepDeviceID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE user_id = $1 AND device_id <> $2 AND revoked_at IS NULL`, userID, keepDeviceID)
	return err
}
