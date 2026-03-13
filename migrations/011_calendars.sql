-- Calendar sources (similar to accounts for mail)
CREATE TABLE calendar_sources (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    source_type VARCHAR(20) NOT NULL,  -- 'local', 'caldav', 'ics_import'

    -- For CalDAV sources
    caldav_url TEXT,
    caldav_username VARCHAR(255),
    caldav_password TEXT,  -- encrypted

    -- OAuth (for Google/Microsoft)
    auth_type VARCHAR(20) DEFAULT 'password',  -- 'password', 'oauth2_google', 'oauth2_microsoft'
    oauth_access_token TEXT,
    oauth_refresh_token TEXT,
    oauth_token_expiry TIMESTAMP,

    -- Sync settings
    sync_enabled BOOLEAN DEFAULT true,
    sync_interval INTEGER DEFAULT 300,  -- seconds
    last_sync TIMESTAMP,
    sync_token TEXT,  -- for incremental sync

    color VARCHAR(7) DEFAULT '#3788d8',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Calendars (can be multiple per source)
CREATE TABLE calendars (
    id SERIAL PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    remote_id VARCHAR(255),  -- ID on remote server
    name VARCHAR(255) NOT NULL,
    description TEXT,
    color VARCHAR(7),
    timezone VARCHAR(50) DEFAULT 'UTC',

    ctag VARCHAR(64),  -- for sync

    -- Access rights (for CalDAV)
    can_write BOOLEAN DEFAULT true,

    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(source_id, remote_id)
);

-- Events
CREATE TABLE calendar_events (
    id SERIAL PRIMARY KEY,
    calendar_id INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,

    uid VARCHAR(255) NOT NULL,  -- iCalendar UID
    remote_id VARCHAR(255),     -- ID on remote server (if any)

    ical_data TEXT NOT NULL,    -- raw iCalendar (VEVENT)

    -- Indexed fields for search
    summary VARCHAR(255),
    description TEXT,
    location VARCHAR(255),
    dtstart TIMESTAMP,
    dtend TIMESTAMP,
    all_day BOOLEAN DEFAULT false,

    -- Recurring events
    rrule TEXT,
    recurrence_id VARCHAR(255),

    -- Sync
    etag VARCHAR(64),
    local_modified BOOLEAN DEFAULT false,  -- modified locally, needs push

    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(calendar_id, uid)
);

CREATE INDEX idx_calendar_sources_user ON calendar_sources(user_id);
CREATE INDEX idx_calendars_source ON calendars(source_id);
CREATE INDEX idx_calendars_user ON calendars(user_id);
CREATE INDEX idx_events_calendar ON calendar_events(calendar_id);
CREATE INDEX idx_events_dtstart ON calendar_events(dtstart);
CREATE INDEX idx_events_dtend ON calendar_events(dtend);
