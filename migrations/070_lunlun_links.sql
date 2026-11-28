-- The lunlun.io account of a service user (internal/maillink): made when the
-- user signs up through the bot; a user without a row gets one from the
-- hourly reconcile.
CREATE TABLE IF NOT EXISTS lunlun_links (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    lunlun_id  BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);
