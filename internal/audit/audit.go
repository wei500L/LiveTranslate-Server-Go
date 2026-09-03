// Package audit writes append-only audit events (admin operations and
// security events). Snapshots are small status summaries — never
// transcript text, never credentials, never tokens.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"livetranslate/server/internal/store"
)

const (
	ActionUserSuspend         = "user.suspend"
	ActionUserUnsuspend       = "user.unsuspend"
	ActionUserRevokeAll       = "user.revoke_all_sessions"
	ActionUserRevokeDevice    = "user.revoke_device"
	ActionUserDeleteStart     = "user.delete_start"
	ActionUserDeleteCancel    = "user.delete_cancel"
	ActionUserDeleted         = "user.deleted"
	ActionInvitationNew       = "invitation.create"
	ActionInvitationRevoke    = "invitation.revoke"
	ActionPasswordChange      = "account.password_change"
	ActionPasswordReset       = "account.password_reset"
	ActionEmailVerified       = "account.email_verified"
	ActionEmailChangeStart    = "account.email_change_start"
	ActionEmailChanged        = "account.email_changed"
	ActionProfileUpdate       = "account.profile_update"
	ActionRegister            = "account.register"
	ActionLogin               = "account.login"
	ActionLogoutAll           = "account.logout_all"
	ActionAppleBind           = "account.apple_bind"
	ActionAppleUnbind         = "account.apple_unbind"
	ActionCloudDataPurge      = "account.cloud_data_purge"
	ActionAdminLogin          = "admin.login"
	ActionAdminLoginFail      = "admin.login_failed"
	ActionAdminLogout         = "admin.logout"
	ActionAdminSuspendUser    = "admin.user_suspend"
	ActionAdminReactivateUser = "admin.user_reactivate"
	ActionAdminForceLogout    = "admin.user_force_logout"
	ActionAdminDeleteUser     = "admin.user_delete"
	ActionAdminRevokeDevice   = "admin.user_revoke_device"
	ActionAdminResendVerify   = "admin.user_resend_verification"
	ActionAdminSendReset      = "admin.user_send_password_reset"
)

type Recorder struct{ db *store.DB }

func NewRecorder(db *store.DB) *Recorder { return &Recorder{db: db} }

func (r *Recorder) Record(ctx context.Context, q store.Q, actorType string, actorID *uuid.UUID,
	action string, targetUserID *uuid.UUID, reason, ipHash string,
	before, after any) error {
	var beforeJSON, afterJSON any
	if before != nil {
		b, err := json.Marshal(before)
		if err == nil {
			beforeJSON = b
		}
	}
	if after != nil {
		b, err := json.Marshal(after)
		if err == nil {
			afterJSON = b
		}
	}
	var aid, tid any
	if actorID != nil {
		aid = *actorID
	}
	if targetUserID != nil {
		tid = *targetUserID
	}
	_, err := q.Exec(ctx, `
		INSERT INTO audit_events (actor_type, actor_id, action, target_user_id,
			reason, ip_hash, before_state, after_state, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		actorType, aid, action, tid, reason, ipHash, beforeJSON, afterJSON, time.Now())
	return err
}

// AuditEvent is the read model for the admin log pages.
type AuditEvent struct {
	ID           int64
	ActorType    string
	ActorID      *uuid.UUID
	Action       string
	TargetUserID *uuid.UUID
	Reason       string
	IPHash       string
	BeforeState  json.RawMessage
	AfterState   json.RawMessage
	CreatedAt    time.Time
}

func ListEvents(ctx context.Context, q store.Q, targetUserID *uuid.UUID, limit, offset int) ([]*AuditEvent, error) {
	where := "TRUE"
	var args []any
	if targetUserID != nil {
		args = append(args, *targetUserID)
		where = fmt.Sprintf("target_user_id = $%d", len(args))
	}
	args = append(args, limit)
	limitArg := fmt.Sprintf("$%d", len(args))
	args = append(args, offset)
	offsetArg := fmt.Sprintf("$%d", len(args))
	rows, err := q.Query(ctx, `
		SELECT id, actor_type, actor_id, action, target_user_id, reason, ip_hash,
		       before_state, after_state, created_at
		FROM audit_events WHERE `+where+`
		ORDER BY id DESC LIMIT `+limitArg+`
		OFFSET `+offsetArg, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEvent
	for rows.Next() {
		e := &AuditEvent{}
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorID, &e.Action, &e.TargetUserID,
			&e.Reason, &e.IPHash, &e.BeforeState, &e.AfterState, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEventsForTarget returns the newest events concerning one user (the
// admin user-detail page's security timeline).
func ListEventsForTarget(ctx context.Context, q store.Q, targetUserID uuid.UUID, limit int) ([]*AuditEvent, error) {
	return ListEvents(ctx, q, &targetUserID, limit, 0)
}
