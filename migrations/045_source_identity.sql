-- 045: mandatory identity association on calendar & contact sources.
--
-- The desktop client is identity-keyed: everything is attributed to a concrete
-- email address, and every calendar/contact source must belong to exactly one
-- identity — even an ICS feed with no address of its own. No orphan sources.
-- See docs/unified-identity-aggregation.md.
--
-- Applied manually via psql (RunMigrations is a no-op).

ALTER TABLE calendar_sources ADD COLUMN IF NOT EXISTS identity_email TEXT;
ALTER TABLE contact_sources  ADD COLUMN IF NOT EXISTS identity_email TEXT;

-- Backfill calendar_sources: prefer the already-linked account's email …
UPDATE calendar_sources cs
SET identity_email = a.email
FROM accounts a
WHERE cs.account_id = a.id
  AND COALESCE(a.email, '') <> ''
  AND COALESCE(cs.identity_email, '') = '';

-- … else the user's first local mailbox, else their first external account,
-- else the username (last-resort, mirrors the /identities fallback).
UPDATE calendar_sources cs
SET identity_email = COALESCE(
  (SELECT m.local_part || '@' || d.domain
     FROM mailboxes m JOIN domains d ON m.domain_id = d.id
     WHERE m.user_id = cs.user_id ORDER BY m.id LIMIT 1),
  (SELECT a.email FROM accounts a
     WHERE a.user_id = cs.user_id AND COALESCE(a.email, '') <> '' ORDER BY a.id LIMIT 1),
  (SELECT u.username FROM users u WHERE u.id = cs.user_id)
)
WHERE COALESCE(cs.identity_email, '') = '';

-- Backfill contact_sources (no account link today): same fallback chain.
UPDATE contact_sources cs
SET identity_email = COALESCE(
  (SELECT m.local_part || '@' || d.domain
     FROM mailboxes m JOIN domains d ON m.domain_id = d.id
     WHERE m.user_id = cs.user_id ORDER BY m.id LIMIT 1),
  (SELECT a.email FROM accounts a
     WHERE a.user_id = cs.user_id AND COALESCE(a.email, '') <> '' ORDER BY a.id LIMIT 1),
  (SELECT u.username FROM users u WHERE u.id = cs.user_id)
)
WHERE COALESCE(cs.identity_email, '') = '';

-- Enforce the invariant now that every row has an identity.
ALTER TABLE calendar_sources ALTER COLUMN identity_email SET NOT NULL;
ALTER TABLE contact_sources  ALTER COLUMN identity_email SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_calendar_sources_identity
  ON calendar_sources (user_id, identity_email);
CREATE INDEX IF NOT EXISTS idx_contact_sources_identity
  ON contact_sources (user_id, identity_email);
