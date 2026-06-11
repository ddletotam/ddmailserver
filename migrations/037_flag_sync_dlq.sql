-- Retry/backoff metadata on the flag reverse-sync queue — same scheme as
-- calendar_event_sync_queue (035_calendar_sync_dlq.sql):
--   retry_count       — number of failed attempts so far.
--   last_error        — most recent error (truncated).
--   last_attempt_at   — ms of the last attempt.
--   next_attempt_at   — ms before which the worker MUST NOT retry. Backoff
--                       1m → 5m → 30m → 3h → 12h → 24h, then sticks at 24h
--                       so a re-auth eventually drains the queue.
ALTER TABLE flag_sync_queue
    ADD COLUMN IF NOT EXISTS retry_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_attempt_at BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_attempt_at BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_flag_sync_queue_ready
    ON flag_sync_queue(account_id, next_attempt_at);
