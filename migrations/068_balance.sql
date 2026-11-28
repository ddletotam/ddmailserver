-- Balance instead of Telegram subscriptions (docs/lunlun-bot-plan.md, решения
-- 2026-11-28). A user tops up a balance in Stars; every paid feature is a
-- subscription renewed from the balance every 30 days. Mailboxes are paid each:
-- one subscription per mailbox (ref_id).
--
-- entitlements/payments (067) held nothing yet and are replaced.
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS entitlements;

CREATE TABLE IF NOT EXISTS subscriptions (
    id         BIGSERIAL PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    feature    VARCHAR(32) NOT NULL,
    ref_id     BIGINT,                       -- the mailbox, for per-mailbox subscriptions
    price      INTEGER NOT NULL,             -- Stars per 30 days, fixed at the last charge
    paid_until BIGINT NOT NULL,              -- ms
    status     VARCHAR(12) NOT NULL DEFAULT 'active', -- active | cancelled | suspended
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS subscriptions_user ON subscriptions (user_id, feature);
CREATE INDEX IF NOT EXISTS subscriptions_due ON subscriptions (paid_until) WHERE status = 'active';

-- Every movement of the balance. The balance is SUM(amount).
CREATE TABLE IF NOT EXISTS balance_ops (
    id                 BIGSERIAL PRIMARY KEY,
    user_id            INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind               VARCHAR(12) NOT NULL,  -- topup | charge | refund
    amount             INTEGER NOT NULL,      -- Stars, + in, - out
    subscription_id    BIGINT REFERENCES subscriptions(id) ON DELETE SET NULL,
    telegram_charge_id VARCHAR(255) UNIQUE,   -- topups only
    refunded           BOOLEAN NOT NULL DEFAULT false, -- a topup given back
    refund_of          BIGINT REFERENCES balance_ops(id), -- a refund: the topup it gives back
    note               TEXT NOT NULL DEFAULT '',
    created_at         BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS balance_ops_user ON balance_ops (user_id, id);
