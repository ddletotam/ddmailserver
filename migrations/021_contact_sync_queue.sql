-- Bidirectional CardDAV contact sync queue
-- Queues local contact changes for reverse sync to external CardDAV servers
CREATE TABLE IF NOT EXISTS contact_sync_queue (
    id SERIAL PRIMARY KEY,
    contact_id BIGINT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    address_book_id BIGINT NOT NULL REFERENCES address_books(id) ON DELETE CASCADE,
    source_id BIGINT NOT NULL REFERENCES contact_sources(id) ON DELETE CASCADE,
    uid VARCHAR(512) NOT NULL,
    remote_id VARCHAR(512),
    vcard_data TEXT,
    operation VARCHAR(10) NOT NULL, -- 'create', 'update', 'delete'
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(contact_id)
);

CREATE INDEX IF NOT EXISTS idx_contact_sync_queue_source
    ON contact_sync_queue(source_id);
