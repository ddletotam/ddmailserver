-- Spam filtering system
-- 1. Mark messages as spam
-- 2. User-defined rules (whitelist/blacklist)
-- 3. Disable specific system checks per user

-- Add spam flag to messages
ALTER TABLE messages ADD COLUMN IF NOT EXISTS is_spam BOOLEAN DEFAULT false;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS spam_rule_id BIGINT;

-- User spam rules (whitelist/blacklist)
CREATE TABLE IF NOT EXISTS user_spam_rules (
    id SERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    rule_type VARCHAR(20) NOT NULL,  -- 'address', 'domain'
    rule_value TEXT NOT NULL,        -- 'spammer@evil.com' or 'evil.com'
    action VARCHAR(10) NOT NULL,     -- 'spam' (blacklist), 'allow' (whitelist)
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(user_id, rule_type, rule_value)
);

-- Disabled system spam checks per user
CREATE TABLE IF NOT EXISTS user_disabled_spam_checks (
    id SERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    check_name VARCHAR(100) NOT NULL,  -- 'spf_fail', 'rbl', 'spam_word:viagra', 'url_shortener'
    disabled_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(user_id, check_name)
);

-- Index for fast spam queries
CREATE INDEX IF NOT EXISTS idx_messages_spam ON messages(user_id, is_spam) WHERE is_spam = true;
CREATE INDEX IF NOT EXISTS idx_messages_not_spam ON messages(folder_id, is_spam) WHERE is_spam = false;

-- Index for spam rules lookup
CREATE INDEX IF NOT EXISTS idx_user_spam_rules_user ON user_spam_rules(user_id);
CREATE INDEX IF NOT EXISTS idx_user_spam_rules_lookup ON user_spam_rules(user_id, rule_type, action);

-- Index for disabled checks lookup
CREATE INDEX IF NOT EXISTS idx_user_disabled_checks ON user_disabled_spam_checks(user_id);
