-- Add uid_validity column to folders table for incremental sync
ALTER TABLE folders ADD COLUMN IF NOT EXISTS uid_validity INTEGER DEFAULT 0;
