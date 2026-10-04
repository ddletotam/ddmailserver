-- Bidirectional CalDAV event sync queue
-- Queues local event changes for reverse sync to external CalDAV servers
CREATE TABLE IF NOT EXISTS calendar_event_sync_queue (
    id SERIAL PRIMARY KEY,
    event_id BIGINT NOT NULL REFERENCES calendar_events(id) ON DELETE CASCADE,
    calendar_id BIGINT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    source_id BIGINT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
    uid VARCHAR(255) NOT NULL,
    remote_id VARCHAR(512),
    ical_data TEXT,
    operation VARCHAR(10) NOT NULL, -- 'create', 'update', 'delete'
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(event_id)
);

CREATE INDEX IF NOT EXISTS idx_calendar_event_sync_queue_source
    ON calendar_event_sync_queue(source_id);
