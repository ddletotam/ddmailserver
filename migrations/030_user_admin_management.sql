-- User administration: ban flag for blocking login without deletion.
-- (is_admin column already exists from earlier migration; this just adds is_banned.)

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS is_banned BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_users_is_banned ON users(is_banned) WHERE is_banned = TRUE;
