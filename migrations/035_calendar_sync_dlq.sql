-- Retry/backoff metadata on the reverse-sync queue.
--   retry_count       — number of failed attempts so far.
--   last_error        — most recent server-side error (truncated).
--   last_attempt_at   — ms of the last attempt (success or fail).
--   next_attempt_at   — ms before which the worker MUST NOT retry. Schedule
--                       follows a 1m → 5m → 30m → 3h → 12h → 24h backoff and
--                       then sticks at 24h forever (so a re-auth eventually
--                       drains the queue without manual reset).
ALTER TABLE calendar_event_sync_queue
    ADD COLUMN IF NOT EXISTS retry_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_attempt_at BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_attempt_at BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_calendar_event_sync_queue_ready
    ON calendar_event_sync_queue(source_id, next_attempt_at);

-- Per-source bookkeeping for the "synthetic system email" reminder. We send
-- at most one warning email per source per day, once `consecutive_failures`
-- crosses a threshold (3 in code today). When the queue for a source drains,
-- the worker resets `consecutive_failures` to 0 and the reminders stop.
CREATE TABLE IF NOT EXISTS calendar_source_warnings (
    source_id BIGINT PRIMARY KEY REFERENCES calendar_sources(id) ON DELETE CASCADE,
    last_warning_sent_at BIGINT NOT NULL DEFAULT 0,
    consecutive_failures INT NOT NULL DEFAULT 0
);
