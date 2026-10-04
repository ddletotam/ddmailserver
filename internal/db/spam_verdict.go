package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// MessageSpamState is the filtering decision stored on a message: whether it
// is hidden as spam and which user rule (blacklist, or partial allow) decided.
// spam_score / spam_status / spam_reasons — the analyzer's part — are on
// models.Message.
type MessageSpamState struct {
	IsSpam bool
	RuleID *int64
}

// GetMessageSpamState returns the stored spam decision of a message.
func (db *DB) GetMessageSpamState(messageID int64) (*MessageSpamState, error) {
	var st MessageSpamState
	var ruleID sql.NullInt64
	err := db.QueryRow(
		`SELECT COALESCE(is_spam, false), spam_rule_id FROM messages WHERE id = $1`, messageID,
	).Scan(&st.IsSpam, &ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get spam state of message %d: %w", messageID, err)
	}
	if ruleID.Valid {
		id := ruleID.Int64
		st.RuleID = &id
	}
	return &st, nil
}
