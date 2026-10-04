-- Migration: System settings table for admin configuration

CREATE TABLE IF NOT EXISTS system_settings (
    key VARCHAR(100) PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- OAuth settings (encrypted values for secrets)
-- Keys: google_oauth_client_id, google_oauth_client_secret, google_oauth_redirect_uri

-- Admin flag on users (read by auth since 2026-03). Production got the column
-- by hand; recorded here so an empty database matches it.
ALTER TABLE users ADD COLUMN IF NOT EXISTS is_admin BOOLEAN DEFAULT false;
