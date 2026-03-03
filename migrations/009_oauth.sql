-- Migration: Add OAuth2 support for Gmail and other providers

-- Auth type: 'password' for regular login, 'oauth2_google' for Google OAuth2
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS auth_type VARCHAR(20) DEFAULT 'password';

-- OAuth2 tokens (encrypted like passwords)
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS oauth_access_token TEXT;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS oauth_refresh_token TEXT;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS oauth_token_expiry TIMESTAMP;

-- Index for finding accounts that need token refresh
CREATE INDEX IF NOT EXISTS idx_accounts_oauth_expiry ON accounts(oauth_token_expiry) WHERE auth_type != 'password';
