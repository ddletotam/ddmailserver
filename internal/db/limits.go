package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strconv"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/models"
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
	// ErrRecipientLimit is a kind of ErrSendLimit: too many different
	// recipients today. Callers that only know ErrSendLimit still refuse it.
	ErrRecipientLimit = fmt.Errorf("daily recipients limit reached: %w", ErrSendLimit)
	// ErrSendStopped is a kind of ErrSendLimit: sending is stopped for the day
	// because too much of the account's mail went to addresses that don't exist.
	ErrSendStopped = fmt.Errorf("sending stopped after undeliverable mail: %w", ErrSendLimit)
)

// Free tier of the sign-up service (docs/lunlun-bot-plan.md, decided
// 2026-11-27): an account signed up through the Telegram bot. Spammers write
// to many different addresses, people to a few — so different recipients per
// day are limited along with messages. A fresh account gets half of both for
// its first week.
const (
	FreeSendPerDay       = 50
	FreeRecipientsPerDay = 20
	FreeWarmupDays       = 7
	FreeQuotaMB          = 1024
	// FreeBounceStop: this many messages in 24 hours that came back «no such
	// mailbox / no such domain» stop a free account's sending — the mark of
	// mail sent to a bought or harvested list.
	FreeBounceStop = 5
)

// freeTierSince returns when userID signed up through the bot, or 0 when the
// account is not on the free tier (made by an administrator, a mailbox, ...).
func freeTierSince(q querier, userID int64) (int64, error) {
	var created sql.NullInt64
	err := q.QueryRow(`SELECT u.created_at FROM telegram_links l JOIN users u ON u.id = l.user_id
		WHERE l.user_id = $1`, userID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("free tier of user %d: %w", userID, err)
	}
	if !created.Valid || created.Int64 == 0 {
		return 1, nil // on the free tier, sign-up time unknown: no warm-up
	}
	return created.Int64, nil
}

// freeTierLimits: messages and different recipients a free account may send
// per 24 hours, halved during the warm-up week.
func freeTierLimits(since, now int64) (msgs, rcpts int) {
	msgs, rcpts = FreeSendPerDay, FreeRecipientsPerDay
	if since > 1 && now-since < FreeWarmupDays*24*3600*1000 {
		msgs, rcpts = msgs/2, rcpts/2
	}
	return msgs, rcpts
}

// recipientAddrs extracts lower-cased addresses from To/Cc/Bcc header values.
func recipientAddrs(fields ...string) []string {
	var out []string
	for _, f := range fields {
		if strings.TrimSpace(f) == "" {
			continue
		}
		if list, err := mail.ParseAddressList(f); err == nil {
			for _, a := range list {
				out = append(out, strings.ToLower(a.Address))
			}
			continue
		}
		for _, part := range strings.Split(f, ",") {
			part = strings.TrimSpace(part)
			if i := strings.LastIndexByte(part, '<'); i >= 0 {
				part = strings.TrimSuffix(part[i+1:], ">")
			}
			if part != "" {
				out = append(out, strings.ToLower(part))
			}
		}
	}
	return out
}

// checkFreeTierSend: a free account may queue msg only within its daily
// limits of messages and different recipients.
func checkFreeTierSend(q querier, msg *models.OutboxMessage) error {
	since, err := freeTierSince(q, msg.UserID)
	if err != nil || since == 0 {
		return err
	}
	if paid, err := hasEntitlement(q, msg.UserID, FeatureSendLimits); err != nil || paid {
		return err
	}
	now := timeutil.Now()
	maxMsgs, maxRcpts := freeTierLimits(since, now)
	rows, err := q.Query(`SELECT to_addr, COALESCE(cc, ''), COALESCE(bcc, '') FROM outbox_messages
		WHERE user_id = $1 AND created_at > $2`, msg.UserID, now-24*3600*1000)
	if err != nil {
		return fmt.Errorf("today's sending: %w", err)
	}
	defer rows.Close()
	sent := 0
	seen := map[string]bool{}
	for rows.Next() {
		var to, cc, bcc string
		if err := rows.Scan(&to, &cc, &bcc); err != nil {
			return fmt.Errorf("today's sending: %w", err)
		}
		sent++
		for _, a := range recipientAddrs(to, cc, bcc) {
			seen[a] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("today's sending: %w", err)
	}
	if sent >= maxMsgs {
		return ErrSendLimit
	}
	var bounced int
	if err := q.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id = $1 AND delivery_status = 'failed'
		AND delivery_reason IN ('mailbox_unavailable', 'domain_not_found') AND created_at > $2`,
		msg.UserID, now-24*3600*1000).Scan(&bounced); err != nil {
		return fmt.Errorf("undeliverable mail: %w", err)
	}
	if bounced >= FreeBounceStop {
		if err := notifyUserOnce(q, msg.UserID, "send_stopped", fmt.Sprintf(
			"Отправка писем с вашего адреса остановлена на сутки: %d писем за последние 24 часа вернулись — таких адресов не существует. "+
				"Так бывает при рассылке по чужим или устаревшим спискам адресов. Проверьте адреса получателей.", bounced),
			24*3600*1000); err != nil {
			log.Printf("free tier: %v", err)
		}
		return ErrSendStopped
	}
	for _, a := range recipientAddrs(msg.To, msg.Cc, msg.Bcc) {
		seen[a] = true
	}
	if len(seen) > maxRcpts {
		return ErrRecipientLimit
	}
	return nil
}

// FreeTierOverQuota: a free account's storage has reached the free quota.
func (db *DB) FreeTierOverQuota(userID int64) (bool, error) {
	since, err := freeTierSince(db, userID)
	if err != nil || since == 0 {
		return false, err
	}
	quotaMB := int64(FreeQuotaMB)
	if paid, err := hasEntitlement(db, userID, FeatureExtraStorage); err != nil {
		return false, err
	} else if paid {
		quotaMB += ExtraStorageMB
	}
	var used int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM messages WHERE user_id = $1`, userID).Scan(&used); err != nil {
		return false, fmt.Errorf("storage of user %d: %w", userID, err)
	}
	return used >= quotaMB*1024*1024, nil
}

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
	// The free tier connects other mailboxes on the server only with the
	// paid feature; in the DDMail app it can connect them for free.
	if since, err := freeTierSince(q, userID); err != nil {
		return err
	} else if since != 0 {
		paid, err := hasEntitlement(q, userID, FeatureExternalAccounts)
		if err != nil {
			return err
		}
		if !paid {
			return ErrAccountLimit
		}
	}
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
// last hour, or — on the free tier — is past its daily limits.
func checkSendLimit(q querier, msg *models.OutboxMessage) error {
	if err := checkFreeTierSend(q, msg); err != nil {
		return err
	}
	userID := msg.UserID
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
