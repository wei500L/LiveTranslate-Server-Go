-- LiveTranslate Server — initial PostgreSQL schema (Go baseline).
--
-- This is a CLEAN baseline for the Go server (no production PostgreSQL data
-- existed when the Python service ran on SQLite locally). The sync tables
-- (users/devices/refresh_tokens/classroom_sessions/transcript_entries/
-- bookmarks/favorite_sessions/sync_changes/processed_operations) match the
-- Python Alembic 0001 migration column-for-column so a Postgres database
-- migrated by Alembic can be adopted by the Go server as-is.
-- Identity tables (auth_identities, password_credentials, email_challenges,
-- password_reset_tokens, login_events, invitations, admin_accounts,
-- admin_sessions, audit_events) are new in the Go baseline.

-- +goose Up

CREATE TABLE users (
    id                   UUID PRIMARY KEY,
    email                VARCHAR(320),
    -- Uniqueness lives in the PARTIAL index below: a plain UNIQUE would
    -- keep soft-deleted accounts owning their email forever, blocking
    -- re-registration after account deletion.
    normalized_email     VARCHAR(320),
    display_name         VARCHAR(128) NOT NULL DEFAULT '',
    -- active | pending | suspended | pending_deletion | deleted
    status               VARCHAR(32)  NOT NULL DEFAULT 'active',
    -- user | admin (admin rights are checked server-side per request,
    -- never trusted from a long-lived JWT alone)
    role                 VARCHAR(16)  NOT NULL DEFAULT 'user',
    -- Legacy Apple/dev columns kept for compatibility with the Python era
    -- schema; new accounts leave them NULL.
    apple_subject        VARCHAR(128) UNIQUE,
    dev_name             VARCHAR(64),
    email_verified_at    TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL,
    last_login_at        TIMESTAMPTZ,
    deletion_requested_at TIMESTAMPTZ,
    deleted_at           TIMESTAMPTZ
);
-- One live account (pending or verified) per email; deleted rows are
-- excluded so their emails become reusable.
CREATE UNIQUE INDEX users_email_active_uidx
    ON users (normalized_email)
    WHERE deleted_at IS NULL AND status <> 'deleted';
CREATE INDEX ix_users_apple_subject ON users (apple_subject) WHERE apple_subject IS NOT NULL;
CREATE INDEX ix_users_status ON users (status);

CREATE TABLE auth_identities (
    id                UUID PRIMARY KEY,
    user_id           UUID NOT NULL REFERENCES users(id),
    -- apple | password (password rows exist only when the email/password
    -- credential was created through the bind flow; login itself hashes
    -- against password_credentials)
    provider          VARCHAR(32) NOT NULL,
    provider_subject  VARCHAR(190) NOT NULL,
    provider_email    VARCHAR(320),
    created_at        TIMESTAMPTZ NOT NULL,
    last_used_at      TIMESTAMPTZ,
    UNIQUE (provider, provider_subject)
);
CREATE INDEX ix_auth_identities_user ON auth_identities (user_id);

CREATE TABLE password_credentials (
    user_id             UUID PRIMARY KEY REFERENCES users(id),
    -- Argon2id PHC-format string (algorithm+params self-describing).
    password_hash       VARCHAR(256) NOT NULL,
    password_changed_at TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL
);

-- Email verification codes. Only the SHA-256 hash of the code is stored;
-- codes are single-use, expire, and a new code invalidates older ones.
CREATE TABLE email_challenges (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users(id),
    purpose         VARCHAR(32) NOT NULL, -- verify_email
    token_hash      VARCHAR(64) NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    attempt_count   INT NOT NULL DEFAULT 0,
    consumed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_email_challenges_user ON email_challenges (user_id, purpose);
CREATE INDEX ix_email_challenges_expires ON email_challenges (expires_at);

-- Password reset tokens. Hash-only, single-use, short-lived.
CREATE TABLE password_reset_tokens (
    id          UUID PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id),
    token_hash  VARCHAR(64) NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_password_resets_user ON password_reset_tokens (user_id);
CREATE INDEX ix_password_resets_expires ON password_reset_tokens (expires_at);

-- Login attempt audit (IP hash + email hash so the table itself carries no
-- readable PII). result: success | invalid_password | unknown_email |
-- suspended | rate_limited | unverified
CREATE TABLE login_events (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             UUID,
    normalized_email_hash VARCHAR(64) NOT NULL,
    device_id           VARCHAR(128),
    ip_hash             VARCHAR(64) NOT NULL,
    result              VARCHAR(32) NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_login_events_email_time ON login_events (normalized_email_hash, created_at);
CREATE INDEX ix_login_events_ip_time ON login_events (ip_hash, created_at);
CREATE INDEX ix_login_events_user_time ON login_events (user_id, created_at);

-- Admin accounts are SEPARATE from users (no impersonation path, no shared
-- login surface).
CREATE TABLE admin_accounts (
    id                 UUID PRIMARY KEY,
    username           VARCHAR(64) NOT NULL UNIQUE,
    password_hash      VARCHAR(256) NOT NULL, -- Argon2id PHC
    -- Optional TOTP secret (base32). NULL = TOTP disabled.
    totp_secret        VARCHAR(128),
    failed_attempts    INT NOT NULL DEFAULT 0,
    locked_until       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL,
    last_login_at      TIMESTAMPTZ
);

CREATE TABLE admin_sessions (
    id           UUID PRIMARY KEY,
    admin_id     UUID NOT NULL REFERENCES admin_accounts(id),
    token_hash   VARCHAR(64) NOT NULL UNIQUE,
    csrf_token   VARCHAR(64) NOT NULL,
    ip_hash      VARCHAR(64) NOT NULL,
    user_agent   VARCHAR(256) NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_admin_sessions_admin ON admin_sessions (admin_id);

-- Optional registration invitations. created_by references the ADMIN who
-- minted the code (admins are not users — there is no impersonation path).
CREATE TABLE invitations (
    code          VARCHAR(32) PRIMARY KEY,
    note          VARCHAR(256) NOT NULL DEFAULT '',
    created_by    UUID REFERENCES admin_accounts(id),
    max_uses      INT NOT NULL DEFAULT 1,
    used_count    INT NOT NULL DEFAULT 0,
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_invitations_expires ON invitations (expires_at);

-- Append-only audit log for admin operations and security events.
-- before/after snapshots are small status summaries, never transcript text.
CREATE TABLE audit_events (
    id            BIGSERIAL PRIMARY KEY,
    actor_type    VARCHAR(16) NOT NULL, -- admin | user | system
    actor_id      UUID,
    action        VARCHAR(64) NOT NULL,
    target_user_id UUID,
    reason        VARCHAR(256) NOT NULL DEFAULT '',
    ip_hash       VARCHAR(64) NOT NULL DEFAULT '',
    before_state  JSONB,
    after_state   JSONB,
    created_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_audit_events_target ON audit_events (target_user_id, created_at);
CREATE INDEX ix_audit_events_action ON audit_events (action, created_at);

-- === Sync core (column-compatible with the Python Alembic 0001 schema) ===

CREATE TABLE devices (
    id                UUID PRIMARY KEY,
    user_id           UUID NOT NULL,
    client_device_id  VARCHAR(128) NOT NULL,
    display_name      VARCHAR(128) NOT NULL DEFAULT '',
    app_version       VARCHAR(32) NOT NULL DEFAULT '',
    last_seen_at      TIMESTAMPTZ NOT NULL,
    revoked_at        TIMESTAMPTZ,
    UNIQUE (user_id, client_device_id)
);
CREATE INDEX ix_devices_user_id ON devices (user_id);

CREATE TABLE refresh_tokens (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL,
    device_id    UUID NOT NULL,
    token_hash   VARCHAR(64) NOT NULL UNIQUE,
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    replaced_by  UUID,
    created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_refresh_tokens_user_id ON refresh_tokens (user_id);
CREATE INDEX ix_refresh_tokens_device_id ON refresh_tokens (device_id);

CREATE TABLE classroom_sessions (
    id                    UUID PRIMARY KEY,
    user_id               UUID NOT NULL,
    title                 VARCHAR(256) NOT NULL DEFAULT '',
    started_at            TIMESTAMPTZ NOT NULL,
    ended_at              TIMESTAMPTZ,
    duration              DOUBLE PRECISION NOT NULL DEFAULT 0,
    source_language       VARCHAR(16) NOT NULL DEFAULT 'ru',
    target_language       VARCHAR(16) NOT NULL DEFAULT 'zh-CN',
    session_status        VARCHAR(16) NOT NULL DEFAULT 'active',
    abnormal_termination  BOOLEAN NOT NULL DEFAULT FALSE,
    server_version        INT NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    deleted_at            TIMESTAMPTZ
);
CREATE INDEX ix_sessions_user_updated ON classroom_sessions (user_id, updated_at);
CREATE INDEX ix_sessions_user_id ON classroom_sessions (user_id);

CREATE TABLE transcript_entries (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL,
    session_id          UUID NOT NULL,
    sequence_id         INT NOT NULL,
    start_offset        DOUBLE PRECISION NOT NULL DEFAULT 0,
    end_offset          DOUBLE PRECISION NOT NULL DEFAULT 0,
    russian_text        TEXT NOT NULL DEFAULT '',
    chinese_text        TEXT,
    translation_status  VARCHAR(16) NOT NULL DEFAULT 'pending',
    server_version      INT NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL,
    deleted_at          TIMESTAMPTZ,
    UNIQUE (user_id, session_id, sequence_id)
);
CREATE INDEX ix_entries_user_session ON transcript_entries (user_id, session_id);
CREATE INDEX ix_entries_user_id ON transcript_entries (user_id);

CREATE TABLE bookmarks (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL,
    session_id      UUID NOT NULL,
    entry_id        UUID NOT NULL,
    is_bookmarked   BOOLEAN NOT NULL DEFAULT TRUE,
    server_version  INT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL,
    deleted_at      TIMESTAMPTZ,
    UNIQUE (user_id, entry_id)
);
CREATE INDEX ix_bookmarks_user_id ON bookmarks (user_id);
CREATE INDEX ix_bookmarks_session ON bookmarks (user_id, session_id);

CREATE TABLE favorite_sessions (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL,
    session_id      UUID NOT NULL,
    is_favorite     BOOLEAN NOT NULL DEFAULT TRUE,
    server_version  INT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL,
    deleted_at      TIMESTAMPTZ,
    UNIQUE (user_id, session_id)
);
CREATE INDEX ix_favorites_user_id ON favorite_sessions (user_id);

-- Global change log driving incremental pull. change_sequence is THE cursor.
CREATE TABLE sync_changes (
    change_sequence  BIGSERIAL PRIMARY KEY,
    user_id          UUID NOT NULL,
    entity_type      VARCHAR(16) NOT NULL,
    entity_id        UUID NOT NULL,
    operation        VARCHAR(16) NOT NULL, -- upsert | delete
    server_version   INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX ix_changes_user_seq ON sync_changes (user_id, change_sequence);

-- Idempotency ledger: (user_id, operation_id) -> stored push result.
CREATE TABLE processed_operations (
    id            BIGSERIAL PRIMARY KEY,
    user_id       UUID NOT NULL,
    operation_id  UUID NOT NULL,
    entity_type   VARCHAR(16) NOT NULL,
    entity_id     UUID NOT NULL,
    result        JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, operation_id)
);
CREATE INDEX ix_processed_ops_user ON processed_operations (user_id);

-- +goose Down
DROP TABLE IF EXISTS processed_operations;
DROP TABLE IF EXISTS sync_changes;
DROP TABLE IF EXISTS favorite_sessions;
DROP TABLE IF EXISTS bookmarks;
DROP TABLE IF EXISTS transcript_entries;
DROP TABLE IF EXISTS classroom_sessions;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS admin_sessions;
DROP TABLE IF EXISTS admin_accounts;
DROP TABLE IF EXISTS login_events;
DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS email_challenges;
DROP TABLE IF EXISTS password_credentials;
DROP TABLE IF EXISTS auth_identities;
DROP TABLE IF EXISTS users;
