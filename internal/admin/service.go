package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/internal/audit"
	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	passwordpkg "livetranslate/server/internal/password"
	"livetranslate/server/internal/store"
	"livetranslate/server/internal/token"
)

var (
	ErrBadCredentials = errors.New("invalid username or password")
	ErrLocked         = errors.New("account temporarily locked")
	ErrBadTOTP        = errors.New("invalid TOTP code")
)

// Service implements admin authentication and the user/invitation/audit
// operations. It deliberately shares NOTHING with the /v1 token stack: no
// JWTs, no user accounts — admins live in their own table with cookie
// sessions. The auth service reference exists ONLY for the admin-issued
// mail flows (resend verification / send password reset) and the deletion
// scheduling — it is never used to impersonate or sign in a user.
type Service struct {
	cfg   *config.Config
	db    *store.DB
	audit *audit.Recorder
	auth  *auth.Service
	log   *slog.Logger
}

func NewService(cfg *config.Config, db *store.DB, auditor *audit.Recorder, authSvc *auth.Service) *Service {
	return &Service{cfg: cfg, db: db, audit: auditor, auth: authSvc,
		log: slog.Default().With("component", "admin")}
}

const sessionCookie = "lt_admin_session"

// Login verifies username + password (+ TOTP when configured) and creates a
// cookie session. It returns the opaque session token and the CSRF token;
// the HTTP layer turns the former into a Secure/HttpOnly/SameSite cookie.
func (s *Service) Login(ctx context.Context, username, totpCode, passwd, ipHash, userAgent string) (sessionToken, csrfToken string, err error) {
	q := s.db.Q()
	adm, err := GetAdminByUsername(ctx, q, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Burn comparable Argon2id cost so timing cannot enumerate
			// admin usernames.
			passwordpkg.VerifyDummy(passwordpkg.Params{
				MemoryKiB: s.cfg.Argon2MemoryKiB, Iterations: s.cfg.Argon2Iterations,
				Parallel: s.cfg.Argon2Parallel, SaltLen: 16, KeyLen: 32,
			})
			return "", "", ErrBadCredentials
		}
		return "", "", err
	}
	if adm.LockedUntil != nil && adm.LockedUntil.After(time.Now()) {
		return "", "", ErrLocked
	}
	ok, err := passwordpkg.Verify(passwd, adm.PasswordHash)
	if err != nil || !ok {
		if err == nil {
			_ = RecordAdminLoginFailure(ctx, q, adm.ID, adm.FailedAttempts+1)
		}
		return "", "", ErrBadCredentials
	}
	if adm.TOTPSecret != nil {
		if !VerifyTOTP(*adm.TOTPSecret, totpCode) {
			_ = RecordAdminLoginFailure(ctx, q, adm.ID, adm.FailedAttempts+1)
			return "", "", ErrBadTOTP
		}
	}

	opaque := token.NewOpaqueToken()
	sess := &AdminSession{
		ID:        uuid.New(),
		AdminID:   adm.ID,
		TokenHash: token.HashToken(opaque),
		CSRFToken: config.RandomHex(32),
		IPHash:    ipHash,
		UserAgent: truncate(userAgent, 256),
		ExpiresAt: time.Now().Add(s.cfg.SessionTTL),
	}
	if err := CreateAdminSession(ctx, q, sess); err != nil {
		return "", "", err
	}
	if err := RecordAdminLoginSuccess(ctx, q, adm.ID); err != nil {
		return "", "", err
	}
	if err := s.auditAdmin(ctx, adm.ID, audit.ActionAdminLogin, nil, ""); err != nil {
		s.log.Error("audit write failed", "err", err.Error())
	}
	// The cookie carries the OPAQUE token; only its SHA-256 lives in the DB.
	return opaque, sess.CSRFToken, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Logout revokes the session named by the cookie.
func (s *Service) Logout(ctx context.Context, r *http.Request) error {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	q := s.db.Q()
	sess, err := GetLiveAdminSession(ctx, q, token.HashToken(c.Value))
	if err != nil {
		return nil // already gone: idempotent
	}
	if err := RevokeAdminSession(ctx, q, sess.ID); err != nil {
		return err
	}
	return s.auditAdmin(ctx, sess.AdminID, audit.ActionAdminLogout, nil, "")
}

// ResolveSession maps a request to a live session (nil when absent/expired).
func (s *Service) ResolveSession(ctx context.Context, r *http.Request) *AdminSession {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	sess, err := GetLiveAdminSession(ctx, s.db.Q(), token.HashToken(c.Value))
	if err != nil {
		return nil
	}
	return sess
}

// ValidCSRF enforces the double-submit pattern: the form field must match
// the session's stored token.
func (s *Service) ValidCSRF(sess *AdminSession, r *http.Request) bool {
	return sess != nil && r.FormValue("csrf_token") == sess.CSRFToken
}

func (s *Service) auditAdmin(ctx context.Context, adminID uuid.UUID, action string, target *uuid.UUID, reason string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		return s.audit.Record(ctx, q, "admin", &adminID, action, target, reason, "", nil, nil)
	})
}

// --- User actions ---------------------------------------------------------------

// SuspendUser sets status='suspended' and revokes all refresh tokens so the
// user's devices lose access at their next refresh.
func (s *Service) SuspendUser(ctx context.Context, adminID, userID uuid.UUID, reason string) error {
	if reason == "" {
		reason = "suspended by admin"
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		if err := store.UpdateUserStatus(ctx, q, userID, "suspended"); err != nil {
			return err
		}
		if err := store.RevokeAllUserRefreshTokens(ctx, q, userID); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, "admin", &adminID, audit.ActionAdminSuspendUser, &userID, reason, "", nil, nil)
	})
	return err
}

// ReactivateUser restores status='active' (only from suspended).
func (s *Service) ReactivateUser(ctx context.Context, adminID, userID uuid.UUID, reason string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		if err := store.UpdateUserStatus(ctx, q, userID, "active"); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, "admin", &adminID, audit.ActionAdminReactivateUser, &userID, reason, "", nil, nil)
	})
}

// ForceLogout revokes every refresh token of the user (access tokens die
// within ACCESS_TOKEN_TTL).
func (s *Service) ForceLogout(ctx context.Context, adminID, userID uuid.UUID, reason string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		if err := store.RevokeAllUserRefreshTokens(ctx, q, userID); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, "admin", &adminID, audit.ActionAdminForceLogout, &userID, reason, "", nil, nil)
	})
}

// RevokeUserDevice signs out ONE device of the user.
func (s *Service) RevokeUserDevice(ctx context.Context, adminID, userID, deviceID uuid.UUID, reason string) error {
	if err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		return RevokeUserDevice(ctx, q, userID, deviceID)
	}); err != nil {
		return err
	}
	return s.auditAdmin(ctx, adminID, audit.ActionAdminRevokeDevice, &userID, reason)
}

// ResendVerification re-issues the email-verification code for a pending
// user (admin escalation path; bypasses the user-driven cooldown).
func (s *Service) ResendVerification(ctx context.Context, adminID, userID uuid.UUID) error {
	ok, err := s.auth.IssueVerificationForUser(ctx, userID)
	if err != nil {
		return err
	}
	if !ok {
		return store.ErrNotFound
	}
	return s.auditAdmin(ctx, adminID, audit.ActionAdminResendVerify, &userID, "")
}

// SendPasswordReset mails a fresh reset link for the user.
func (s *Service) SendPasswordReset(ctx context.Context, adminID, userID uuid.UUID) error {
	ok, err := s.auth.IssuePasswordResetForUser(ctx, userID)
	if err != nil {
		return err
	}
	if !ok {
		return store.ErrNotFound
	}
	return s.auditAdmin(ctx, adminID, audit.ActionAdminSendReset, &userID, "")
}

// RequestDeletion schedules account deletion (pending_deletion + session
// revocation + notice mail).
func (s *Service) RequestDeletion(ctx context.Context, adminID, userID uuid.UUID, reason string) error {
	return s.auth.StartAccountDeletion(ctx, adminID, userID, reason, "")
}

// CancelDeletion restores a pending_deletion account.
func (s *Service) CancelDeletion(ctx context.Context, adminID, userID uuid.UUID, reason string) error {
	return s.auth.CancelAccountDeletion(ctx, adminID, userID, reason, "")
}

// DeleteUser purges sync data, revokes tokens and soft-deletes the account
// (same semantics as the user's own DELETE /v1/account).
func (s *Service) DeleteUser(ctx context.Context, adminID, userID uuid.UUID, reason string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
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
		return s.audit.Record(ctx, q, "admin", &adminID, audit.ActionAdminDeleteUser, &userID, reason, "", before,
			map[string]string{"status": "deleted"})
	})
}

// --- Views -----------------------------------------------------------------------

func (s *Service) Dashboard(ctx context.Context) (*DashboardStats, error) {
	d, err := LoadDashboardStats(ctx, s.db.Q())
	if err != nil {
		return nil, err
	}
	d.RegistrationMode = s.cfg.RegistrationMode
	return d, nil
}

// ListUsers returns one page of the filtered/sorted user list + the total
// (for pagination). Query parameters are preserved by the handler.
func (s *Service) ListUsers(ctx context.Context, query UserQuery) ([]*UserSummary, int, error) {
	const perPage = 25
	if query.Page < 1 {
		query.Page = 1
	}
	q := s.db.Q()
	total, err := CountUsers(ctx, q, query)
	if err != nil {
		return nil, 0, err
	}
	users, err := ListUsers(ctx, q, query, perPage, (query.Page-1)*perPage)
	return users, total, err
}

// UserDetail assembles the detail page model (counts + providers + audit
// timelines). No transcript text is selected.
func (s *Service) UserDetail(ctx context.Context, id uuid.UUID) (*UserDetail, error) {
	q := s.db.Q()
	d, err := GetUserDetail(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if d.SecurityEvents, err = ListAuditEventsForTarget(ctx, q, id, false, 20); err != nil {
		return nil, err
	}
	if d.AdminActions, err = ListAuditEventsForTarget(ctx, q, id, true, 20); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Service) InvitationList(ctx context.Context) ([]Invitation, error) {
	return ListInvitations(ctx, s.db.Q(), 100)
}

func (s *Service) CreateInvitation(ctx context.Context, adminID uuid.UUID, note string, maxUses int, ttl time.Duration) (string, error) {
	code := config.RandomHex(12)
	q := s.db.Q()
	aid := adminID
	if err := CreateInvitation(ctx, q, code, note, maxUses, ttl, &aid); err != nil {
		return "", err
	}
	if err := s.auditAdmin(ctx, adminID, audit.ActionInvitationNew, nil, note); err != nil {
		s.log.Error("audit write failed", "err", err.Error())
	}
	return code, nil
}

func (s *Service) RevokeInvitation(ctx context.Context, adminID uuid.UUID, code string) error {
	q := s.db.Q()
	if err := RevokeInvitation(ctx, q, code); err != nil {
		return err
	}
	return s.auditAdmin(ctx, adminID, audit.ActionInvitationRevoke, nil, code)
}

func (s *Service) AuditFeed(ctx context.Context) ([]AuditEventRow, error) {
	return ListAuditEvents(ctx, s.db.Q(), 200)
}

// MaskEmailForLog keeps emails out of ordinary admin page logs; the user
// list itself shows them (operations require identifying accounts), but the
// access log never will.
func MaskEmailForLog(email string) string {
	return auth.HashPII(email)
}
