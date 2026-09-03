package store

import "github.com/jackc/pgx/v5"

// pgxErrNoRows is compared against via pgx.ErrNoRows directly in callers;
// this alias keeps the store package's error vocabulary in one place.
var _ = pgx.ErrNoRows

// Typed rotation errors mapped by the auth service to HTTP 401 reasons.
var (
	ErrReuseDetected      = &RotateError{"refresh token reuse detected"}
	ErrTokenExpired       = &RotateError{"refresh token expired"}
	ErrAccountUnavailable = &RotateError{"account or device no longer exists"}
)

type RotateError struct{ Msg string }

func (e *RotateError) Error() string { return e.Msg }
