-- Attachments table for storing email attachments and inline images
CREATE TABLE IF NOT EXISTS attachments (
    id SERIAL PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,

    -- Content-ID for inline images (without angle brackets)
    content_id VARCHAR(255),

    -- File info
    filename VARCHAR(255),
    content_type VARCHAR(100) NOT NULL DEFAULT 'application/octet-stream',
    size INTEGER NOT NULL DEFAULT 0,

    -- Is this an inline attachment (embedded in HTML)?
    is_inline BOOLEAN DEFAULT false,

    -- Binary content stored as bytea
    content BYTEA,

    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Index for fast lookup by message_id
CREATE INDEX IF NOT EXISTS idx_attachments_message_id ON attachments(message_id);

-- Index for CID lookups (for inline images)
CREATE INDEX IF NOT EXISTS idx_attachments_content_id ON attachments(content_id) WHERE content_id IS NOT NULL;
