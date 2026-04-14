-- Attachments for outbox messages (compose with files)
CREATE TABLE IF NOT EXISTS outbox_attachments (
    id SERIAL PRIMARY KEY,
    outbox_message_id INTEGER NOT NULL REFERENCES outbox_messages(id) ON DELETE CASCADE,
    filename VARCHAR(512) NOT NULL,
    content_type VARCHAR(255) NOT NULL DEFAULT 'application/octet-stream',
    size INTEGER NOT NULL DEFAULT 0,
    data BYTEA,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_outbox_attachments_message ON outbox_attachments(outbox_message_id);
