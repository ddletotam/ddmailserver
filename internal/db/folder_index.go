package db

import (
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/lib/pq"
)

// FolderMessageRef is the smallest per-message row an IMAP command needs to
// map sequence numbers and UIDs onto messages: the identity plus the flags
// (FETCH FLAGS and flag-only SEARCH are answered from it alone).
type FolderMessageRef struct {
	ID       int64
	UID      uint32
	Seen     bool
	Flagged  bool
	Answered bool
	Draft    bool
}

// metaByIDsChunk bounds the id array of one GetMessagesMetaByIDs query.
const metaByIDsChunk = 5000

// GetFolderMessageRefs returns every message the IMAP view of the folder
// shows — the same rows as GetMessagesByFolderMeta, without a limit — ordered
// by UID, so index i is sequence number i+1. Only the columns above are read
// (served by idx_messages_folder_uid_live), so it stays cheap for folders far
// past 10 000 messages; callers load metadata for the messages they select.
func (db *DB) GetFolderMessageRefs(folderID int64) ([]FolderMessageRef, error) {
	rows, err := db.Query(`
		SELECT id, uid, seen, flagged, answered, draft
		FROM messages
		WHERE folder_id = $1 AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
		      AND (is_spam = false OR is_spam IS NULL)
		ORDER BY uid ASC
	`, folderID)
	if err != nil {
		return nil, fmt.Errorf("failed to get folder message refs: %w", err)
	}
	defer rows.Close()

	var refs []FolderMessageRef
	for rows.Next() {
		var r FolderMessageRef
		var uid int64
		if err := rows.Scan(&r.ID, &uid, &r.Seen, &r.Flagged, &r.Answered, &r.Draft); err != nil {
			return nil, fmt.Errorf("failed to scan folder message ref: %w", err)
		}
		r.UID = uint32(uid)
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read folder message refs: %w", err)
	}
	return refs, nil
}

// GetDeletedFolderUIDs returns, ascending, the UIDs of the folder's messages
// flagged \Deleted but not expunged — the rows GetDeletedMessagesByFolder
// returns, without loading them.
func (db *DB) GetDeletedFolderUIDs(folderID int64) ([]uint32, error) {
	rows, err := db.Query(`
		SELECT uid
		FROM messages
		WHERE folder_id = $1 AND deleted = true AND (soft_deleted = false OR soft_deleted IS NULL)
		ORDER BY uid ASC
	`, folderID)
	if err != nil {
		return nil, fmt.Errorf("failed to get deleted UIDs: %w", err)
	}
	defer rows.Close()

	var uids []uint32
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("failed to scan deleted UID: %w", err)
		}
		uids = append(uids, uint32(uid))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read deleted UIDs: %w", err)
	}
	return uids, nil
}

// GetMessagesMetaByIDs is GetMessagesByIDs without body/body_html (like
// GetMessagesByFolderMeta), ordered by UID. Messages deleted meanwhile are
// simply absent from the result.
func (db *DB) GetMessagesMetaByIDs(ids []int64) ([]*models.Message, error) {
	var out []*models.Message
	for start := 0; start < len(ids); start += metaByIDsChunk {
		end := start + metaByIDsChunk
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := db.Query(`
			SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
			       date, '' AS body, '' AS body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
			       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
			       COALESCE(remote_uid, 0), COALESCE(remote_folder, 'INBOX'), created_at, updated_at
			FROM messages
			WHERE id = ANY($1) AND deleted = false AND (soft_deleted = false OR soft_deleted IS NULL)
			ORDER BY uid ASC
		`, pq.Array(ids[start:end]))
		if err != nil {
			return nil, fmt.Errorf("failed to get message metadata by IDs: %w", err)
		}
		msgs, err := scanMessages(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}
