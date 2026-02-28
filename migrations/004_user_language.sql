-- Add language preference field to users table
ALTER TABLE users ADD COLUMN IF NOT EXISTS language VARCHAR(10) DEFAULT 'en';

-- Create index for language lookups
CREATE INDEX IF NOT EXISTS idx_users_language ON users(language);
