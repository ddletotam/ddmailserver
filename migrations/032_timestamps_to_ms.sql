-- Migration: convert ALL timestamp columns to BIGINT (milliseconds since epoch).
-- Nullable timestamps remain nullable (NULL stays NULL).
-- Adds _tz (SMALLINT, UTC offset in minutes) for sender-meaningful dates.
--
-- Strategy per column:
--   1. ADD new_col BIGINT
--   2. UPDATE new_col = EXTRACT(EPOCH FROM old_col) * 1000
--   3. DROP old_col
--   4. RENAME new_col → old_col
--
-- Wrapped in a transaction for atomicity.

BEGIN;

-- ══════════════════════════════════════════════════════════════
-- messages (date, created_at, updated_at, soft_deleted_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE messages ADD COLUMN date_ms BIGINT;
UPDATE messages SET date_ms = EXTRACT(EPOCH FROM date) * 1000 WHERE date IS NOT NULL;
ALTER TABLE messages DROP COLUMN date;
ALTER TABLE messages RENAME COLUMN date_ms TO date;
ALTER TABLE messages ADD COLUMN date_tz SMALLINT DEFAULT 0;

ALTER TABLE messages ADD COLUMN created_at_ms BIGINT;
UPDATE messages SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE messages DROP COLUMN created_at;
ALTER TABLE messages RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE messages ADD COLUMN updated_at_ms BIGINT;
UPDATE messages SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE messages DROP COLUMN updated_at;
ALTER TABLE messages RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE messages ADD COLUMN soft_deleted_at_ms BIGINT;
UPDATE messages SET soft_deleted_at_ms = EXTRACT(EPOCH FROM soft_deleted_at) * 1000 WHERE soft_deleted_at IS NOT NULL;
ALTER TABLE messages DROP COLUMN soft_deleted_at;
ALTER TABLE messages RENAME COLUMN soft_deleted_at_ms TO soft_deleted_at;

-- ══════════════════════════════════════════════════════════════
-- users (created_at, updated_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE users ADD COLUMN created_at_ms BIGINT;
UPDATE users SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE users DROP COLUMN created_at;
ALTER TABLE users RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE users ADD COLUMN updated_at_ms BIGINT;
UPDATE users SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE users DROP COLUMN updated_at;
ALTER TABLE users RENAME COLUMN updated_at_ms TO updated_at;

-- ══════════════════════════════════════════════════════════════
-- accounts (created_at, updated_at, last_sync, oauth_token_expiry)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE accounts ADD COLUMN created_at_ms BIGINT;
UPDATE accounts SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE accounts DROP COLUMN created_at;
ALTER TABLE accounts RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE accounts ADD COLUMN updated_at_ms BIGINT;
UPDATE accounts SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE accounts DROP COLUMN updated_at;
ALTER TABLE accounts RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE accounts ADD COLUMN last_sync_ms BIGINT;
UPDATE accounts SET last_sync_ms = EXTRACT(EPOCH FROM last_sync) * 1000 WHERE last_sync IS NOT NULL;
ALTER TABLE accounts DROP COLUMN last_sync;
ALTER TABLE accounts RENAME COLUMN last_sync_ms TO last_sync;

ALTER TABLE accounts ADD COLUMN oauth_token_expiry_ms BIGINT;
UPDATE accounts SET oauth_token_expiry_ms = EXTRACT(EPOCH FROM oauth_token_expiry) * 1000 WHERE oauth_token_expiry IS NOT NULL;
ALTER TABLE accounts DROP COLUMN oauth_token_expiry;
ALTER TABLE accounts RENAME COLUMN oauth_token_expiry_ms TO oauth_token_expiry;

-- ══════════════════════════════════════════════════════════════
-- folders (created_at, updated_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE folders ADD COLUMN created_at_ms BIGINT;
UPDATE folders SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE folders DROP COLUMN created_at;
ALTER TABLE folders RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE folders ADD COLUMN updated_at_ms BIGINT;
UPDATE folders SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE folders DROP COLUMN updated_at;
ALTER TABLE folders RENAME COLUMN updated_at_ms TO updated_at;

-- ══════════════════════════════════════════════════════════════
-- attachments (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE attachments ADD COLUMN created_at_ms BIGINT;
UPDATE attachments SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE attachments DROP COLUMN created_at;
ALTER TABLE attachments RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- outbox_messages (created_at, updated_at, sent_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE outbox_messages ADD COLUMN created_at_ms BIGINT;
UPDATE outbox_messages SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE outbox_messages DROP COLUMN created_at;
ALTER TABLE outbox_messages RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE outbox_messages ADD COLUMN updated_at_ms BIGINT;
UPDATE outbox_messages SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE outbox_messages DROP COLUMN updated_at;
ALTER TABLE outbox_messages RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE outbox_messages ADD COLUMN sent_at_ms BIGINT;
UPDATE outbox_messages SET sent_at_ms = EXTRACT(EPOCH FROM sent_at) * 1000 WHERE sent_at IS NOT NULL;
ALTER TABLE outbox_messages DROP COLUMN sent_at;
ALTER TABLE outbox_messages RENAME COLUMN sent_at_ms TO sent_at;

-- ══════════════════════════════════════════════════════════════
-- outbox_attachments (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE outbox_attachments ADD COLUMN created_at_ms BIGINT;
UPDATE outbox_attachments SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE outbox_attachments DROP COLUMN created_at;
ALTER TABLE outbox_attachments RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- domains (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE domains ADD COLUMN created_at_ms BIGINT;
UPDATE domains SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE domains DROP COLUMN created_at;
ALTER TABLE domains RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- mailboxes (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE mailboxes ADD COLUMN created_at_ms BIGINT;
UPDATE mailboxes SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE mailboxes DROP COLUMN created_at;
ALTER TABLE mailboxes RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- account_logs (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE account_logs ADD COLUMN created_at_ms BIGINT;
UPDATE account_logs SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE account_logs DROP COLUMN created_at;
ALTER TABLE account_logs RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- flag_sync_queue (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE flag_sync_queue ADD COLUMN created_at_ms BIGINT;
UPDATE flag_sync_queue SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE flag_sync_queue DROP COLUMN created_at;
ALTER TABLE flag_sync_queue RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- folder_subscriptions (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE folder_subscriptions ADD COLUMN created_at_ms BIGINT;
UPDATE folder_subscriptions SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE folder_subscriptions DROP COLUMN created_at;
ALTER TABLE folder_subscriptions RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- sync_status (last_sync, updated_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE sync_status ADD COLUMN last_sync_ms BIGINT;
UPDATE sync_status SET last_sync_ms = EXTRACT(EPOCH FROM last_sync) * 1000 WHERE last_sync IS NOT NULL;
ALTER TABLE sync_status DROP COLUMN last_sync;
ALTER TABLE sync_status RENAME COLUMN last_sync_ms TO last_sync;

ALTER TABLE sync_status ADD COLUMN updated_at_ms BIGINT;
UPDATE sync_status SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE sync_status DROP COLUMN updated_at;
ALTER TABLE sync_status RENAME COLUMN updated_at_ms TO updated_at;

-- ══════════════════════════════════════════════════════════════
-- calendar_sources (created_at, updated_at, last_sync, oauth_token_expiry)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE calendar_sources ADD COLUMN created_at_ms BIGINT;
UPDATE calendar_sources SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE calendar_sources DROP COLUMN created_at;
ALTER TABLE calendar_sources RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE calendar_sources ADD COLUMN updated_at_ms BIGINT;
UPDATE calendar_sources SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE calendar_sources DROP COLUMN updated_at;
ALTER TABLE calendar_sources RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE calendar_sources ADD COLUMN last_sync_ms BIGINT;
UPDATE calendar_sources SET last_sync_ms = EXTRACT(EPOCH FROM last_sync) * 1000 WHERE last_sync IS NOT NULL;
ALTER TABLE calendar_sources DROP COLUMN last_sync;
ALTER TABLE calendar_sources RENAME COLUMN last_sync_ms TO last_sync;

ALTER TABLE calendar_sources ADD COLUMN oauth_token_expiry_ms BIGINT;
UPDATE calendar_sources SET oauth_token_expiry_ms = EXTRACT(EPOCH FROM oauth_token_expiry) * 1000 WHERE oauth_token_expiry IS NOT NULL;
ALTER TABLE calendar_sources DROP COLUMN oauth_token_expiry;
ALTER TABLE calendar_sources RENAME COLUMN oauth_token_expiry_ms TO oauth_token_expiry;

-- ══════════════════════════════════════════════════════════════
-- calendars (created_at, updated_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE calendars ADD COLUMN created_at_ms BIGINT;
UPDATE calendars SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE calendars DROP COLUMN created_at;
ALTER TABLE calendars RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE calendars ADD COLUMN updated_at_ms BIGINT;
UPDATE calendars SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE calendars DROP COLUMN updated_at;
ALTER TABLE calendars RENAME COLUMN updated_at_ms TO updated_at;

-- ══════════════════════════════════════════════════════════════
-- calendar_events (dtstart, dtend, created_at, updated_at, soft_deleted_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE calendar_events ADD COLUMN dtstart_ms BIGINT;
UPDATE calendar_events SET dtstart_ms = EXTRACT(EPOCH FROM dtstart) * 1000 WHERE dtstart IS NOT NULL;
ALTER TABLE calendar_events DROP COLUMN dtstart;
ALTER TABLE calendar_events RENAME COLUMN dtstart_ms TO dtstart;
ALTER TABLE calendar_events ADD COLUMN dtstart_tz SMALLINT DEFAULT 0;

ALTER TABLE calendar_events ADD COLUMN dtend_ms BIGINT;
UPDATE calendar_events SET dtend_ms = EXTRACT(EPOCH FROM dtend) * 1000 WHERE dtend IS NOT NULL;
ALTER TABLE calendar_events DROP COLUMN dtend;
ALTER TABLE calendar_events RENAME COLUMN dtend_ms TO dtend;
ALTER TABLE calendar_events ADD COLUMN dtend_tz SMALLINT DEFAULT 0;

ALTER TABLE calendar_events ADD COLUMN created_at_ms BIGINT;
UPDATE calendar_events SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE calendar_events DROP COLUMN created_at;
ALTER TABLE calendar_events RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE calendar_events ADD COLUMN updated_at_ms BIGINT;
UPDATE calendar_events SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE calendar_events DROP COLUMN updated_at;
ALTER TABLE calendar_events RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE calendar_events ADD COLUMN soft_deleted_at_ms BIGINT;
UPDATE calendar_events SET soft_deleted_at_ms = EXTRACT(EPOCH FROM soft_deleted_at) * 1000 WHERE soft_deleted_at IS NOT NULL;
ALTER TABLE calendar_events DROP COLUMN soft_deleted_at;
ALTER TABLE calendar_events RENAME COLUMN soft_deleted_at_ms TO soft_deleted_at;

-- ══════════════════════════════════════════════════════════════
-- calendar_event_sync_queue (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE calendar_event_sync_queue ADD COLUMN created_at_ms BIGINT;
UPDATE calendar_event_sync_queue SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE calendar_event_sync_queue DROP COLUMN created_at;
ALTER TABLE calendar_event_sync_queue RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- calendar_attendees (created_at, updated_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE calendar_attendees ADD COLUMN created_at_ms BIGINT;
UPDATE calendar_attendees SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE calendar_attendees DROP COLUMN created_at;
ALTER TABLE calendar_attendees RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE calendar_attendees ADD COLUMN updated_at_ms BIGINT;
UPDATE calendar_attendees SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE calendar_attendees DROP COLUMN updated_at;
ALTER TABLE calendar_attendees RENAME COLUMN updated_at_ms TO updated_at;

-- ══════════════════════════════════════════════════════════════
-- contact_sources (created_at, updated_at, last_sync, oauth_token_expiry)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE contact_sources ADD COLUMN created_at_ms BIGINT;
UPDATE contact_sources SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE contact_sources DROP COLUMN created_at;
ALTER TABLE contact_sources RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE contact_sources ADD COLUMN updated_at_ms BIGINT;
UPDATE contact_sources SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE contact_sources DROP COLUMN updated_at;
ALTER TABLE contact_sources RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE contact_sources ADD COLUMN last_sync_ms BIGINT;
UPDATE contact_sources SET last_sync_ms = EXTRACT(EPOCH FROM last_sync) * 1000 WHERE last_sync IS NOT NULL;
ALTER TABLE contact_sources DROP COLUMN last_sync;
ALTER TABLE contact_sources RENAME COLUMN last_sync_ms TO last_sync;

ALTER TABLE contact_sources ADD COLUMN oauth_token_expiry_ms BIGINT;
UPDATE contact_sources SET oauth_token_expiry_ms = EXTRACT(EPOCH FROM oauth_token_expiry) * 1000 WHERE oauth_token_expiry IS NOT NULL;
ALTER TABLE contact_sources DROP COLUMN oauth_token_expiry;
ALTER TABLE contact_sources RENAME COLUMN oauth_token_expiry_ms TO oauth_token_expiry;

-- ══════════════════════════════════════════════════════════════
-- contact_sync_queue (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE contact_sync_queue ADD COLUMN created_at_ms BIGINT;
UPDATE contact_sync_queue SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE contact_sync_queue DROP COLUMN created_at;
ALTER TABLE contact_sync_queue RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- address_books (created_at, updated_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE address_books ADD COLUMN created_at_ms BIGINT;
UPDATE address_books SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE address_books DROP COLUMN created_at;
ALTER TABLE address_books RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE address_books ADD COLUMN updated_at_ms BIGINT;
UPDATE address_books SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE address_books DROP COLUMN updated_at;
ALTER TABLE address_books RENAME COLUMN updated_at_ms TO updated_at;

-- ══════════════════════════════════════════════════════════════
-- contacts (created_at, updated_at, soft_deleted_at)
-- birthday stays special — see below
-- ══════════════════════════════════════════════════════════════
ALTER TABLE contacts ADD COLUMN created_at_ms BIGINT;
UPDATE contacts SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE contacts DROP COLUMN created_at;
ALTER TABLE contacts RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE contacts ADD COLUMN updated_at_ms BIGINT;
UPDATE contacts SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE contacts DROP COLUMN updated_at;
ALTER TABLE contacts RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE contacts ADD COLUMN soft_deleted_at_ms BIGINT;
UPDATE contacts SET soft_deleted_at_ms = EXTRACT(EPOCH FROM soft_deleted_at) * 1000 WHERE soft_deleted_at IS NOT NULL;
ALTER TABLE contacts DROP COLUMN soft_deleted_at;
ALTER TABLE contacts RENAME COLUMN soft_deleted_at_ms TO soft_deleted_at;

-- ══════════════════════════════════════════════════════════════
-- sender_reputation (last_seen)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE sender_reputation ADD COLUMN last_seen_ms BIGINT;
UPDATE sender_reputation SET last_seen_ms = EXTRACT(EPOCH FROM last_seen) * 1000 WHERE last_seen IS NOT NULL;
ALTER TABLE sender_reputation DROP COLUMN last_seen;
ALTER TABLE sender_reputation RENAME COLUMN last_seen_ms TO last_seen;

-- ══════════════════════════════════════════════════════════════
-- spam_feedback (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE spam_feedback ADD COLUMN created_at_ms BIGINT;
UPDATE spam_feedback SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE spam_feedback DROP COLUMN created_at;
ALTER TABLE spam_feedback RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- user_disabled_spam_checks (disabled_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE user_disabled_spam_checks ADD COLUMN disabled_at_ms BIGINT;
UPDATE user_disabled_spam_checks SET disabled_at_ms = EXTRACT(EPOCH FROM disabled_at) * 1000 WHERE disabled_at IS NOT NULL;
ALTER TABLE user_disabled_spam_checks DROP COLUMN disabled_at;
ALTER TABLE user_disabled_spam_checks RENAME COLUMN disabled_at_ms TO disabled_at;

-- ══════════════════════════════════════════════════════════════
-- user_spam_rules (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE user_spam_rules ADD COLUMN created_at_ms BIGINT;
UPDATE user_spam_rules SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE user_spam_rules DROP COLUMN created_at;
ALTER TABLE user_spam_rules RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- message_auth (created_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE message_auth ADD COLUMN created_at_ms BIGINT;
UPDATE message_auth SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE message_auth DROP COLUMN created_at;
ALTER TABLE message_auth RENAME COLUMN created_at_ms TO created_at;

-- ══════════════════════════════════════════════════════════════
-- system_settings (updated_at)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE system_settings ADD COLUMN updated_at_ms BIGINT;
UPDATE system_settings SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE system_settings DROP COLUMN updated_at;
ALTER TABLE system_settings RENAME COLUMN updated_at_ms TO updated_at;

-- ══════════════════════════════════════════════════════════════
-- eas_devices (created_at, updated_at, first_sync, last_sync)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE eas_devices ADD COLUMN created_at_ms BIGINT;
UPDATE eas_devices SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE eas_devices DROP COLUMN created_at;
ALTER TABLE eas_devices RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE eas_devices ADD COLUMN updated_at_ms BIGINT;
UPDATE eas_devices SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE eas_devices DROP COLUMN updated_at;
ALTER TABLE eas_devices RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE eas_devices ADD COLUMN first_sync_ms BIGINT;
UPDATE eas_devices SET first_sync_ms = EXTRACT(EPOCH FROM first_sync) * 1000 WHERE first_sync IS NOT NULL;
ALTER TABLE eas_devices DROP COLUMN first_sync;
ALTER TABLE eas_devices RENAME COLUMN first_sync_ms TO first_sync;

ALTER TABLE eas_devices ADD COLUMN last_sync_ms BIGINT;
UPDATE eas_devices SET last_sync_ms = EXTRACT(EPOCH FROM last_sync) * 1000 WHERE last_sync IS NOT NULL;
ALTER TABLE eas_devices DROP COLUMN last_sync;
ALTER TABLE eas_devices RENAME COLUMN last_sync_ms TO last_sync;

-- ══════════════════════════════════════════════════════════════
-- eas_folder_sync (created_at, updated_at, last_sync)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE eas_folder_sync ADD COLUMN created_at_ms BIGINT;
UPDATE eas_folder_sync SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE eas_folder_sync DROP COLUMN created_at;
ALTER TABLE eas_folder_sync RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE eas_folder_sync ADD COLUMN updated_at_ms BIGINT;
UPDATE eas_folder_sync SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE eas_folder_sync DROP COLUMN updated_at;
ALTER TABLE eas_folder_sync RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE eas_folder_sync ADD COLUMN last_sync_ms BIGINT;
UPDATE eas_folder_sync SET last_sync_ms = EXTRACT(EPOCH FROM last_sync) * 1000 WHERE last_sync IS NOT NULL;
ALTER TABLE eas_folder_sync DROP COLUMN last_sync;
ALTER TABLE eas_folder_sync RENAME COLUMN last_sync_ms TO last_sync;

-- ══════════════════════════════════════════════════════════════
-- eas_sync_state (created_at, updated_at, last_sync)
-- ══════════════════════════════════════════════════════════════
ALTER TABLE eas_sync_state ADD COLUMN created_at_ms BIGINT;
UPDATE eas_sync_state SET created_at_ms = EXTRACT(EPOCH FROM created_at) * 1000 WHERE created_at IS NOT NULL;
ALTER TABLE eas_sync_state DROP COLUMN created_at;
ALTER TABLE eas_sync_state RENAME COLUMN created_at_ms TO created_at;

ALTER TABLE eas_sync_state ADD COLUMN updated_at_ms BIGINT;
UPDATE eas_sync_state SET updated_at_ms = EXTRACT(EPOCH FROM updated_at) * 1000 WHERE updated_at IS NOT NULL;
ALTER TABLE eas_sync_state DROP COLUMN updated_at;
ALTER TABLE eas_sync_state RENAME COLUMN updated_at_ms TO updated_at;

ALTER TABLE eas_sync_state ADD COLUMN last_sync_ms BIGINT;
UPDATE eas_sync_state SET last_sync_ms = EXTRACT(EPOCH FROM last_sync) * 1000 WHERE last_sync IS NOT NULL;
ALTER TABLE eas_sync_state DROP COLUMN last_sync;
ALTER TABLE eas_sync_state RENAME COLUMN last_sync_ms TO last_sync;

COMMIT;
