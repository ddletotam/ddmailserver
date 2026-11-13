package db

import (
	"database/sql"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// MessageDelivery is the delivery state written onto the sender's copy of an
// outgoing message (migration 056).
type MessageDelivery struct {
	Status string // outbox.Status*
	Reason string // outbox.Reason*; empty when there is nothing to explain
	Failed string // addresses that were not delivered to, comma-separated
}

// SetMessageDelivery writes the delivery state onto the user's message with
// this Message-ID and reports the row it touched and whether anything
// changed.
//
// A write that changes nothing is skipped (and reported as unchanged), so a
// retry that fails for the same reason as the last one neither bumps
// updated_at nor makes callers announce a status change that did not happen.
// A change bumps updated_at — the /conversations delta keys on it — and,
// through the journal trigger (056), lands in /changes.
//
// id is 0 when the user has no message with this Message-ID (the copy was
// deleted, or never made it into Sent).
func (db *DB) SetMessageDelivery(userID int64, messageID string, d MessageDelivery) (id int64, changed bool, err error) {
	if messageID == "" {
		return 0, false, nil
	}
	err = db.QueryRow(`
		UPDATE messages
		SET delivery_status = $3, delivery_reason = $4, delivery_failed = $5, updated_at = $6
		WHERE user_id = $1 AND message_id = $2
		  AND (delivery_status IS DISTINCT FROM $3
		    OR delivery_reason IS DISTINCT FROM $4
		    OR delivery_failed IS DISTINCT FROM $5)
		RETURNING id
	`, userID, messageID, d.Status, d.Reason, d.Failed, timeutil.Now()).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("failed to set delivery status: %w", err)
	}
	// Unchanged, or no such message: tell the two apart for the caller.
	err = db.QueryRow(`SELECT id FROM messages WHERE user_id = $1 AND message_id = $2 LIMIT 1`,
		userID, messageID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to look up message for delivery status: %w", err)
	}
	return id, false, nil
}

// StartMessageDelivery marks the user's message with this Message-ID as
// pending — but only if it has no delivery status yet, so accepting a
// message twice (a retried submission, a send task finishing what an older
// server version accepted) never turns «delivered» or «failed» back into
// «pending». Reports the row and whether it changed; id is 0 when there is
// no such message.
func (db *DB) StartMessageDelivery(userID int64, messageID string) (id int64, changed bool, err error) {
	if messageID == "" {
		return 0, false, nil
	}
	err = db.QueryRow(`
		UPDATE messages
		SET delivery_status = 'pending', delivery_reason = '', delivery_failed = '', updated_at = $3
		WHERE user_id = $1 AND message_id = $2 AND delivery_status = ''
		RETURNING id
	`, userID, messageID, timeutil.Now()).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("failed to start delivery status: %w", err)
	}
	err = db.QueryRow(`SELECT id FROM messages WHERE user_id = $1 AND message_id = $2 LIMIT 1`,
		userID, messageID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to look up message for delivery status: %w", err)
	}
	return id, false, nil
}
