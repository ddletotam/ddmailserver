-- Add address book sync control columns
-- enabled: completely disable address book (no sync, no CardDAV display)
-- reverse_sync: sync local changes back to external CardDAV server

ALTER TABLE address_books ADD COLUMN IF NOT EXISTS enabled BOOLEAN DEFAULT true;
ALTER TABLE address_books ADD COLUMN IF NOT EXISTS reverse_sync BOOLEAN DEFAULT false;

-- Set reverse_sync=true for all existing CardDAV address books
UPDATE address_books ab
SET reverse_sync = true
FROM contact_sources cs
WHERE ab.source_id = cs.id
AND cs.source_type = 'carddav'
AND ab.reverse_sync = false;

-- Fix contact_sync_queue: remove CASCADE on contact_id
-- This allows queuing delete operations BEFORE deleting the contact
ALTER TABLE contact_sync_queue DROP CONSTRAINT IF EXISTS contact_sync_queue_contact_id_fkey;
