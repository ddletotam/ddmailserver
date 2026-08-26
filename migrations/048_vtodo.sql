-- 048: tasks (VTODO) alongside events.
--
-- The CalDAV server has been advertising `<C:comp name="VTODO"/>` in
-- supported-calendar-component-set since it was written, while nothing in the
-- codebase could represent a task. iOS Reminders took that advertisement at
-- face value: it PUT its lists to us, we stored the raw ical_data on a
-- calendar_events row whose parser only reads VEVENT — hence rows with an empty
-- summary — and then the reverse sync pushed them at an iCloud *event*
-- collection, which refused every one with 403 and an empty body. Three of them
-- were still retrying after six rounds.
--
-- Rather than a parallel table, VTODO becomes a component of the existing row.
-- A task shares almost everything an event has (UID, SUMMARY, DESCRIPTION,
-- DTSTART, RRULE, STATUS, ical_data, ETag, remote_id), and a second table would
-- have meant a second copy of the reverse-sync queue, the dead letters, the
-- attendee join and the CalDAV routing. There are only 14 places that read
-- calendar_events; adding a filter to them is the cheaper half of the trade.
--
-- Applied manually via psql (RunMigrations is a no-op).

-- Which component this row is. Everything already stored is an event.
ALTER TABLE calendar_events
    ADD COLUMN IF NOT EXISTS component VARCHAR(10) NOT NULL DEFAULT 'VEVENT';

-- VTODO-only fields. DUE is the task's deadline: the counterpart of DTEND, but
-- not the same thing — an event without DTEND lasts an hour, a task without DUE
-- has no deadline at all, so it gets its own nullable column instead of reusing
-- dtend and hoping every reader remembers which meaning applies.
ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS due BIGINT;
ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS due_tz SMALLINT DEFAULT 0;

-- COMPLETED is a timestamp, not a flag: RFC 5545 records *when* a task was
-- finished, and clients show it.
ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS completed_at BIGINT;

-- 0..100, nullable — absent means the task never said.
ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS percent_complete SMALLINT;

-- 0 = undefined, 1 = highest … 9 = lowest (RFC 5545 §3.8.1.9).
ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS priority SMALLINT;

-- Every list view is "this calendar, this component", so the index carries both.
CREATE INDEX IF NOT EXISTS idx_calendar_events_component
    ON calendar_events(calendar_id, component);

-- What a collection actually accepts, comma-separated: 'VEVENT', 'VTODO' or
-- 'VEVENT,VTODO'. Stored per calendar rather than assumed, because it is the
-- thing that was being guessed:
--   * our own CalDAV server hardcoded "VEVENT and VTODO" for every calendar;
--   * the reverse sync assumed the remote would take whatever we sent.
-- Apple keeps reminder lists in their own VTODO-only collections, so a task and
-- an event genuinely do not belong in the same place upstream.
ALTER TABLE calendars
    ADD COLUMN IF NOT EXISTS supported_components VARCHAR(64) NOT NULL DEFAULT 'VEVENT';

-- Backfill: rows already holding a task are relabelled from their stored body.
-- Cheap and one-off — ical_data is authoritative here, since the parser never
-- populated the indexed columns for these rows in the first place.
UPDATE calendar_events
SET component = 'VTODO'
WHERE component = 'VEVENT'
  AND ical_data LIKE '%BEGIN:VTODO%'
  AND ical_data NOT LIKE '%BEGIN:VEVENT%';

-- Existing calendars keep the conservative answer until discovery reports
-- otherwise: claiming VTODO is what caused this whole class of failure.
UPDATE calendars SET supported_components = 'VEVENT' WHERE supported_components = '';
