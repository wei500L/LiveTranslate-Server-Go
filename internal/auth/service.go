// Package auth implements the account lifecycle: register, email verify,
// login, refresh rotation, logout, password forgot/reset/change, Apple
// login (preserved) and bind/unbind. Security posture:
//
//   - Argon2id PHC hashes, per-password salt, constant-time compare;
//   - unknown emails run a dummy Argon2id (anti-enumeration timing);
//   - verification codes / reset tokens stored hash-only, single-use,
//     short-TTL, resend-cooldown, capped attempts;
//   - unified "if the account exists we sent mail" responses;
//   - progressive delay via login_events (no permanent lockout);
//   - password reset revokes every refresh token of the account.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/sha3"

	"livetranslate/server/internal/audit"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/mail"
	"livetranslate/server/internal/password"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
	"livetranslate/server/internal/token"
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrEmailTaken         = errors.New("email already registered")
	ErrNotVerified        = errors.New("email not verified")
	ErrSuspended          = errors.New("account suspended")
	ErrWeakPassword       = errors.New("password rejected")
	ErrBadCode            = errors.New("invalid or expired code")
	ErrTooManyAttempts    = errors.New("too many attempts")
	ErrResendCooldown     = errors.New("please wait before requesting another code")
	ErrRateLimited        = errors.New("too many attempts, try later")
	ErrNoMailTransport    = errors.New("mail transport unavailable")
)

// Service wires the pieces together.
type Service struct {
	cfg    *config.Config
	db     *store.DB
	tokens *token.Manager
	mailer mail.Sender
	audit  *audit.Recorder
	log    *slog.Logger
}

func NewService(cfg *config.Config, db *store.DB, tokens *token.Manager, mailer mail.Sender, auditor *audit.Recorder) *Service {
	return &Service{cfg: cfg, db: db, tokens: tokens, mailer: mailer, audit: auditor,
		log: slog.Default().With("component", "auth")}
}

// --- Request/response DTOs ----------------------------------------------------

type RegisterRequest struct {
	Email            string             `json:"email"`
	Password         string             `json:"password"`
	DisplayName      string             `json:"displayName"`
	InvitationCode   string             `json:"invitationCode,omitempty"`
	Device           syncpkg.DeviceInfo `json:"device"`
	AcceptedTermsVer string             `json:"acceptedTermsVersion,omitempty"`
}

type LoginRequest struct {
	Email    string             `json:"email"`
	Password string             `json:"password"`
	Device   syncpkg.DeviceInfo `json:"device"`
}

type VerifyEmailRequest struct {
	Email  string             `json:"email"`
	Code   string             `json:"code"`
	Device syncpkg.DeviceInfo `json:"device"`
}

type ResendRequest struct {
	Email string `json:"email"`
}

type ForgotPasswordRequest struct {
	Email string `json:"email"`
}

type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type DeviceSessionDTO struct {
	DeviceID   string    `json:"deviceId"`
	Name       string    `json:"name"`
	AppVersion string    `json:"appVersion"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	Current    bool      `json:"current"`
}

// LoginResponse is the full login payload. Tokens are also present as
// accessToken/refreshToken for direct compat with SyncTokenPairDTO.
type LoginResponse struct {
	AccessToken   string              `json:"accessToken"`
	RefreshToken  string              `json:"refreshToken"`
	TokenType     string              `json:"tokenType"`
	ExpiresIn     int                 `json:"expiresIn"`
	UserID        string              `json:"userId"`
	User          *syncpkg.PublicUser `json:"user"`
	DeviceSession *DeviceSessionDTO   `json:"deviceSession"`
	EmailVerified bool                `json:"emailVerified"`
	IsNewUser     bool                `json:"isNewUser,omitempty"`
}

// --- Helpers --------------------------------------------------------------------

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// NormalizeEmail lowercases and trims dots/plus addressing NO — only trim
// and lowercase. The stored normalized form is what uniqueness uses.
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func ValidEmail(raw string) bool {
	raw = strings.TrimSpace(raw)
	return len(raw) <= 320 && emailRe.MatchString(raw)
}

// HashPII turns an email or IP into an unreversible digest for the
// login_events table (the table itself then carries no readable PII).
func HashPII(s string) string {
	sum := sha3.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

func currentParams(cfg *config.Config) password.Params {
	return password.Params{
		MemoryKiB: cfg.Argon2MemoryKiB, Iterations: cfg.Argon2Iterations,
		Parallel: cfg.Argon2Parallel, SaltLen: 16, KeyLen: 32,
	}
}

// --- Register --------------------------------------------------------------------

// Register creates a pending user + password credential + verification
// code, and emails the code. It does NOT issue tokens (email must be
// verified first). The response never reveals whether the email exists.
func (s *Service) Register(ctx context.Context, req *RegisterRequest, ip string) (*LoginResponse, error) {
	email := NormalizeEmail(req.Email)
	if !ValidEmail(email) {
		// Malformed address: no account state is touched, no mail leaves.
		// The handler's uniform response keeps register enumeration-safe
		// (format validity is public knowledge, so nothing is leaked).
		return &LoginResponse{EmailVerified: false}, nil
	}
	if err := password.Validate(req.Password, email, req.DisplayName); err != nil {
		var pe *password.PolicyError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("%w: %s", ErrWeakPassword, pe.Reason)
		}
		return nil, ErrWeakPassword
	}
	// Production without SMTP: refuse rather than pretend to deliver.
	if !s.mailer.Configured() && !s.cfg.DevMode {
		return nil, ErrNoMailTransport
	}

	hash, err := password.Hash(req.Password, currentParams(s.cfg))
	if err != nil {
		return nil, err
	}

	emailHash := HashPII(email)
	created := false
	var plainCode string
	err = s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		// Existing ACTIVE user with this email → enumeration-safe refusal.
		existing, err := store.GetUserByNormalizedEmail(ctx, q, email)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if existing != nil && existing.DeletedAt == nil && existing.Status != "pending" {
			// Occupied: do NOT create; caller returns a generic "check your
			// email" style success to avoid enumeration. Burn the same Argon2id
			// cost so timing cannot distinguish this path from a real signup.
			password.VerifyDummy(currentParams(s.cfg))
			return ErrEmailTaken
		}
		if existing != nil && existing.Status == "pending" {
			// A stale pending signup: reuse the row (id keeps prior codes
			// dead via challenge invalidation below).
			if err := store.UpsertPasswordCredential(ctx, q, existing.ID, hash); err != nil {
				return err
			}
			existing.DisplayName = req.DisplayName
			_, err := q.Exec(ctx, `UPDATE users SET display_name = $2 WHERE id = $1`,
				existing.ID, req.DisplayName)
			if err != nil {
				return err
			}
			plainCode, err = s.createChallengeTx(ctx, q, existing.ID)
			return err
		}
		// Invitation gate.
		if s.cfg.RequireInvitation {
			if req.InvitationCode == "" {
				return fmt.Errorf("invitation code required")
			}
			if err := store.ConsumeInvitation(ctx, q, req.InvitationCode); err != nil {
				return fmt.Errorf("invalid invitation code")
			}
		}
		now := time.Now()
		u := &store.User{
			ID: uuid.New(), Email: &email, NormalizedEmail: &email,
			DisplayName: req.DisplayName, Status: "pending", Role: "user",
			CreatedAt: now, UpdatedAt: now,
		}
		if err := store.CreateUser(ctx, q, u); err != nil {
			return err
		}
		if err := store.UpsertPasswordCredential(ctx, q, u.ID, hash); err != nil {
			return err
		}
		var cerr error
		plainCode, cerr = s.createChallengeTx(ctx, q, u.ID)
		if cerr != nil {
			return cerr
		}
		created = true
		var uid = u.ID
		return s.audit.Record(ctx, q, "user", &uid, audit.ActionRegister, &uid, "", HashPII(ip), nil,
			map[string]string{"status": "pending", "email_domain": domainOf(email)})
	})
	if err != nil {
		return nil, err
	}
	_ = emailHash
	// Delivery outside the transaction: a transport failure must not roll
	// back the challenge — the user can hit resend after the cooldown.
	if plainCode != "" {
		s.sendCodeEmail(ctx, email, plainCode)
	}
	return &LoginResponse{EmailVerified: false, IsNewUser: created}, nil
}

func domainOf(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return email[i+1:]
	}
	return ""
}

// createChallengeTx invalidates prior codes and inserts a fresh one,
// returning the PLAINTEXT code so the caller can mail it after commit.
// Storage sees only the hash.
func (s *Service) createChallengeTx(ctx context.Context, q store.Q, userID uuid.UUID) (string, error) {
	if err := store.InvalidatePendingChallenges(ctx, q, userID, "verify_email"); err != nil {
		return "", err
	}
	code := token.NewEmailCode()
	ch := &store.EmailChallenge{
		ID: uuid.New(), UserID: userID, Purpose: "verify_email",
		TokenHash: token.HashToken(code),
		ExpiresAt: time.Now().Add(s.cfg.EmailVerifyTTL),
	}
	return code, store.CreateEmailChallenge(ctx, q, ch)
}

// sendCodeEmail delivers a verification code. Failures are logged, never
// fatal: the challenge exists and resend is available after the cooldown.
func (s *Service) sendCodeEmail(ctx context.Context, email, plain string) {
	body := "Your LiveTranslate verification code is: " + plain + "\n\n" +
		"It expires in 10 minutes. If you did not request it, ignore this email."
	if err := s.mailer.Send(ctx, email, "LiveTranslate 验证码 / Verification code", body); err != nil {
		s.log.Error("verification email delivery failed", "err", err.Error())
	}
}

// issueAndSendVerification is the register/resend flow: one code, sent.
func (s *Service) issueAndSendVerification(ctx context.Context, email string, ip string) error {
	if !s.mailer.Configured() && !s.cfg.DevMode {
		return ErrNoMailTransport
	}
	var plain string
	err := s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		u, err := store.GetUserByNormalizedEmail(ctx, q, email)
		if err != nil || u == nil {
			return nil // silent: anti-enumeration
		}
		if u.Status != "pending" {
			return nil // already verified or otherwise: no-op, same response
		}
		// Resend cooldown: newest live challenge must be older than the cooldown.
		if ch, err := store.LatestLiveChallenge(ctx, q, u.ID, "verify_email"); err == nil && ch != nil {
			if time.Since(ch.CreatedAt) < s.cfg.ResendCooldown {
				return ErrResendCooldown
			}
		}
		var cerr error
		plain, cerr = s.createChallengeTx(ctx, q, u.ID)
		return cerr
	})
	if err != nil {
		return err
	}
	if plain != "" {
		s.sendCodeEmail(ctx, email, plain)
	}
	return nil
}

// VerifyEmail consumes the code and activates the account, then issues the
// first token pair + device session.
func (s *Service) VerifyEmail(ctx context.Context, req *VerifyEmailRequest, ip string) (*LoginResponse, error) {
	email := NormalizeEmail(req.Email)
	q0 := s.db.Q()
	u, err := store.GetUserByNormalizedEmail(ctx, q0, email)
	if err != nil || u == nil {
		// Anti-enumeration: burn comparable time.
		password.VerifyDummy(currentParams(s.cfg))
		return nil, ErrBadCode
	}
	if u.Status != "pending" || u.DeletedAt != nil {
		return nil, ErrBadCode
	}

	var resp *LoginResponse
	err = s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		ch, err := store.LatestLiveChallenge(ctx, q, u.ID, "verify_email")
		if err != nil || ch == nil {
			return ErrBadCode
		}
		if time.Now().After(ch.ExpiresAt) {
			return ErrBadCode
		}
		if ch.AttemptCount >= 5 {
			return ErrTooManyAttempts
		}
		if token.HashToken(strings.TrimSpace(req.Code)) != ch.TokenHash {
			// The failed-attempt bump MUST survive the rollback of this
			// transaction — writing it through the tx would discard it on
			// return ErrBadCode, and the attempt cap would never accumulate.
			_, _ = store.BumpChallengeAttempt(ctx, s.db.Q(), ch.ID)
			return ErrBadCode
		}
		if err := store.ConsumeChallenge(ctx, q, ch.ID); err != nil {
			return ErrBadCode
		}
		if err := store.MarkEmailVerified(ctx, q, u.ID); err != nil {
			return err
		}
		uid := u.ID
		if err := s.audit.Record(ctx, q, "user", &uid, audit.ActionEmailVerified, &uid, "", HashPII(ip), nil, nil); err != nil {
			return err
		}
		// Device + first token pair.
		fresh, err := store.GetUserByID(ctx, q, u.ID)
		if err != nil {
			return err
		}
		resp, err = s.issueTokensForDevice(ctx, q, fresh, req.Device2(), ip)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Device2 returns the device payload carried by the verify request.
func (r *VerifyEmailRequest) Device2() syncpkg.DeviceInfo { return r.Device }

// Resend issues a new code if allowed. Response is always generic.
func (s *Service) Resend(ctx context.Context, req *ResendRequest, ip string) error {
	email := NormalizeEmail(req.Email)
	if !ValidEmail(email) {
		return nil // uniform response
	}
	return s.issueAndSendVerification(ctx, email, ip)
}

// --- Login --------------------------------------------------------------------

func (s *Service) Login(ctx context.Context, req *LoginRequest, ip string) (*LoginResponse, error) {
	email := NormalizeEmail(req.Email)
	emailHash := HashPII(email)
	ipHash := HashPII(ip)
	params := currentParams(s.cfg)

	q0 := s.db.Q()
	u, err := store.GetUserByNormalizedEmail(ctx, q0, email)

	// Anti-enumeration: identical Argon2id cost for unknown emails.
	if err != nil || u == nil {
		password.VerifyDummy(params)
		_ = store.RecordLoginEvent(ctx, q0, nil, emailHash, req.Device.ClientDeviceID, ipHash, "unknown_email")
		return nil, ErrInvalidCredentials
	}
	if u.DeletedAt != nil || u.Status == "deleted" {
		password.VerifyDummy(params)
		_ = store.RecordLoginEvent(ctx, q0, nil, emailHash, req.Device.ClientDeviceID, ipHash, "unknown_email")
		return nil, ErrInvalidCredentials
	}
	if u.Status == "pending" {
		password.VerifyDummy(params)
		_ = store.RecordLoginEvent(ctx, q0, &u.ID, emailHash, req.Device.ClientDeviceID, ipHash, "unverified")
		return nil, ErrNotVerified
	}
	if u.Status == "suspended" || u.Status == "pending_deletion" {
		password.VerifyDummy(params)
		_ = store.RecordLoginEvent(ctx, q0, &u.ID, emailHash, req.Device.ClientDeviceID, ipHash, "suspended")
		return nil, ErrSuspended
	}

	// Progressive delay: many recent failures for this email slow the
	// response (bounded), without any permanent lockout.
	fails, err := store.CountRecentFailuresByEmail(ctx, q0, emailHash, s.cfg.LoginFailWindow)
	if err == nil && fails >= s.cfg.LoginFailMax {
		// Short, bounded delay; still no lockout flag on the account.
		delay := time.Duration(fails) * 250 * time.Millisecond
		if delay > 8*time.Second {
			delay = 8 * time.Second
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ipFails, err := store.CountRecentFailuresByIP(ctx, q0, ipHash, s.cfg.LoginFailWindow)
	if err == nil && ipFails >= s.cfg.IPFailMax {
		_ = store.RecordLoginEvent(ctx, q0, &u.ID, emailHash, req.Device.ClientDeviceID, ipHash, "rate_limited")
		return nil, ErrRateLimited
	}

	hash, _, err := store.GetPasswordHash(ctx, q0, u.ID)
	if err != nil {
		password.VerifyDummy(params)
		_ = store.RecordLoginEvent(ctx, q0, &u.ID, emailHash, req.Device.ClientDeviceID, ipHash, "invalid_password")
		return nil, ErrInvalidCredentials
	}
	ok, err := password.Verify(req.Password, hash)
	if err != nil || !ok {
		_ = store.RecordLoginEvent(ctx, q0, &u.ID, emailHash, req.Device.ClientDeviceID, ipHash, "invalid_password")
		return nil, ErrInvalidCredentials
	}

	var resp *LoginResponse
	err = s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		// Transparent re-hash on parameter upgrade.
		if password.NeedsRehash(hash, params) {
			if newHash, err := password.Hash(req.Password, params); err == nil {
				_ = store.UpsertPasswordCredential(ctx, q, u.ID, newHash)
			}
		}
		if err := store.TouchLastLogin(ctx, q, u.ID); err != nil {
			return err
		}
		uid := u.ID
		_ = store.RecordLoginEvent(ctx, q, &uid, emailHash, req.Device.ClientDeviceID, ipHash, "success")
		var verr error
		resp, verr = s.issueTokensForDevice(ctx, q, u, req.Device, ip)
		return verr
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// issueTokensForDevice creates/refreshes the device row and issues a token
// pair. Runs INSIDE a transaction (q is a tx-bound query interface).
func (s *Service) issueTokensForDevice(ctx context.Context, q store.Q, u *store.User, device syncpkg.DeviceInfo, ip string) (*LoginResponse, error) {
	if u.Status != "active" || u.DeletedAt != nil {
		return nil, ErrSuspended
	}
	// Verify-email may arrive without device info; devices.client_device_id
	// must never be empty (it keys UNIQUE(user_id, client_device_id) and is
	// what /v1/me/devices shows), so synthesize one for that case.
	clientDeviceID := device.ClientDeviceID
	if clientDeviceID == "" {
		clientDeviceID = "verify-" + config.RandomHex(8)
	}
	dev, err := store.GetOrCreateDevice(ctx, q, u.ID, clientDeviceID, device.DisplayName, device.AppVersion)
	if err != nil {
		return nil, err
	}
	access, ttl, err := s.tokens.NewAccessToken(u.ID.String(), dev.ID.String(), u.Role)
	if err != nil {
		return nil, err
	}
	refreshPlain, refreshHash, refreshTTL := s.tokens.NewRefreshToken()
	rt := &store.RefreshToken{
		ID: uuid.New(), UserID: u.ID, DeviceID: dev.ID,
		TokenHash: refreshHash, ExpiresAt: time.Now().Add(refreshTTL),
	}
	if err := store.InsertRefreshToken(ctx, q, rt); err != nil {
		return nil, err
	}
	verified := u.EmailVerifiedAt != nil
	return &LoginResponse{
		AccessToken: access, RefreshToken: refreshPlain, TokenType: "Bearer",
		ExpiresIn: int(ttl.Seconds()), UserID: u.ID.String(),
		User: publicUser(u),
		DeviceSession: &DeviceSessionDTO{
			DeviceID: dev.ID.String(), Name: dev.DisplayName,
			AppVersion: dev.AppVersion, LastSeenAt: dev.LastSeenAt, Current: true,
		},
		EmailVerified: verified,
	}, nil
}

func publicUser(u *store.User) *syncpkg.PublicUser {
	return &syncpkg.PublicUser{
		UserID: u.ID.String(), Email: u.Email, DisplayName: u.DisplayName,
		Status: u.Status, EmailVerified: u.EmailVerifiedAt != nil,
	}
}

// --- Refresh / logout -------------------------------------------------------------

// Refresh rotates a refresh token. Reuse detection: replaying an
// already-rotated or revoked token revokes EVERY token of that device.
//
// Transaction discipline: the reuse-revocation is written and COMMITTED
// inside the transaction — only after commit does ErrReuseDetected surface.
// (Returning the error from the tx body would roll the revocation back and
// leave the thief's successor tokens alive.)
func (s *Service) Refresh(ctx context.Context, plainRefresh string, ip string) (*LoginResponse, error) {
	tokenHash := token.HashToken(plainRefresh)
	var resp *LoginResponse
	reuse := false
	err := s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		old, err := store.LockRefreshToken(ctx, q, tokenHash)
		if err != nil {
			return err // unknown token
		}
		if old.RevokedAt != nil || old.ReplacedBy != nil {
			if err := store.RevokeDeviceTokens(ctx, q, old.DeviceID); err != nil {
				return err
			}
			reuse = true
			return nil // COMMIT the revocation
		}
		if old.ExpiresAt.Before(time.Now()) {
			return store.ErrTokenExpired
		}
		user, err := store.GetUserByID(ctx, q, old.UserID)
		if err != nil {
			return err
		}
		if user.DeletedAt != nil || user.Status == "suspended" ||
			user.Status == "pending_deletion" || user.Status == "deleted" ||
			(user.Status == "pending" && user.EmailVerifiedAt == nil) {
			return store.ErrAccountUnavailable
		}

		// Valid: issue the successor pair in the same transaction.
		access, ttl, err := s.tokens.NewAccessToken(user.ID.String(), old.DeviceID.String(), user.Role)
		if err != nil {
			return err
		}
		plain, hash, refreshTTL := s.tokens.NewRefreshToken()
		successor := &store.RefreshToken{
			ID: uuid.New(), UserID: user.ID, DeviceID: old.DeviceID,
			TokenHash: hash, ExpiresAt: time.Now().Add(refreshTTL),
		}
		if err := store.InsertRefreshToken(ctx, q, successor); err != nil {
			return err
		}
		if err := store.MarkReplaced(ctx, q, old.ID, successor.ID); err != nil {
			return err
		}
		_ = store.TouchDeviceSeen(ctx, q, old.DeviceID)
		_ = store.TouchLastLogin(ctx, q, user.ID)
		resp = &LoginResponse{
			AccessToken: access, RefreshToken: plain, TokenType: "Bearer",
			ExpiresIn: int(ttl.Seconds()), UserID: user.ID.String(),
			User: publicUser(user),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if reuse {
		return nil, store.ErrReuseDetected
	}
	return resp, nil
}

func (s *Service) Logout(ctx context.Context, plainRefresh string, revokeDevice bool) error {
	return s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		return store.RevokeRefreshToken(ctx, q, token.HashToken(plainRefresh), revokeDevice)
	})
}

// LogoutAllUser revokes every refresh token of the user (current device
// included — the client then drops its local tokens).
func (s *Service) LogoutAllUser(ctx context.Context, userID uuid.UUID, ip string) error {
	return s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		if err := store.RevokeAllUserRefreshTokens(ctx, q, userID); err != nil {
			return err
		}
		uid := userID
		return s.audit.Record(ctx, q, "user", &uid, audit.ActionLogoutAll, &uid, "", HashPII(ip), nil, nil)
	})
}

// --- Password forgot / reset / change ------------------------------------------------

// ForgotPassword always returns nil error to the caller (which answers the
// generic message). A reset token is created and mailed only for a
// qualifying account.
func (s *Service) ForgotPassword(ctx context.Context, rawEmail, ip string) error {
	if !s.mailer.Configured() && !s.cfg.DevMode {
		return ErrNoMailTransport
	}
	email := NormalizeEmail(rawEmail)
	if !ValidEmail(email) {
		return nil
	}
	plain := ""
	err := s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		u, err := store.GetUserByNormalizedEmail(ctx, q, email)
		if err != nil || u == nil {
			return nil
		}
		if u.DeletedAt != nil || u.Status != "active" || u.EmailVerifiedAt == nil {
			return nil // pending/suspended/deleted: uniform silence
		}
		// Cooldown on resets: newest live reset for the user.
		var last time.Time
		e := q.QueryRow(ctx, `
			SELECT created_at FROM password_reset_tokens
			WHERE user_id = $1 AND consumed_at IS NULL
			ORDER BY created_at DESC LIMIT 1`, u.ID).Scan(&last)
		if e == nil && time.Since(last) < s.cfg.ResendCooldown {
			return nil // silent cooldown: same response
		}
		plain = token.NewOpaqueToken()
		t := &store.PasswordResetToken{
			ID: uuid.New(), UserID: u.ID, TokenHash: token.HashToken(plain),
			ExpiresAt: time.Now().Add(s.cfg.PasswordResetTTL),
		}
		return store.CreatePasswordResetToken(ctx, q, t)
	})
	if err != nil {
		return err
	}
	if plain != "" {
		body := "Reset your LiveTranslate password with this token (valid 30 minutes):\n\n" +
			plain + "\n\nIf you did not request this, ignore this email — your password is unchanged."
		if err := s.mailer.Send(ctx, email, "LiveTranslate 密码重置 / Password reset", body); err != nil {
			s.log.Error("reset email delivery failed", "err", err.Error())
		}
	}
	return nil
}

// ResetPassword consumes the token, re-hashes the new password and revokes
// ALL refresh tokens of the account.
func (s *Service) ResetPassword(ctx context.Context, req *ResetPasswordRequest, ip string) error {
	if err := password.Validate(req.NewPassword, "", ""); err != nil {
		return ErrWeakPassword
	}
	tokenHash := token.HashToken(strings.TrimSpace(req.Token))
	return s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		userID, err := store.ConsumePasswordResetToken(ctx, q, tokenHash)
		if err != nil {
			return ErrBadCode
		}
		u, err := store.GetUserByID(ctx, q, userID)
		if err != nil || u == nil || u.DeletedAt != nil {
			return ErrBadCode
		}
		hash, err := password.Hash(req.NewPassword, currentParams(s.cfg))
		if err != nil {
			return err
		}
		if err := store.UpsertPasswordCredential(ctx, q, userID, hash); err != nil {
			return err
		}
		if err := store.RevokeAllUserRefreshTokens(ctx, q, userID); err != nil {
			return err
		}
		uid := userID
		return s.audit.Record(ctx, q, "user", &uid, audit.ActionPasswordReset, &uid, "", HashPII(ip), nil, nil)
	})
}

// ChangePassword verifies the current password, swaps the hash and revokes
// every OTHER device session (the current device keeps its session per the
// unified security policy: re-auth just happened).
func (s *Service) ChangePassword(ctx context.Context, userID, currentDeviceID uuid.UUID, req *ChangePasswordRequest, ip string) error {
	q0 := s.db.Q()
	u, err := store.GetUserByID(ctx, q0, userID)
	if err != nil || u == nil {
		return ErrInvalidCredentials
	}
	hash, _, err := store.GetPasswordHash(ctx, q0, userID)
	if err != nil {
		password.VerifyDummy(currentParams(s.cfg))
		return ErrInvalidCredentials
	}
	ok, err := password.Verify(req.CurrentPassword, hash)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	if err := password.Validate(req.NewPassword, deref(u.NormalizedEmail), u.DisplayName); err != nil {
		return ErrWeakPassword
	}
	newHash, err := password.Hash(req.NewPassword, currentParams(s.cfg))
	if err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		if err := store.UpsertPasswordCredential(ctx, q, userID, newHash); err != nil {
			return err
		}
		// Revoke other devices' sessions; keep the current device.
		if err := store.RevokeAllUserRefreshTokensExceptDevice(ctx, q, userID, currentDeviceID); err != nil {
			return err
		}
		uid := userID
		return s.audit.Record(ctx, q, "user", &uid, audit.ActionPasswordChange, &uid, "", HashPII(ip), nil,
			map[string]string{"other_sessions_revoked": "true"})
	})
}

func deref(sp *string) string {
	if sp == nil {
		return ""
	}
	return *sp
}

// --- Apple login (preserved) --------------------------------------------------------

// AppleLogin creates/fetches the account by verified Apple subject and
// issues a token pair. Mirror of the Python endpoint's semantics.
func (s *Service) AppleLogin(ctx context.Context, appleSubject string, device syncpkg.DeviceInfo, ip string) (*LoginResponse, bool, error) {
	var resp *LoginResponse
	created := false
	err := s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		u, err := store.GetUserByAppleSubject(ctx, q, appleSubject)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if u == nil {
			now := time.Now()
			u = &store.User{
				ID: uuid.New(), DisplayName: "", Status: "active", Role: "user",
				AppleSubject: &appleSubject, CreatedAt: now, UpdatedAt: now,
				// Apple accounts are email-verified by Apple's token (the
				// relay email, when present, is verified by Apple).
				EmailVerifiedAt: &now,
			}
			if err := store.CreateUser(ctx, q, u); err != nil {
				return err
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO auth_identities (id, user_id, provider, provider_subject, created_at, last_used_at)
				VALUES ($1, $2, 'apple', $3, now(), now())`,
				uuid.New(), u.ID, appleSubject); err != nil {
				return err
			}
			created = true
		} else {
			if u.DeletedAt != nil {
				return ErrSuspended
			}
			if u.Status != "active" {
				return ErrSuspended
			}
			_, _ = q.Exec(ctx, `
				INSERT INTO auth_identities (id, user_id, provider, provider_subject, created_at, last_used_at)
				VALUES ($1, $2, 'apple', $3, now(), now())
				ON CONFLICT (provider, provider_subject) DO UPDATE SET last_used_at = now()`,
				uuid.New(), u.ID, appleSubject)
		}
		resp, err = s.issueTokensForDevice(ctx, q, u, device, ip)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	resp.IsNewUser = created
	return resp, created, nil
}

// DevLogin is the local-development shortcut (enabled only when
// DEV_LOGIN_ENABLED=true): find-or-create the "dev:<name>" account and issue
// a token pair, mirroring the Python /v1/auth/dev endpoint.
func (s *Service) DevLogin(ctx context.Context, devName string, device syncpkg.DeviceInfo, ip string) (*LoginResponse, error) {
	if devName == "" {
		return nil, ErrInvalidCredentials
	}
	var resp *LoginResponse
	err := s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		u, err := store.GetUserByDevName(ctx, q, devName)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if u == nil {
			now := time.Now()
			u = &store.User{
				ID: uuid.New(), DisplayName: devName, Status: "active", Role: "user",
				DevName: &devName, CreatedAt: now, UpdatedAt: now,
			}
			if err := store.CreateUser(ctx, q, u); err != nil {
				return err
			}
		} else {
			if u.DeletedAt != nil || u.Status != "active" {
				return ErrSuspended
			}
		}
		resp, err = s.issueTokensForDevice(ctx, q, u, device, ip)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// --- Devices -----------------------------------------------------------------------

type DeviceInfoDTO struct {
	DeviceID   string     `json:"deviceId"`
	Name       string     `json:"name"`
	AppVersion string     `json:"appVersion"`
	LastSeenAt time.Time  `json:"lastSeenAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

func (s *Service) ListDevices(ctx context.Context, userID uuid.UUID) ([]*DeviceInfoDTO, error) {
	devs, err := store.ListUserDevices(ctx, s.db.Q(), userID)
	if err != nil {
		return nil, err
	}
	out := make([]*DeviceInfoDTO, 0, len(devs))
	for _, d := range devs {
		out = append(out, &DeviceInfoDTO{
			DeviceID: d.ID.String(), Name: d.DisplayName, AppVersion: d.AppVersion,
			LastSeenAt: d.LastSeenAt, RevokedAt: d.RevokedAt,
		})
	}
	return out, nil
}

// RevokeDevice revokes one device's sessions (logout of a remote device).
func (s *Service) RevokeDevice(ctx context.Context, userID, deviceID uuid.UUID, ip string) error {
	return s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
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
		uid := userID
		return s.audit.Record(ctx, q, "user", &uid, "user.revoke_device", &uid, "", HashPII(ip), nil, nil)
	})
}

// --- Account deletion / purge ---------------------------------------------------------

// DeleteAccount purges sync data, revokes tokens and soft-deletes the user.
func (s *Service) DeleteAccount(ctx context.Context, userID uuid.UUID, ip string) error {
	return s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		u, err := store.GetUserByID(ctx, q, userID)
		if err != nil {
			return err
		}
		before := map[string]string{"status": u.Status}
		if err := store.PurgeUserSyncData(ctx, q, userID); err != nil {
			return err
		}
		if err := store.RevokeAllUserRefreshTokens(ctx, q, userID); err != nil {
			return err
		}
		if err := store.SoftDeleteUser(ctx, q, userID); err != nil {
			return err
		}
		uid := userID
		return s.audit.Record(ctx, q, "user", &uid, audit.ActionUserDeleted, &uid, "", HashPII(ip), before,
			map[string]string{"status": "deleted"})
	})
}

// PurgeCloudData deletes synced data, keeping the account + devices.
func (s *Service) PurgeCloudData(ctx context.Context, userID uuid.UUID, ip string) error {
	return s.db.Tx(ctx, func(tx pgxTx) error {
		q := store.TxQ(tx)
		if err := store.PurgeUserSyncData(ctx, q, userID); err != nil {
			return err
		}
		uid := userID
		return s.audit.Record(ctx, q, "user", &uid, audit.ActionCloudDataPurge, &uid, "", HashPII(ip), nil, nil)
	})
}
