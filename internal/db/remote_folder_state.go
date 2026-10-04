package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// RemoteFolderState is the incremental-sync bookmark of one remote IMAP
// mailbox of an external account (table remote_folder_state, migration 051).
// LastSeenUID is meaningful only under the same UIDVALIDITY.
type RemoteFolderState struct {
	UIDValidity uint32
	LastSeenUID uint32
}

// GetRemoteFolderState returns the bookmark for (accountID, remoteFolder), or
// nil when the folder has never been synced incrementally.
func (db *DB) GetRemoteFolderState(accountID int64, remoteFolder string) (*RemoteFolderState, error) {
	var validity, last int64
	err := db.QueryRow(
		`SELECT uid_validity, last_seen_uid FROM remote_folder_state
		  WHERE account_id = $1 AND remote_folder = $2`,
		accountID, remoteFolder,
	).Scan(&validity, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get remote folder state: %w", err)
	}
	return &RemoteFolderState{UIDValidity: uint32(validity), LastSeenUID: uint32(last)}, nil
}

// SaveRemoteFolderState upserts the bookmark for (accountID, remoteFolder).
func (db *DB) SaveRemoteFolderState(accountID int64, remoteFolder string, st RemoteFolderState) error {
	_, err := db.Exec(
		`INSERT INTO remote_folder_state (account_id, remote_folder, uid_validity, last_seen_uid, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (account_id, remote_folder) DO UPDATE SET
		   uid_validity = EXCLUDED.uid_validity,
		   last_seen_uid = EXCLUDED.last_seen_uid,
		   updated_at = EXCLUDED.updated_at`,
		accountID, remoteFolder, int64(st.UIDValidity), int64(st.LastSeenUID), timeutil.Now(),
	)
	if err != nil {
		return fmt.Errorf("save remote folder state: %w", err)
	}
	return nil
}

// RemoteMessageRef identifies the local row that mirrors one remote message.
type RemoteMessageRef struct {
	ID        int64  // messages.id
	MessageID string // RFC 5322 Message-ID (or the derived one)
	From      string // stored From header — enough for CheckSpamRules
}

// GetRemoteMessageRefs maps remote UID → local row for every message this
// account owns in the given remote folder. The incremental flag pass fetches
// only (UID FLAGS) from the server, so this is how a remote UID is resolved
// back to the Message-ID that RefreshExistingFromRemote and the spam
// reclassification key on. Served by idx_messages_remote.
func (db *DB) GetRemoteMessageRefs(accountID int64, remoteFolder string) (map[uint32]RemoteMessageRef, error) {
	rows, err := db.Query(
		`SELECT id, remote_uid, message_id, COALESCE(from_addr, '')
		   FROM messages
		  WHERE account_id = $1 AND remote_folder = $2
		    AND remote_uid IS NOT NULL AND remote_uid > 0
		    AND message_id IS NOT NULL AND message_id <> ''`,
		accountID, remoteFolder,
	)
	if err != nil {
		return nil, fmt.Errorf("get remote message refs: %w", err)
	}
	defer rows.Close()
	refs := make(map[uint32]RemoteMessageRef)
	for rows.Next() {
		var uid int64
		var ref RemoteMessageRef
		if err := rows.Scan(&ref.ID, &uid, &ref.MessageID, &ref.From); err != nil {
			return nil, fmt.Errorf("scan remote message ref: %w", err)
		}
		refs[uint32(uid)] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate remote message refs: %w", err)
	}
	return refs, nil
}
