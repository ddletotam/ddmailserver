-- Registration as a server feature (stage 5/6 of
-- docs/server-admin-split-plan.md).
--
-- approved = false: signed up while registration was "moderated" and waits
-- for an administrator; such a user cannot sign in anywhere until approved
-- (rejecting deletes the account). Everyone who exists today is approved.
ALTER TABLE users ADD COLUMN IF NOT EXISTS approved BOOLEAN NOT NULL DEFAULT true;

-- Invitations: an administrator hands out a one-time link; whoever opens it
-- picks a login and password and gets an approved account, even when
-- registration is closed. Only the SHA-256 of the token is stored.
CREATE TABLE IF NOT EXISTS user_invites (
    id          BIGSERIAL PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    note        TEXT NOT NULL DEFAULT '',
    created_by  BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at  BIGINT NOT NULL,
    expires_at  BIGINT NOT NULL,
    used_at     BIGINT,
    used_by     BIGINT REFERENCES users(id) ON DELETE SET NULL
);
