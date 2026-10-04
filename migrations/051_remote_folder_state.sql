-- 051: incremental IMAP sync state per remote folder.
--
-- Until now every sync cycle did `UID FETCH 1:* (ENVELOPE FLAGS BODY.PEEK[])`
-- on every remote folder and threw away everything already known after
-- downloading it (prod: «synced 0 new messages (skipped 618 duplicates)» —
-- 618 full bodies every 5 minutes and on every IDLE wake-up). This table
-- remembers, per (account, remote folder), the UIDVALIDITY of the mailbox
-- and the highest UID already processed, so the next cycle downloads bodies
-- only for `UID last_seen_uid+1:*` and refreshes the rest with a light
-- `UID FETCH 1:last_seen_uid (FLAGS)`.
--
-- A row is trusted only while the server reports the same UIDVALIDITY; a
-- mismatch (or a missing row) makes the sync fall back to a full pass with
-- Message-ID dedup (envelopes first, bodies only for unknown messages) and
-- then rewrite the row.
--
-- remote_folder is the upstream mailbox name exactly as LIST returned it
-- (the same string as messages.remote_folder).
--
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS remote_folder_state (
    account_id    BIGINT       NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    remote_folder VARCHAR(255) NOT NULL,
    uid_validity  BIGINT       NOT NULL,
    last_seen_uid BIGINT       NOT NULL DEFAULT 0,
    updated_at    BIGINT       NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, remote_folder)
);
