-- Refine the message-identity guarantee: dedup applies ONLY to messages that
-- actually carry a Message-ID. Upstream/MX ingress without one is already
-- rejected, but LOCAL-origin rows (drafts, calendar warnings) may legitimately
-- have an empty message_id — and a full UNIQUE(user_id, message_id) would
-- collide every such row after the first, silently dropping the user's second
-- draft. A partial unique index excludes empties (NULL or ''), so they never
-- conflict, while (user_id, Message-ID) stays globally unique for real mail.

ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_user_message_id_key;

CREATE UNIQUE INDEX IF NOT EXISTS messages_user_message_id_uq
    ON messages (user_id, message_id)
    WHERE COALESCE(message_id, '') <> '';
