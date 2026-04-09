-- Track sync errors so users can see when accounts are broken
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS last_sync_error TEXT DEFAULT '';
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS consecutive_errors INTEGER DEFAULT 0;
