-- Add content_id for inline images (Content-ID header, e.g. "att123@dd.local")
-- NULL = regular file attachment, non-NULL = inline image referenced from HTML via cid:
ALTER TABLE outbox_attachments ADD COLUMN IF NOT EXISTS content_id VARCHAR(255);
