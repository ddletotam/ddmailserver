-- Address Book tables for CardDAV and LDAP support

-- Contact sources (similar to calendar_sources)
CREATE TABLE IF NOT EXISTS contact_sources (
    id SERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    source_type VARCHAR(20) NOT NULL DEFAULT 'local', -- 'local', 'carddav', 'google', 'microsoft'

    -- CardDAV connection settings
    carddav_url TEXT,
    carddav_username VARCHAR(255),
    carddav_password TEXT, -- encrypted

    -- Authentication type
    auth_type VARCHAR(20) DEFAULT 'password', -- 'password', 'oauth2_google', 'oauth2_microsoft'

    -- OAuth tokens (for Google/Microsoft)
    oauth_access_token TEXT,
    oauth_refresh_token TEXT,
    oauth_token_expiry TIMESTAMPTZ,

    -- Sync settings
    sync_enabled BOOLEAN DEFAULT true,
    sync_interval INTEGER DEFAULT 3600, -- seconds
    last_sync TIMESTAMPTZ,
    last_error TEXT,

    -- Metadata
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_contact_sources_user_id ON contact_sources(user_id);
CREATE INDEX IF NOT EXISTS idx_contact_sources_sync_enabled ON contact_sources(sync_enabled) WHERE sync_enabled = true;

-- Address books (containers, similar to calendars)
CREATE TABLE IF NOT EXISTS address_books (
    id SERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_id BIGINT REFERENCES contact_sources(id) ON DELETE CASCADE,
    remote_id VARCHAR(512), -- ID on external server

    name VARCHAR(255) NOT NULL,
    description TEXT,
    ctag VARCHAR(255), -- Collection sync token

    can_write BOOLEAN DEFAULT true,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_address_books_user_id ON address_books(user_id);
CREATE INDEX IF NOT EXISTS idx_address_books_source_id ON address_books(source_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_address_books_remote_id ON address_books(source_id, remote_id) WHERE remote_id IS NOT NULL;

-- Contacts (similar to calendar_events)
CREATE TABLE IF NOT EXISTS contacts (
    id SERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    address_book_id BIGINT NOT NULL REFERENCES address_books(id) ON DELETE CASCADE,

    -- Identifiers
    uid VARCHAR(512) NOT NULL, -- vCard UID
    remote_id VARCHAR(512), -- ID on external server (path or href)

    -- Raw vCard data
    vcard_data TEXT NOT NULL,

    -- Parsed fields for search and display
    full_name VARCHAR(255),
    given_name VARCHAR(255),
    family_name VARCHAR(255),
    nickname VARCHAR(255),

    -- Multiple emails (common case)
    email VARCHAR(255),
    email2 VARCHAR(255),
    email3 VARCHAR(255),

    -- Multiple phones
    phone VARCHAR(50),
    phone2 VARCHAR(50),
    phone3 VARCHAR(50),

    -- Organization info
    organization VARCHAR(255),
    title VARCHAR(255),
    department VARCHAR(255),

    -- Other fields
    address TEXT,
    notes TEXT,
    photo_url TEXT,
    birthday DATE,

    -- Sync tracking
    etag VARCHAR(255),
    local_modified BOOLEAN DEFAULT false,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_contacts_user_id ON contacts(user_id);
CREATE INDEX IF NOT EXISTS idx_contacts_address_book_id ON contacts(address_book_id);
CREATE INDEX IF NOT EXISTS idx_contacts_email ON contacts(email);
CREATE INDEX IF NOT EXISTS idx_contacts_full_name ON contacts(full_name);
CREATE UNIQUE INDEX IF NOT EXISTS idx_contacts_uid ON contacts(address_book_id, uid);
CREATE INDEX IF NOT EXISTS idx_contacts_remote_id ON contacts(address_book_id, remote_id) WHERE remote_id IS NOT NULL;

-- Full text search index for contacts
CREATE INDEX IF NOT EXISTS idx_contacts_search ON contacts
USING gin(to_tsvector('simple', COALESCE(full_name, '') || ' ' || COALESCE(email, '') || ' ' || COALESCE(organization, '')));
