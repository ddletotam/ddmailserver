package db

import (
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/lib/pq"
)

// Building blocks for mirroring upstream deletions (a message that vanished
// from the source IMAP server). The decision — what counts as vanished, the
// safety rails — lives in internal/imap/client (sync) and
// internal/service/messages (the delete itself).

// RemoteRowPointer is the current state of a local row that mirrors a remote
// message: where it points upstream and whether it may be touched now.
type RemoteRowPointer struct {
	ID           int64
	MessageID    string
	AccountID    int64
	RemoteFolder string
	RemoteUID    uint32
	FolderID     int64
	UID          uint32
	FolderType   string // type of the local folder ("trash", "inbox", …)
	Deleted      bool   // \Deleted set locally, waiting for a client EXPUNGE
	SoftDeleted  bool   // already in the vault
	Pending      bool   // an unpushed flag_sync_queue row exists (local change or delete)
}

// GetRemoteRowPointers reads the current pointer of the given rows. Rows that
// no longer exist are absent from the map.
func (db *DB) GetRemoteRowPointers(ids []int64) (map[int64]RemoteRowPointer, error) {
	out := make(map[int64]RemoteRowPointer, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(`
		SELECT m.id, COALESCE(m.message_id, ''), COALESCE(m.account_id, 0),
		       COALESCE(m.remote_folder, ''), COALESCE(m.remote_uid, 0),
		       m.folder_id, m.uid, COALESCE(f.type, ''),
		       COALESCE(m.deleted, false), COALESCE(m.soft_deleted, false),
		       EXISTS (SELECT 1 FROM flag_sync_queue q WHERE q.message_id = m.id)
		  FROM messages m
		  LEFT JOIN folders f ON f.id = m.folder_id
		 WHERE m.id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("get remote row pointers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p RemoteRowPointer
		var ruid, uid int64
		if err := rows.Scan(&p.ID, &p.MessageID, &p.AccountID, &p.RemoteFolder, &ruid,
			&p.FolderID, &uid, &p.FolderType, &p.Deleted, &p.SoftDeleted, &p.Pending); err != nil {
			return nil, fmt.Errorf("scan remote row pointer: %w", err)
		}
		p.RemoteUID, p.UID = uint32(ruid), uint32(uid)
		out[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate remote row pointers: %w", err)
	}
	return out, nil
}

// RemoteFolderLiveCounts counts, per remote folder, the account's rows that
// still mirror a remote message (known remote UID, not in the vault). It is
// the denominator of the mass-deletion guard and how the sync notices rows
// pointing at a remote folder that disappeared from LIST.
func (db *DB) RemoteFolderLiveCounts(accountID int64) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT remote_folder, COUNT(*)
		  FROM messages
		 WHERE account_id = $1 AND remote_uid > 0 AND remote_folder IS NOT NULL
		   AND (soft_deleted = false OR soft_deleted IS NULL)
		 GROUP BY remote_folder`, accountID)
	if err != nil {
		return nil, fmt.Errorf("count remote folder rows: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("scan remote folder count: %w", err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate remote folder counts: %w", err)
	}
	return out, nil
}

// RepointRemoteMessage moves a row's upstream pointer to where the message
// was found now (another remote folder / UID), but only while the row still
// points where the caller saw it — a concurrent refresh wins. Reports whether
// the row was updated.
func (db *DB) RepointRemoteMessage(id, accountID int64, fromFolder string, fromUID uint32, toFolder string, toUID uint32) (bool, error) {
	res, err := db.Exec(`
		UPDATE messages SET remote_folder = $1, remote_uid = $2, updated_at = $3
		 WHERE id = $4 AND account_id = $5 AND remote_folder = $6 AND remote_uid = $7`,
		toFolder, int64(toUID), timeutil.Now(), id, accountID, fromFolder, int64(fromUID))
	if err != nil {
		return false, fmt.Errorf("repoint message %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("repoint message %d: rows affected: %w", id, err)
	}
	return n > 0, nil
}

// FolderClientSeqNums maps UIDs of a local folder to their 1-based positions
// in the sequence IMAP clients see: the UID-ordered union of visible and
// \Deleted-flagged (not yet expunged) rows — the same view the IMAP server's
// Mailbox.clientSeqMap computes. UIDs not in that view are absent.
func (db *DB) FolderClientSeqNums(folderID int64, uids []uint32) (map[uint32]uint32, error) {
	want := make(map[uint32]bool, len(uids))
	for _, u := range uids {
		want[u] = true
	}
	visible, err := db.GetFolderMessageRefs(folderID)
	if err != nil {
		return nil, err
	}
	flagged, err := db.GetDeletedFolderUIDs(folderID)
	if err != nil {
		return nil, err
	}
	seqOf := make(map[uint32]uint32, len(uids))
	i, j := 0, 0
	var pos uint32
	for i < len(visible) || j < len(flagged) {
		pos++
		var uid uint32
		if j >= len(flagged) || (i < len(visible) && visible[i].UID < flagged[j]) {
			uid = visible[i].UID
			i++
		} else {
			uid = flagged[j]
			j++
		}
		if want[uid] {
			seqOf[uid] = pos
		}
	}
	return seqOf, nil
}

// UpstreamVanishGuard is what the vanished-upstream delete re-checks under
// the row lock.
type UpstreamVanishGuard struct {
	SoftDeleted bool
	Pending     bool   // an unpushed flag_sync_queue row exists
	FolderType  string // local folder type
}

// UpstreamVanishGuard reads the guard state of a row already locked by
// LockMessageForUpdate in this transaction.
func (tx *Tx) UpstreamVanishGuard(messageID int64) (UpstreamVanishGuard, error) {
	var g UpstreamVanishGuard
	err := tx.QueryRow(`
		SELECT COALESCE(m.soft_deleted, false), COALESCE(f.type, ''),
		       EXISTS (SELECT 1 FROM flag_sync_queue q WHERE q.message_id = m.id)
		  FROM messages m
		  LEFT JOIN folders f ON f.id = m.folder_id
		 WHERE m.id = $1`, messageID).Scan(&g.SoftDeleted, &g.FolderType, &g.Pending)
	if err != nil {
		return g, fmt.Errorf("vanish guard of message %d: %w", messageID, err)
	}
	return g, nil
}

// ClearRemoteUID forgets the row's upstream UID: the remote copy is gone, so
// nothing may be pushed to that UID any more, and a row restored from the
// vault is a local-only message that the next sync must not delete again.
// remote_folder is kept as a hint of where the message lived.
func (tx *Tx) ClearRemoteUID(messageID int64) error {
	if _, err := tx.Exec(`UPDATE messages SET remote_uid = 0, updated_at = $1 WHERE id = $2`,
		timeutil.Now(), messageID); err != nil {
		return fmt.Errorf("clear remote uid of message %d: %w", messageID, err)
	}
	return nil
}
