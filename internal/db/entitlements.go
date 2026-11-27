package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Paid features (docs/lunlun-bot-plan.md): each one a monthly subscription.
const (
	FeatureExternalAccounts = "external_accounts" // connect other mailboxes on the server
	FeatureExtraStorage     = "extra_storage"     // +10 GB to the mailbox
	FeatureSendLimits       = "send_limits"       // sending without the free-tier limits
)

// ExtraStorageMB is what FeatureExtraStorage adds to the free quota.
const ExtraStorageMB = 10 * 1024

// SubscriptionPeriod is one paid month, as Telegram bills a subscription.
const SubscriptionPeriod = int64(30 * 24 * 3600 * 1000)

// hasEntitlement reports whether userID has the feature paid up to now.
func hasEntitlement(q querier, userID int64, feature string) (bool, error) {
	var ok bool
	err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM entitlements WHERE user_id = $1 AND feature = $2 AND until > $3)`,
		userID, feature, timeutil.Now()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("entitlement %s of user %d: %w", feature, userID, err)
	}
	return ok, nil
}

// Entitlements returns until when each feature of userID is paid (ms; past
// dates included — the bot shows «expired»).
func (db *DB) Entitlements(userID int64) (map[string]int64, error) {
	rows, err := db.Query(`SELECT feature, until FROM entitlements WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("entitlements of user %d: %w", userID, err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var f string
		var until int64
		if err := rows.Scan(&f, &until); err != nil {
			return nil, err
		}
		out[f] = until
	}
	return out, rows.Err()
}

// Payment is one Telegram Stars charge for a feature.
type Payment struct {
	UserID, TgUserID int64
	Feature          string
	Amount           int
	ChargeID         string
	Recurring        bool
	// ExpiresAt — when Telegram says the subscription period ends (ms); 0 —
	// unknown, a month from now (or from the current end, if still paid).
	ExpiresAt int64
}

// RecordPayment stores a charge and extends the feature; a charge seen before
// (the bot retried) changes nothing. Returns the feature's new end.
func (db *DB) RecordPayment(p Payment) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	now := timeutil.Now()
	var until int64
	err = tx.QueryRow(`SELECT until FROM entitlements WHERE user_id = $1 AND feature = $2 FOR UPDATE`, p.UserID, p.Feature).Scan(&until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("entitlement: %w", err)
	}
	var known bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM payments WHERE telegram_charge_id = $1)`, p.ChargeID).Scan(&known); err != nil {
		return 0, fmt.Errorf("payment lookup: %w", err)
	}
	if known {
		return until, nil
	}
	newUntil := p.ExpiresAt
	if newUntil <= now {
		from := now
		if until > now {
			from = until
		}
		newUntil = from + SubscriptionPeriod
	}
	if _, err := tx.Exec(`INSERT INTO entitlements (user_id, feature, until, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, feature) DO UPDATE SET until = GREATEST(entitlements.until, EXCLUDED.until), updated_at = EXCLUDED.updated_at`,
		p.UserID, p.Feature, newUntil, now); err != nil {
		return 0, fmt.Errorf("extend %s: %w", p.Feature, err)
	}
	if _, err := tx.Exec(`INSERT INTO payments (user_id, tg_user_id, feature, amount, telegram_charge_id, recurring, paid_until, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.UserID, p.TgUserID, p.Feature, p.Amount, p.ChargeID, p.Recurring, newUntil, now); err != nil {
		return 0, fmt.Errorf("store payment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	if newUntil < until {
		newUntil = until
	}
	return newUntil, nil
}

// Outbound classes: which source IP a user's direct mail leaves from.
const (
	OutboundFree = "free" // signed up through the bot, nothing paid
	OutboundPaid = "paid" // signed up through the bot, some feature paid
)

// OutboundClass returns the class of userID's outgoing mail; "" — the
// server's own users (the default route).
func (db *DB) OutboundClass(userID int64) (string, error) {
	since, err := freeTierSince(db, userID)
	if err != nil || since == 0 {
		return "", err
	}
	var paid bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM entitlements WHERE user_id = $1 AND until > $2)`,
		userID, timeutil.Now()).Scan(&paid); err != nil {
		return "", fmt.Errorf("outbound class of user %d: %w", userID, err)
	}
	if paid {
		return OutboundPaid, nil
	}
	return OutboundFree, nil
}
