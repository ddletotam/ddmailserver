-- Exchange ActiveSync (EAS) tables

-- EAS Device tracking
CREATE TABLE IF NOT EXISTS eas_devices (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id VARCHAR(255) NOT NULL,
    device_type VARCHAR(100),
    friendly_name VARCHAR(255),
    user_agent TEXT,
    model VARCHAR(100),
    os VARCHAR(100),
    first_sync TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_sync TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    policy_key VARCHAR(64),
    policy_status INTEGER DEFAULT 0,
    wipe_requested BOOLEAN DEFAULT false,
    wipe_confirmed BOOLEAN DEFAULT false,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, device_id)
);

-- Folder sync state per device
CREATE TABLE IF NOT EXISTS eas_folder_sync (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id VARCHAR(255) NOT NULL,
    sync_key VARCHAR(64) NOT NULL,
    last_sync TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, device_id)
);

-- Item sync state per folder per device
CREATE TABLE IF NOT EXISTS eas_sync_state (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id VARCHAR(255) NOT NULL,
    folder_id VARCHAR(64) NOT NULL,  -- EAS folder ID (calendar_1, mail_5, etc.)
    sync_key VARCHAR(64) NOT NULL,
    last_sync TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_item_id BIGINT DEFAULT 0,   -- High watermark for change detection
    filter_type INTEGER DEFAULT 0,    -- Date filter type
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, device_id, folder_id)
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_eas_devices_user ON eas_devices(user_id);
CREATE INDEX IF NOT EXISTS idx_eas_devices_device ON eas_devices(device_id);
CREATE INDEX IF NOT EXISTS idx_eas_folder_sync_user_device ON eas_folder_sync(user_id, device_id);
CREATE INDEX IF NOT EXISTS idx_eas_sync_state_user_device ON eas_sync_state(user_id, device_id);
CREATE INDEX IF NOT EXISTS idx_eas_sync_state_folder ON eas_sync_state(folder_id);
