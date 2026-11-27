-- Sign-up through the Telegram bot (docs/lunlun-bot-plan.md): one Telegram
-- account owns at most one free account. Only the numeric Telegram user id is
-- stored — no name, no username.
--
-- user_id becomes NULL when the account is deleted, but the row stays: a
-- banned Telegram id must not come back with a new account.
CREATE TABLE IF NOT EXISTS telegram_links (
    tg_user_id        BIGINT PRIMARY KEY,
    user_id           INTEGER UNIQUE REFERENCES users(id) ON DELETE SET NULL,
    terms_accepted_at BIGINT NOT NULL,
    banned            BOOLEAN NOT NULL DEFAULT false,
    created_at        BIGINT NOT NULL
);
