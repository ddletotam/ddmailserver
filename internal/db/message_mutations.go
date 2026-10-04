package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/lib/pq"
)

// This file holds the row-level building blocks of message mutations (flags,
// deletion, upstream delete-sync) in a form that works both on *DB and inside
// a *Tx. The business rules — when a change must be pushed to the source
// server, what "delete" means — live in internal/service/messages, which
// composes these inside one transaction.

// MessageFlags is the persisted flag state of a message.
type MessageFlags struct {
	Seen     bool
	Flagged  bool
	Answered bool
	Deleted  bool
	Draft    bool
}

// MessageSyncState is what a mutation needs to know about a message: who owns
// it, its current flags and where it lives on the source server (AccountID 0 =
// delivered by our own MX, RemoteUID 0 = source UID never learned).
type MessageSyncState struct {
	ID           int64
	UserID       int64
	AccountID    int64
	RemoteUID    uint32
	RemoteFolder string
	Flags        MessageFlags
}

// InTx runs fn inside a transaction: committed when fn returns nil, rolled
// back otherwise (and on panic).
func (db *DB) InTx(ctx context.Context, fn func(*Tx) error) (err error) {
	tx, err := db.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				err = fmt.Errorf("%w (rollback: %v)", err, rbErr)
			}
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

const messageSyncStateColumns = `id, user_id, COALESCE(account_id, 0), COALESCE(remote_uid, 0),
	COALESCE(remote_folder, 'INBOX'), seen, flagged, answered, deleted, draft`

func scanMessageSyncState(sc interface{ Scan(...interface{}) error }) (*MessageSyncState, error) {
	st := &MessageSyncState{}
	err := sc.Scan(&st.ID, &st.UserID, &st.AccountID, &st.RemoteUID, &st.RemoteFolder,
		&st.Flags.Seen, &st.Flags.Flagged, &st.Flags.Answered, &st.Flags.Deleted, &st.Flags.Draft)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// LockMessageForUpdate reads a message's sync state and row-locks it until the
// transaction ends, so a concurrent writer cannot slip between the read and
// the write that follows. Returns ErrNotFound when the message does not exist
// or belongs to another user.
func (tx *Tx) LockMessageForUpdate(userID, messageID int64) (*MessageSyncState, error) {
	row := tx.QueryRow(`SELECT `+messageSyncStateColumns+`
		FROM messages WHERE id = $1 AND user_id = $2 FOR UPDATE`, messageID, userID)
	st, err := scanMessageSyncState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock message %d: %w", messageID, err)
	}
	return st, nil
}

// SetMessageFlags writes the full flag state of a message.
func (tx *Tx) SetMessageFlags(messageID int64, f MessageFlags) error {
	_, err := tx.Exec(`UPDATE messages
		SET seen = $1, flagged = $2, answered = $3, deleted = $4, draft = $5, updated_at = $6
		WHERE id = $7`,
		f.Seen, f.Flagged, f.Answered, f.Deleted, f.Draft, timeutil.Now(), messageID)
	if err != nil {
		return fmt.Errorf("set flags of message %d: %w", messageID, err)
	}
	return nil
}

// QueueFlagSync is the transactional variant of DB.QueueFlagSync.
func (tx *Tx) QueueFlagSync(messageID, accountID int64, remoteFolder string, remoteUID uint32, seen, flagged, answered, deleted bool) error {
	return queueFlagSync(tx, messageID, accountID, remoteFolder, remoteUID, seen, flagged, answered, deleted)
}

// SoftDeleteMessage is the transactional variant of DB.SoftDeleteMessage.
func (tx *Tx) SoftDeleteMessage(messageID int64) error {
	return softDeleteMessage(tx, messageID)
}

// EscapeLikePattern escapes the LIKE/ILIKE metacharacters (`%`, `_` and the
// escape character `\` itself) so that s matches only literally. PostgreSQL's
// default LIKE escape character is the backslash, so the result is safe to
// embed in a pattern without an ESCAPE clause.
func EscapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// senderPatterns builds the from_addr patterns for a sender purge: an exact
// "Name <addr>" address match and a "<anything@domain>" domain match. The
// values are escaped — a domain is user/client input and must not be able to
// widen the match ("%" would otherwise select every message of the user).
func senderPatterns(addresses, domains []string) []string {
	patterns := make([]string, 0, len(addresses)+len(domains))
	for _, a := range addresses {
		if a = strings.TrimSpace(a); a != "" {
			patterns = append(patterns, "%<"+EscapeLikePattern(a)+">%")
		}
	}
	for _, d := range domains {
		if d = strings.TrimSpace(d); d != "" {
			patterns = append(patterns, "%@"+EscapeLikePattern(d)+">%")
		}
	}
	return patterns
}

// LockPurgeTargets selects (and row-locks) the user's messages that a sender
// purge removes: the explicit ids plus every message whose From matches one of
// the sender addresses or domains (see senderPatterns).
func (tx *Tx) LockPurgeTargets(userID int64, ids []int64, addresses, domains []string) ([]*MessageSyncState, error) {
	if ids == nil {
		ids = []int64{}
	}
	rows, err := tx.Query(`SELECT `+messageSyncStateColumns+`
		FROM messages
		WHERE user_id = $1 AND (id = ANY($2) OR from_addr ILIKE ANY($3))
		ORDER BY id
		FOR UPDATE`,
		userID, pq.Array(ids), pq.Array(senderPatterns(addresses, domains)))
	if err != nil {
		return nil, fmt.Errorf("select purge targets: %w", err)
	}
	defer rows.Close()
	var out []*MessageSyncState
	for rows.Next() {
		st, err := scanMessageSyncState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan purge target: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate purge targets: %w", err)
	}
	return out, nil
}

// HardDeleteUserMessages permanently deletes the given messages of a user and
// returns how many rows went away.
func (tx *Tx) HardDeleteUserMessages(userID int64, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := tx.Exec(`DELETE FROM messages WHERE user_id = $1 AND id = ANY($2)`, userID, pq.Array(ids))
	if err != nil {
		return 0, fmt.Errorf("hard delete messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("hard delete messages: rows affected: %w", err)
	}
	return n, nil
}

// GetSenderAddrsByIDs returns the From header of each of the user's messages
// among ids (messages of other users are silently skipped).
func (db *DB) GetSenderAddrsByIDs(userID int64, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := db.Query(`SELECT from_addr FROM messages WHERE user_id = $1 AND id = ANY($2)`,
		userID, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("get sender addresses: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var from string
		if err := rows.Scan(&from); err != nil {
			return nil, fmt.Errorf("scan sender address: %w", err)
		}
		out = append(out, from)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sender addresses: %w", err)
	}
	return out, nil
}
