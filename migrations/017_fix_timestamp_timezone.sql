-- Fix timestamp columns to use timezone-aware type
-- This fixes calendar sync issues where timestamps are interpreted as UTC instead of local time

-- Convert calendar_sources.last_sync to TIMESTAMPTZ
ALTER TABLE calendar_sources
ALTER COLUMN last_sync TYPE TIMESTAMPTZ
USING last_sync AT TIME ZONE 'Europe/Moscow';

-- Also fix other timestamp columns in calendar_sources
ALTER TABLE calendar_sources
ALTER COLUMN oauth_token_expiry TYPE TIMESTAMPTZ
USING oauth_token_expiry AT TIME ZONE 'Europe/Moscow';

ALTER TABLE calendar_sources
ALTER COLUMN created_at TYPE TIMESTAMPTZ
USING created_at AT TIME ZONE 'Europe/Moscow';

ALTER TABLE calendar_sources
ALTER COLUMN updated_at TYPE TIMESTAMPTZ
USING updated_at AT TIME ZONE 'Europe/Moscow';

-- Fix calendars table
ALTER TABLE calendars
ALTER COLUMN created_at TYPE TIMESTAMPTZ
USING created_at AT TIME ZONE 'Europe/Moscow';

ALTER TABLE calendars
ALTER COLUMN updated_at TYPE TIMESTAMPTZ
USING updated_at AT TIME ZONE 'Europe/Moscow';

-- Fix calendar_events table
ALTER TABLE calendar_events
ALTER COLUMN dtstart TYPE TIMESTAMPTZ
USING dtstart AT TIME ZONE 'Europe/Moscow';

ALTER TABLE calendar_events
ALTER COLUMN dtend TYPE TIMESTAMPTZ
USING dtend AT TIME ZONE 'Europe/Moscow';

ALTER TABLE calendar_events
ALTER COLUMN created_at TYPE TIMESTAMPTZ
USING created_at AT TIME ZONE 'Europe/Moscow';

ALTER TABLE calendar_events
ALTER COLUMN updated_at TYPE TIMESTAMPTZ
USING updated_at AT TIME ZONE 'Europe/Moscow';
