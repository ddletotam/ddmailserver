-- Mailing lists (docs/lunlun-bot-plan.md, «Рассылки», решения 2026-11-28).
-- The owner writes to the list address from their own mailbox; the server
-- sends a personal copy to every confirmed subscriber. People subscribe only
-- themselves, through the bot, and confirm the address by a link — no imports.
-- The owner pays by the subscriber ceiling (tier), one subscription per list.

CREATE TABLE IF NOT EXISTS mailing_lists (
    id            BIGSERIAL PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    domain_id     INTEGER NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    local_part    VARCHAR(64) NOT NULL,
    title         VARCHAR(100) NOT NULL,
    code          VARCHAR(32) NOT NULL UNIQUE,   -- in the bot's subscribe link
    tier          INTEGER NOT NULL,              -- most subscribers it may have
    enabled       BOOLEAN NOT NULL DEFAULT true, -- off while unpaid
    stopped_until BIGINT NOT NULL DEFAULT 0,     -- autostop after bounces, ms
    created_at    BIGINT NOT NULL,
    UNIQUE (domain_id, local_part)
);
CREATE INDEX IF NOT EXISTS mailing_lists_user ON mailing_lists (user_id);

CREATE TABLE IF NOT EXISTS list_subscribers (
    id           BIGSERIAL PRIMARY KEY,
    list_id      BIGINT NOT NULL REFERENCES mailing_lists(id) ON DELETE CASCADE,
    email        VARCHAR(255) NOT NULL,
    tg_user_id   BIGINT NOT NULL,
    status       VARCHAR(12) NOT NULL,          -- pending | active | unsubscribed | bounced
    token        VARCHAR(64) NOT NULL UNIQUE,   -- confirm and unsubscribe links
    created_at   BIGINT NOT NULL,
    confirmed_at BIGINT NOT NULL DEFAULT 0,
    changed_at   BIGINT NOT NULL,
    UNIQUE (list_id, email)
);
CREATE INDEX IF NOT EXISTS list_subscribers_tg ON list_subscribers (tg_user_id);

-- One message the owner sent to the list.
CREATE TABLE IF NOT EXISTS list_issues (
    id         BIGSERIAL PRIMARY KEY,
    list_id    BIGINT NOT NULL REFERENCES mailing_lists(id) ON DELETE CASCADE,
    outbox_id  BIGINT NOT NULL,                 -- the owner's message: posted once
    subject    TEXT NOT NULL DEFAULT '',
    raw        BYTEA NOT NULL,
    created_at BIGINT NOT NULL,
    UNIQUE (list_id, outbox_id)
);

-- One letter to one subscriber: an issue, or the confirmation request.
CREATE TABLE IF NOT EXISTS list_deliveries (
    id            BIGSERIAL PRIMARY KEY,
    list_id       BIGINT NOT NULL REFERENCES mailing_lists(id) ON DELETE CASCADE,
    issue_id      BIGINT REFERENCES list_issues(id) ON DELETE CASCADE, -- NULL: confirmation
    subscriber_id BIGINT NOT NULL REFERENCES list_subscribers(id) ON DELETE CASCADE,
    status        VARCHAR(12) NOT NULL DEFAULT 'pending', -- pending | sent | failed
    attempts      INTEGER NOT NULL DEFAULT 0,
    next_at       BIGINT NOT NULL,
    permanent     BOOLEAN NOT NULL DEFAULT false, -- failed: the address does not exist
    last_error    TEXT NOT NULL DEFAULT '',
    created_at    BIGINT NOT NULL,
    done_at       BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS list_deliveries_due ON list_deliveries (next_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS list_deliveries_issue ON list_deliveries (issue_id);
