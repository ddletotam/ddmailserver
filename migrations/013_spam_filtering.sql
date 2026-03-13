-- Add spam filtering fields to messages table
ALTER TABLE messages ADD COLUMN spam_score REAL DEFAULT 0;
ALTER TABLE messages ADD COLUMN spam_status VARCHAR(20) DEFAULT 'clean';
ALTER TABLE messages ADD COLUMN spam_reasons TEXT;

-- Index for filtering by spam status
CREATE INDEX idx_messages_spam_status ON messages(spam_status);

-- Table for tracking email authentication results (SPF/DKIM/DMARC)
CREATE TABLE IF NOT EXISTS message_auth (
    id SERIAL PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    sender_ip VARCHAR(45),
    spf_result VARCHAR(20),
    dkim_result VARCHAR(20),
    dmarc_result VARCHAR(20),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_message_auth_message_id ON message_auth(message_id);

-- Table for sender reputation tracking
CREATE TABLE IF NOT EXISTS sender_reputation (
    id SERIAL PRIMARY KEY,
    email VARCHAR(255),
    domain VARCHAR(255),
    ip VARCHAR(45),
    spam_count INTEGER DEFAULT 0,
    ham_count INTEGER DEFAULT 0,
    last_seen TIMESTAMP,
    UNIQUE(email, ip)
);

CREATE INDEX IF NOT EXISTS idx_sender_reputation_domain ON sender_reputation(domain);
CREATE INDEX IF NOT EXISTS idx_sender_reputation_email ON sender_reputation(email);

-- Table for user spam feedback (for future learning)
CREATE TABLE IF NOT EXISTS spam_feedback (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    action VARCHAR(20) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_spam_feedback_user_id ON spam_feedback(user_id);
