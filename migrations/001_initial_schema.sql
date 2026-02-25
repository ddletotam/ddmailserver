-- Initial database schema

-- Users table
CREATE TABLE IF NOT EXISTS users (
    id SERIAL PRIMARY KEY,
    username VARCHAR(255) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    email VARCHAR(255) UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- External email accounts
CREATE TABLE IF NOT EXISTS accounts (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    email VARCHAR(255) NOT NULL,
    imap_host VARCHAR(255) NOT NULL,
    imap_port INTEGER NOT NULL,
    imap_username VARCHAR(255) NOT NULL,
    imap_password TEXT NOT NULL, -- Should be encrypted
    imap_tls BOOLEAN DEFAULT true,
    smtp_host VARCHAR(255) NOT NULL,
    smtp_port INTEGER NOT NULL,
    smtp_username VARCHAR(255) NOT NULL,
    smtp_password TEXT NOT NULL, -- Should be encrypted
    smtp_tls BOOLEAN DEFAULT true,
    enabled BOOLEAN DEFAULT true,
    last_sync TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, email)
);

-- Folders
CREATE TABLE IF NOT EXISTS folders (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id INTEGER REFERENCES accounts(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    path VARCHAR(512) NOT NULL,
    type VARCHAR(50) NOT NULL, -- inbox, sent, drafts, trash, junk, archive, custom
    parent_id INTEGER REFERENCES folders(id) ON DELETE CASCADE,
    uid_next INTEGER DEFAULT 1,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, account_id, path)
);

-- Messages
CREATE TABLE IF NOT EXISTS messages (
    id SERIAL PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    folder_id INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    message_id VARCHAR(512) NOT NULL, -- RFC 5322 Message-ID
    subject TEXT,
    from_addr TEXT NOT NULL,
    to_addr TEXT,
    cc TEXT,
    bcc TEXT,
    reply_to TEXT,
    date TIMESTAMP NOT NULL,
    body TEXT,
    body_html TEXT,
    attachments INTEGER DEFAULT 0,
    size BIGINT DEFAULT 0,
    uid INTEGER NOT NULL,
    seen BOOLEAN DEFAULT false,
    flagged BOOLEAN DEFAULT false,
    answered BOOLEAN DEFAULT false,
    draft BOOLEAN DEFAULT false,
    deleted BOOLEAN DEFAULT false,
    in_reply_to VARCHAR(512),
    references TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(account_id, folder_id, uid)
);

-- Attachments
CREATE TABLE IF NOT EXISTS attachments (
    id SERIAL PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    filename VARCHAR(512) NOT NULL,
    content_type VARCHAR(255) NOT NULL,
    size BIGINT NOT NULL,
    data BYTEA, -- Could also store path to filesystem
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Sync status
CREATE TABLE IF NOT EXISTS sync_status (
    id SERIAL PRIMARY KEY,
    account_id INTEGER UNIQUE NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    last_sync TIMESTAMP,
    last_error TEXT,
    status VARCHAR(50) DEFAULT 'idle', -- idle, syncing, error
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Indexes for performance
CREATE INDEX idx_messages_user_id ON messages(user_id);
CREATE INDEX idx_messages_account_id ON messages(account_id);
CREATE INDEX idx_messages_folder_id ON messages(folder_id);
CREATE INDEX idx_messages_date ON messages(date DESC);
CREATE INDEX idx_messages_message_id ON messages(message_id);
CREATE INDEX idx_messages_seen ON messages(seen);
CREATE INDEX idx_folders_user_id ON folders(user_id);
CREATE INDEX idx_folders_account_id ON folders(account_id);
CREATE INDEX idx_accounts_user_id ON accounts(user_id);
CREATE INDEX idx_attachments_message_id ON attachments(message_id);
