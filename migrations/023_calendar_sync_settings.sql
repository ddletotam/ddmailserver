-- Add calendar sync control columns
-- enabled: completely disable calendar (no sync, no CalDAV display)
-- reverse_sync: sync local changes back to external CalDAV server

ALTER TABLE calendars ADD COLUMN IF NOT EXISTS enabled BOOLEAN DEFAULT true;
ALTER TABLE calendars ADD COLUMN IF NOT EXISTS reverse_sync BOOLEAN DEFAULT false;

-- Set reverse_sync=true for all existing CalDAV calendars
UPDATE calendars c
SET reverse_sync = true
FROM calendar_sources s
WHERE c.source_id = s.id
AND s.source_type = 'caldav'
AND c.reverse_sync = false;

-- Fix calendar_event_sync_queue: remove CASCADE on event_id
-- This allows queuing delete operations BEFORE deleting the event
ALTER TABLE calendar_event_sync_queue DROP CONSTRAINT IF EXISTS calendar_event_sync_queue_event_id_fkey;
