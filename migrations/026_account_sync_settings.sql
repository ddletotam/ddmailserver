-- Per-account sync mode and polling interval
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS sync_mode VARCHAR(10) NOT NULL DEFAULT 'idle';
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS poll_interval INTEGER NOT NULL DEFAULT 300;

-- Per-account log
CREATE TABLE IF NOT EXISTS account_logs (
    id SERIAL PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    level VARCHAR(10) NOT NULL DEFAULT 'info',  -- info, error
    message TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_account_logs_account_id ON account_logs(account_id);
CREATE INDEX IF NOT EXISTS idx_account_logs_created_at ON account_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_account_logs_level ON account_logs(account_id, level);
