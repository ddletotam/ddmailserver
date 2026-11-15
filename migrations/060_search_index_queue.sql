-- Search index outbox: messages whose Meilisearch document is stale.
--
-- The search index used to be filled only by the full reindex at server start,
-- so a message that arrived (MX, local delivery, Sent copy, IMAP APPEND,
-- upstream sync, import) was not searchable until the next restart, and a
-- deleted or moved one kept its old document. Instead of asking every write
-- path to remember the index — the same reasoning as the change journal in
-- 043 — a trigger on messages records the row id here, and the server's
-- search worker drains the table into Meilisearch in the background:
-- the row still exists → (re)index it, the row is gone → delete the document.
--
-- One row per message: a burst of changes to the same message collapses into
-- one entry. `seq` moves forward on every new change, so the worker removes
-- only the entry it has indexed — a change that lands while the worker is
-- busy keeps its entry and is picked up on the next pass.
--
-- No foreign key on message_row_id: a delete must outlive the message row.

CREATE SEQUENCE IF NOT EXISTS search_index_queue_seq;

CREATE TABLE IF NOT EXISTS search_index_queue (
    message_row_id BIGINT PRIMARY KEY,  -- messages.id
    seq            BIGINT NOT NULL DEFAULT nextval('search_index_queue_seq'),
    enqueued_at    BIGINT NOT NULL      -- unix millis
);

CREATE INDEX IF NOT EXISTS idx_search_index_queue_seq ON search_index_queue (seq);

CREATE OR REPLACE FUNCTION enqueue_search_index() RETURNS trigger AS $$
DECLARE
    rid BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        rid := OLD.id;
    ELSE
        rid := NEW.id;
    END IF;

    INSERT INTO search_index_queue (message_row_id, seq, enqueued_at)
    VALUES (rid, nextval('search_index_queue_seq'), (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT)
    ON CONFLICT (message_row_id) DO UPDATE
        SET seq = EXCLUDED.seq, enqueued_at = EXCLUDED.enqueued_at;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_search_index_ins ON messages;
DROP TRIGGER IF EXISTS trg_search_index_del ON messages;
DROP TRIGGER IF EXISTS trg_search_index_upd ON messages;

CREATE TRIGGER trg_search_index_ins
    AFTER INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION enqueue_search_index();

CREATE TRIGGER trg_search_index_del
    AFTER DELETE ON messages
    FOR EACH ROW EXECUTE FUNCTION enqueue_search_index();

-- Only UPDATEs that change a field of the search document (see
-- search.messageToDocument). remote_uid bookkeeping, updated_at touches,
-- raw_email and the like do not reach the index.
CREATE TRIGGER trg_search_index_upd
    AFTER UPDATE ON messages
    FOR EACH ROW
    WHEN (
        OLD.user_id      IS DISTINCT FROM NEW.user_id      OR
        OLD.folder_id    IS DISTINCT FROM NEW.folder_id    OR
        OLD.account_id   IS DISTINCT FROM NEW.account_id   OR
        OLD.message_id   IS DISTINCT FROM NEW.message_id   OR
        OLD.subject      IS DISTINCT FROM NEW.subject      OR
        OLD.from_addr    IS DISTINCT FROM NEW.from_addr    OR
        OLD.to_addr      IS DISTINCT FROM NEW.to_addr      OR
        OLD.cc           IS DISTINCT FROM NEW.cc           OR
        OLD.date         IS DISTINCT FROM NEW.date         OR
        OLD.body         IS DISTINCT FROM NEW.body         OR
        OLD.body_html    IS DISTINCT FROM NEW.body_html    OR
        OLD.attachments  IS DISTINCT FROM NEW.attachments  OR
        OLD.seen         IS DISTINCT FROM NEW.seen         OR
        OLD.flagged      IS DISTINCT FROM NEW.flagged      OR
        OLD.spam_status  IS DISTINCT FROM NEW.spam_status  OR
        OLD.spam_score   IS DISTINCT FROM NEW.spam_score   OR
        OLD.soft_deleted IS DISTINCT FROM NEW.soft_deleted
    )
    EXECUTE FUNCTION enqueue_search_index();

-- Catch-up: everything that arrived or changed since the last full reindex is
-- missing from the index. Queue every message once; the worker re-sends them
-- in batches, and later starts no longer need a full reindex.
INSERT INTO search_index_queue (message_row_id, enqueued_at)
SELECT id, (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT FROM messages ORDER BY id
ON CONFLICT (message_row_id) DO NOTHING;
