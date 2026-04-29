-- Folder subscription tracking
-- By default only INBOX is subscribed; clients must explicitly subscribe to other folders
CREATE TABLE IF NOT EXISTS folder_subscriptions (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    folder_id INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, folder_id)
);

CREATE INDEX IF NOT EXISTS idx_folder_subscriptions_user ON folder_subscriptions(user_id);

-- Partial unique index for local folders (account_id IS NULL) to prevent duplicates
CREATE UNIQUE INDEX IF NOT EXISTS idx_folders_local_user_path
ON folders (user_id, path) WHERE account_id IS NULL;

-- Partial unique index for local folders by type (one folder per type per user)
CREATE UNIQUE INDEX IF NOT EXISTS idx_folders_local_user_type
ON folders (user_id, type) WHERE account_id IS NULL AND type != 'custom';

-- Seed: subscribe all existing users to their INBOX
INSERT INTO folder_subscriptions (user_id, folder_id)
SELECT user_id, id FROM folders WHERE type = 'inbox' AND account_id IS NULL
ON CONFLICT DO NOTHING;
