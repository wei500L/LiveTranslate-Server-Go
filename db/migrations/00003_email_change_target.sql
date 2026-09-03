-- Email change flow: the verification challenge for a login-email change
-- must remember WHICH new address it was issued for (the code alone is not
-- enough — the target address must be re-checked inside the consuming
-- transaction, and it must be auditable). verify_email challenges keep
-- target_email NULL.

-- +goose Up
ALTER TABLE email_challenges ADD COLUMN target_email VARCHAR(320);
CREATE INDEX ix_email_challenges_target
    ON email_challenges (user_id, purpose, target_email);

-- +goose Down
DROP INDEX IF EXISTS ix_email_challenges_target;
ALTER TABLE email_challenges DROP COLUMN IF EXISTS target_email;
