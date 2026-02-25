-- Outbox queue for messages to be sent

CREATE TABLE IF NOT EXISTS outbox_messages (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    from_addr TEXT NOT NULL,
    to_addr TEXT NOT NULL,
    cc TEXT,
    bcc TEXT,
    subject TEXT,
    body TEXT,
    body_html TEXT,
    raw_email BYTEA, -- RFC 5322 formatted message
    status VARCHAR(50) DEFAULT 'pending', -- pending, sending, sent, failed
    retries INTEGER DEFAULT 0,
    last_error TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    sent_at TIMESTAMP
);

-- Indexes
CREATE INDEX idx_outbox_user_id ON outbox_messages(user_id);
CREATE INDEX idx_outbox_account_id ON outbox_messages(account_id);
CREATE INDEX idx_outbox_status ON outbox_messages(status);
CREATE INDEX idx_outbox_created_at ON outbox_messages(created_at DESC);
