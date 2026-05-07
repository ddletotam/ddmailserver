-- Store the original RFC-822 source for each received message so View Source
-- in the desktop client can show what really came over the wire instead of a
-- stitched-together reconstruction (which loses HTML, alt-parts, headers, etc).
--
-- BYTEA, nullable: legacy rows pre-dating this migration won't have the source
-- and the API falls back to the old reconstruction path. New rows (MX delivery
-- + saveToSentFolder) populate it.

ALTER TABLE messages ADD COLUMN IF NOT EXISTS raw_email BYTEA;
