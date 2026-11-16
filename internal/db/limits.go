package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Server limits (stage 6, docs/server-admin-split-plan.md). All are off by
// default — switching one on, and its number, is the administrator's
// deliberate act. Stored in system_settings; 0 or absent = off.
const (
	SettingLimitMailboxQuotaMB   = "limit_mailbox_quota_mb"  // size of a mailbox with its own account
	SettingLimitSendPerHour      = "limit_send_per_hour"     // messages queued per sender per hour
	SettingLimitExternalAccounts = "limit_external_accounts" // connected accounts per user
)

// LimitSettings lists the limit keys the admin page edits.
var LimitSettings = []string{SettingLimitMailboxQuotaMB, SettingLimitSendPerHour, SettingLimitExternalAccounts}

var (
	ErrAccountLimit = errors.New("connected accounts limit reached")
	ErrSendLimit    = errors.New("sending limit reached")
)

func limitValue(q querier, key string) (int64, error) {
	var v string
	err := q.QueryRow(`SELECT value FROM system_settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("limit %s: %w", key, err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, nil
	}
	return n, nil
}

// Limit returns a limit's value; 0 — off.
func (db *DB) Limit(key string) (int64, error) { return limitValue(db, key) }

// checkAccountLimit: one more connected account for userID would exceed the
// limit.
func checkAccountLimit(q querier, userID int64) error {
	limit, err := limitValue(q, SettingLimitExternalAccounts)
	if err != nil || limit == 0 {
		return err
	}
	var n int64
	if err := q.QueryRow(`SELECT COUNT(*) FROM accounts WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return fmt.Errorf("count accounts: %w", err)
	}
	if n >= limit {
		return ErrAccountLimit
	}
	return nil
}

// checkSendLimit: the sender has queued the limit's worth of messages in the
// last hour.
func checkSendLimit(q querier, userID int64) error {
	limit, err := limitValue(q, SettingLimitSendPerHour)
	if err != nil || limit == 0 {
		return err
	}
	var n int64
	if err := q.QueryRow(`SELECT COUNT(*) FROM outbox_messages WHERE user_id = $1 AND created_at > $2`,
		userID, timeutil.Now()-3600*1000).Scan(&n); err != nil {
		return fmt.Errorf("count sent: %w", err)
	}
	if n >= limit {
		return ErrSendLimit
	}
	return nil
}

// MailboxOverQuota: the storage of a mailbox's own account has reached the
// mailbox quota. Old-style mailboxes (mail stored with the owner) are not
// counted here — the owner's storage holds more than that mailbox.
func (db *DB) MailboxOverQuota(principalID int64) (bool, error) {
	limit, err := limitValue(db, SettingLimitMailboxQuotaMB)
	if err != nil || limit == 0 || principalID == 0 {
		return false, err
	}
	var used int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM messages WHERE user_id = $1`, principalID).Scan(&used); err != nil {
		return false, fmt.Errorf("mailbox usage: %w", err)
	}
	return used >= limit*1024*1024, nil
}
