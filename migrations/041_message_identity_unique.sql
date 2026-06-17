-- Enforce the global message identity: (user_id, Message-ID).
-- The server no longer generates Message-IDs (ingress without one is rejected/
-- skipped — see MX 5xx and the IMAP-sync skip). This makes the natural key
-- enforceable at the DB level, killing the racy app-level check-then-insert
-- dedup and stabilizing the desktop contract (which moves off the volatile
-- serial id onto message_id).

-- 1) Collapse duplicate (user_id, message_id) rows, keeping the best copy:
--    prefer a live (not soft-deleted), non-spam, newest row.
DELETE FROM messages m
USING (
    SELECT id FROM (
        SELECT id,
               ROW_NUMBER() OVER (
                   PARTITION BY user_id, message_id
                   ORDER BY (soft_deleted IS TRUE), (is_spam IS TRUE), id DESC
               ) AS rn
        FROM messages
        WHERE COALESCE(message_id, '') <> ''
    ) ranked
    WHERE ranked.rn > 1
) dups
WHERE m.id = dups.id;

-- 2) A message is identified by (user_id, Message-ID). The DB now guarantees it.
ALTER TABLE messages
    ADD CONSTRAINT messages_user_message_id_key UNIQUE (user_id, message_id);
