-- Fix messages with UID=0 by assigning sequential UIDs per folder
WITH numbered AS (
    SELECT id, folder_id,
           ROW_NUMBER() OVER (PARTITION BY folder_id ORDER BY id) +
           COALESCE((SELECT MAX(uid) FROM messages m2 WHERE m2.folder_id = messages.folder_id AND m2.uid > 0), 0) as new_uid
    FROM messages
    WHERE uid = 0
)
UPDATE messages
SET uid = numbered.new_uid
FROM numbered
WHERE messages.id = numbered.id;
