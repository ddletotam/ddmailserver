-- Add recovery key support and make email optional

-- Make email nullable (not required for registration)
ALTER TABLE users ALTER COLUMN email DROP NOT NULL;

-- Add recovery key hash (bcrypt hash of the recovery key)
ALTER TABLE users ADD COLUMN recovery_key_hash VARCHAR(255);

-- Create index on recovery key for faster lookups
CREATE INDEX idx_users_recovery_key ON users(recovery_key_hash) WHERE recovery_key_hash IS NOT NULL;
