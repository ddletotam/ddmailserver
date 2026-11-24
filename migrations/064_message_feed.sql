-- Feed version of a message: the body without the quoted earlier
-- correspondence and without the signature (internal/feedtrim). The message
-- itself is never changed; the feed shows this copy and opens the original
-- behind "•••".
--
-- A row exists for every computed message. feed_html / feed_text are NULL
-- when nothing was cut: the client shows the original as before. own_tail is
-- the sender's own lines (before the history cut, lower case, one per line) —
-- what later messages of the same sender are compared with to find the
-- signature. version is the algorithm version (feedtrim.Version): rows of an
-- older version are recomputed by the worker.
CREATE TABLE IF NOT EXISTS message_feed (
    message_row_id BIGINT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    user_id        BIGINT  NOT NULL,
    sender         TEXT    NOT NULL,  -- lower-case address of from_addr
    subject        TEXT    NOT NULL DEFAULT '',
    version        INT     NOT NULL,
    history_cut    BOOLEAN NOT NULL DEFAULT FALSE,
    signature_cut  BOOLEAN NOT NULL DEFAULT FALSE,
    feed_html      TEXT,
    feed_text      TEXT,
    own_tail       TEXT    NOT NULL DEFAULT '',
    computed_at    BIGINT  NOT NULL  -- unix millis
);

CREATE INDEX IF NOT EXISTS idx_message_feed_sender ON message_feed (user_id, sender, message_row_id DESC);

-- Messages waiting for (re)computation. Filled by a trigger on new messages
-- and body changes, by the worker at start (rows of an older version, messages
-- never computed) and when a message arrives whose replies were computed
-- before it — those can now lose the quote of it.
CREATE TABLE IF NOT EXISTS message_feed_queue (
    message_row_id BIGINT PRIMARY KEY,
    enqueued_at    BIGINT NOT NULL
);

CREATE OR REPLACE FUNCTION enqueue_message_feed() RETURNS trigger AS $$
BEGIN
    INSERT INTO message_feed_queue (message_row_id, enqueued_at)
    VALUES (NEW.id, (EXTRACT(EPOCH FROM now()) * 1000)::BIGINT)
    ON CONFLICT (message_row_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_message_feed_ins ON messages;
DROP TRIGGER IF EXISTS trg_message_feed_upd ON messages;

CREATE TRIGGER trg_message_feed_ins
    AFTER INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION enqueue_message_feed();

CREATE TRIGGER trg_message_feed_upd
    AFTER UPDATE ON messages
    FOR EACH ROW
    WHEN (
        OLD.body      IS DISTINCT FROM NEW.body      OR
        OLD.body_html IS DISTINCT FROM NEW.body_html OR
        OLD.from_addr IS DISTINCT FROM NEW.from_addr
    )
    EXECUTE FUNCTION enqueue_message_feed();
