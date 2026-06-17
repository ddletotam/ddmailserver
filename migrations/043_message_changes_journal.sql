-- Change journal (Kafka-style log) for the desktop client.
--
-- Replaces the "delta by updated_at + full resync every 24h to catch
-- deletions" model: the client tracks a single monotone `seq` and reads the
-- tail via /changes?since=seq. The killer feature is explicit DELETE
-- tombstones — the conversation delta never reported removals, so a message
-- deleted elsewhere only vanished after a full resync.
--
-- Identity is (user_id, Message-ID) — see migrations 041/042. The journal keys
-- on message_id; rows without one (legacy local drafts) are never journaled.
--
-- A DB-level trigger is the source of truth: every INSERT/UPDATE/DELETE on
-- messages records a change, so no application path can forget to emit one.

CREATE TABLE IF NOT EXISTS message_changes (
    seq        BIGSERIAL PRIMARY KEY,
    user_id    BIGINT  NOT NULL,
    message_id TEXT    NOT NULL,
    kind       SMALLINT NOT NULL,  -- 1 = upsert (new/changed, visible), 2 = delete (gone from client view)
    ts         BIGINT  NOT NULL    -- unix millis
);

CREATE INDEX IF NOT EXISTS idx_message_changes_user_seq ON message_changes (user_id, seq);

-- Single-row low-watermark: the highest seq that retention has pruned. A client
-- whose cursor is at or below this may have missed a tombstone → must resync.
CREATE TABLE IF NOT EXISTS journal_meta (
    only_row      BOOLEAN PRIMARY KEY DEFAULT true CHECK (only_row),
    low_watermark BIGINT  NOT NULL DEFAULT 0
);
INSERT INTO journal_meta (only_row, low_watermark) VALUES (true, 0)
    ON CONFLICT (only_row) DO NOTHING;

CREATE OR REPLACE FUNCTION record_message_change() RETURNS trigger AS $$
DECLARE
    mid  TEXT;
    uid  BIGINT;
    k    SMALLINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        mid := OLD.message_id; uid := OLD.user_id; k := 2;
    ELSE
        mid := NEW.message_id; uid := NEW.user_id;
        -- Invisible to the client (matches the conversation-list filter) = a delete.
        IF COALESCE(NEW.deleted, false) OR COALESCE(NEW.soft_deleted, false) OR COALESCE(NEW.is_spam, false) THEN
            k := 2;
        ELSE
            k := 1;
        END IF;
    END IF;

    -- No RFC Message-ID → no stable identity → nothing the client can key on.
    IF COALESCE(mid, '') = '' THEN
        RETURN COALESCE(NEW, OLD);
    END IF;

    INSERT INTO message_changes (user_id, message_id, kind, ts)
    VALUES (uid, mid, k, (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT);

    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_message_change_ins ON messages;
DROP TRIGGER IF EXISTS trg_message_change_del ON messages;
DROP TRIGGER IF EXISTS trg_message_change_upd ON messages;

CREATE TRIGGER trg_message_change_ins
    AFTER INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION record_message_change();

CREATE TRIGGER trg_message_change_del
    AFTER DELETE ON messages
    FOR EACH ROW EXECUTE FUNCTION record_message_change();

-- Only journal UPDATEs that change something the client's view depends on:
-- read/flag state, folder, or visibility (soft-delete / spam / deleted). Body
-- backfill, remote_uid bookkeeping, updated_at touches etc. are ignored — the
-- client fetches bodies on demand, so they're not journal-worthy noise.
CREATE TRIGGER trg_message_change_upd
    AFTER UPDATE ON messages
    FOR EACH ROW
    WHEN (
        OLD.seen        IS DISTINCT FROM NEW.seen        OR
        OLD.flagged     IS DISTINCT FROM NEW.flagged     OR
        OLD.answered    IS DISTINCT FROM NEW.answered    OR
        OLD.deleted     IS DISTINCT FROM NEW.deleted     OR
        OLD.soft_deleted IS DISTINCT FROM NEW.soft_deleted OR
        OLD.is_spam     IS DISTINCT FROM NEW.is_spam     OR
        OLD.folder_id   IS DISTINCT FROM NEW.folder_id
    )
    EXECUTE FUNCTION record_message_change();
