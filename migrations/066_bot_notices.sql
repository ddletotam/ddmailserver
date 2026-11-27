-- Messages the server has for people through the Telegram bot
-- (docs/lunlun-bot-plan.md): sending stopped, subscription ending, ... The
-- server can't reach Telegram itself, so the bot polls them and marks them sent.
CREATE TABLE IF NOT EXISTS bot_notices (
    id          BIGSERIAL PRIMARY KEY,
    tg_user_id  BIGINT NOT NULL,
    kind        VARCHAR(32) NOT NULL,
    text        TEXT NOT NULL,
    created_at  BIGINT NOT NULL,
    sent_at     BIGINT
);
CREATE INDEX IF NOT EXISTS bot_notices_pending ON bot_notices (id) WHERE sent_at IS NULL;
