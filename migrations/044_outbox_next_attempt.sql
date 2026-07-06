-- Retry backoff for the outbox: a failed attempt schedules the next one
-- instead of hammering the remote MX three times in a row. 0 = due now
-- (ms since epoch, same clock as timeutil.Now()).
ALTER TABLE outbox_messages ADD COLUMN IF NOT EXISTS next_attempt_at BIGINT NOT NULL DEFAULT 0;

-- The scheduler polls "pending AND due" every cycle.
CREATE INDEX IF NOT EXISTS idx_outbox_pending_next_attempt
    ON outbox_messages (status, next_attempt_at);
