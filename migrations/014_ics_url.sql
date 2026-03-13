-- Add ICS URL field for calendar sources
-- Allows subscribing to remote ICS calendars (e.g., Google Calendar public URLs, Airbnb, etc.)

ALTER TABLE calendar_sources ADD COLUMN ics_url TEXT;

-- Update source_type comment: 'local', 'caldav', 'ics_import' (one-time), 'ics_url' (recurring sync)
COMMENT ON COLUMN calendar_sources.source_type IS 'Source type: local, caldav, ics_import (one-time file import), ics_url (recurring URL sync)';
