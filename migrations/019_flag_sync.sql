-- Migration 019: Bidirectional flag sync support
-- Adds remote UID tracking and flag sync queue for reverse proxy mode

-- Add remote UID tracking to messages
ALTER TABLE messages ADD COLUMN IF NOT EXISTS remote_uid INTEGER;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS remote_folder VARCHAR(255) DEFAULT 'INBOX';

-- Index for efficient lookups by remote UID
CREATE INDEX IF NOT EXISTS idx_messages_remote ON messages(account_id, remote_folder, remote_uid);

-- Queue for pending flag changes to push to remote IMAP servers
CREATE TABLE IF NOT EXISTS flag_sync_queue (
    id SERIAL PRIMARY KEY,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    remote_folder VARCHAR(255) NOT NULL,
    remote_uid INTEGER NOT NULL,
    seen BOOLEAN NOT NULL,
    flagged BOOLEAN NOT NULL,
    answered BOOLEAN NOT NULL,
    deleted BOOLEAN NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    -- Only one pending entry per message (latest wins)
    UNIQUE(message_id)
);

-- Index for fetching pending entries by account
CREATE INDEX IF NOT EXISTS idx_flag_sync_queue_account ON flag_sync_queue(account_id);
