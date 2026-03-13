-- Soft delete support for messages (vault)

ALTER TABLE messages ADD COLUMN soft_deleted BOOLEAN DEFAULT false;
ALTER TABLE messages ADD COLUMN soft_deleted_at TIMESTAMP;
ALTER TABLE messages ADD COLUMN original_folder_id BIGINT;

-- Link to calendar event (for fake emails representing calendar events)
ALTER TABLE messages ADD COLUMN calendar_event_id BIGINT REFERENCES calendar_events(id) ON DELETE CASCADE;

-- Indexes for efficient filtering
CREATE INDEX idx_messages_soft_deleted ON messages(soft_deleted);
CREATE INDEX idx_messages_calendar_event ON messages(calendar_event_id);
CREATE INDEX idx_messages_folder_not_deleted ON messages(folder_id) WHERE soft_deleted = false;
