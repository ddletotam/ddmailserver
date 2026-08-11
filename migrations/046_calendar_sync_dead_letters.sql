-- Dead letters for the calendar reverse-sync queue.
--
-- Until now an entry that exhausted its retries was deleted outright, and the
-- request body went with it. That cost a real investigation: a PUT that iCloud
-- answered 403 to for four days was dropped on the eighth attempt, and by the
-- time anyone looked there was nothing left to compare against a request that
-- worked. Six hypotheses were tested against a body that no longer existed.
--
-- The entry is moved here instead. Nothing reads this table on its own; it
-- exists so a failure can be examined afterwards, and so that "remote state
-- wins" stops meaning "the local change is gone without a trace".
--
-- `035_calendar_sync_dlq.sql` named itself after this table and only added retry
-- columns; this is the table it promised.
CREATE TABLE IF NOT EXISTS calendar_event_sync_dead_letters (
    id SERIAL PRIMARY KEY,

    -- Mirrors the queue row. event_id carries no foreign key for the same
    -- reason the queue does not: the event is often already gone, and that is
    -- precisely the state worth keeping a record of.
    event_id BIGINT,
    calendar_id BIGINT REFERENCES calendars(id) ON DELETE CASCADE,
    source_id BIGINT REFERENCES calendar_sources(id) ON DELETE CASCADE,
    uid VARCHAR(255) NOT NULL,
    remote_id VARCHAR(255) NOT NULL DEFAULT '',

    -- The whole point of the table: the exact bytes the remote server refused.
    ical_data TEXT NOT NULL DEFAULT '',

    operation VARCHAR(20) NOT NULL,
    retry_count INT NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',

    -- created_at of the original queue entry, i.e. when the change was made.
    queued_at BIGINT NOT NULL DEFAULT 0,
    died_at BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_calendar_sync_dead_letters_source
    ON calendar_event_sync_dead_letters(source_id, died_at DESC);
