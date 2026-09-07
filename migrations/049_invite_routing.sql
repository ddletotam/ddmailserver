-- Make incoming invites routable, and stop writes into placeholder calendars.
--
-- Both halves of this migration exist because of the same incident. An invite
-- addressed to one account was filed into an unrelated account's calendar, and
-- reverse sync then spent days PUTting it at that account's CalDAV server,
-- which answered 403 every time — one "calendar sync failed" email per day.
--
-- The routing code could not have chosen correctly with the data as it stood:
--
--  1. calendar_sources.account_id was NULL for every source but one, so
--     "the source belonging to the account this invite arrived on" matched
--     nothing and the choice fell through to a fallback that took whatever
--     calendar the user had added most recently.
--
--  2. The destination it landed on was a placeholder row — the fallback that
--     discovery falls back to when it cannot enumerate collections, holding
--     the SOURCE URL as its remote_id. For a SOGo server that URL is the
--     principal's home, not a collection: nothing can ever be written there.

-- Part 1: bind sources to the mail account they authenticate as.
--
-- caldav_username is the reliable signal — it is the address the source logs
-- in with. identity_email is not: it defaults to the user's default sending
-- identity, so unrelated sources routinely share one value.
UPDATE calendar_sources s
SET account_id = a.id,
    updated_at = (EXTRACT(EPOCH FROM NOW()) * 1000)::BIGINT
FROM accounts a
WHERE s.account_id IS NULL
  AND COALESCE(s.caldav_username, '') <> ''
  AND a.user_id = s.user_id
  AND LOWER(COALESCE(a.email, '')) = LOWER(s.caldav_username);

-- Part 2: mark surviving placeholder calendars read-only.
--
-- An empty placeholder is deleted by PruneDirectURLCalendar once discovery
-- starts working. One that has accumulated events is not — deleting it would
-- cascade the events away — so it stayed writable and kept collecting more.
-- Read-only is the honest state: every PUT aimed at it comes back 403.
--
-- Only rows whose source has at least one other calendar are touched. Where
-- the direct URL is the only calendar there is (Yandex, which has no
-- discovery), that row IS the working calendar and must keep its write bit.
--
-- The events inside are left alone on purpose. They cannot be re-parented
-- safely: an event is in here precisely because the destination was chosen
-- wrongly, so it may belong to a different account entirely, and moving it
-- into a real collection would push it to a remote it has nothing to do with.
UPDATE calendars c
SET can_write = FALSE,
    updated_at = (EXTRACT(EPOCH FROM NOW()) * 1000)::BIGINT
FROM calendar_sources s
WHERE c.source_id = s.id
  AND c.can_write = TRUE
  AND COALESCE(s.caldav_url, '') <> ''
  AND c.remote_id = s.caldav_url
  AND EXISTS (
      SELECT 1 FROM calendars other
      WHERE other.source_id = c.source_id
        AND other.id <> c.id
  );
