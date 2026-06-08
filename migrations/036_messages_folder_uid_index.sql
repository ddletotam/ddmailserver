-- Partial composite index for the hot IMAP read path.
--
-- GetMessagesByFolder / GetMessagesByFolderMeta / GetFolderStatusCounts all run:
--   WHERE folder_id = $1
--     AND deleted = false
--     AND (soft_deleted = false OR soft_deleted IS NULL)
--     AND (is_spam = false OR is_spam IS NULL)
--   ORDER BY uid ASC
--
-- Without this index Postgres scans every row in the folder (live + vault +
-- spam — the vault alone can be 90%+ of the rows) and sorts by uid in memory.
-- The predicate below matches the query verbatim so the planner can use it; the
-- index then holds only live rows, pre-ordered by (folder_id, uid).
CREATE INDEX IF NOT EXISTS idx_messages_folder_uid_live
    ON messages (folder_id, uid)
    WHERE deleted = false
      AND (soft_deleted = false OR soft_deleted IS NULL)
      AND (is_spam = false OR is_spam IS NULL);
