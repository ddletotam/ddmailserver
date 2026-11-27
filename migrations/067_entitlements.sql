-- Paid features of the sign-up service (docs/lunlun-bot-plan.md): no plans,
-- every feature is its own monthly subscription paid with Telegram Stars.
-- entitlements: what a user has and until when; payments: every charge, for
-- refunds (refundStarPayment needs the Telegram charge id) and history.
CREATE TABLE IF NOT EXISTS entitlements (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    feature    VARCHAR(32) NOT NULL,
    until      BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (user_id, feature)
);

CREATE TABLE IF NOT EXISTS payments (
    id                 BIGSERIAL PRIMARY KEY,
    user_id            INTEGER REFERENCES users(id) ON DELETE SET NULL,
    tg_user_id         BIGINT NOT NULL,
    feature            VARCHAR(32) NOT NULL,
    amount             INTEGER NOT NULL,
    currency           VARCHAR(8) NOT NULL DEFAULT 'XTR',
    telegram_charge_id VARCHAR(255) NOT NULL UNIQUE,
    recurring          BOOLEAN NOT NULL DEFAULT false,
    paid_until         BIGINT NOT NULL,
    created_at         BIGINT NOT NULL
);
