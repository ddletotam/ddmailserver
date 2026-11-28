package db

import (
	"fmt"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Paid features of the sign-up service (docs/lunlun-bot-plan.md): each a
// subscription renewed from the user's balance every 30 days (billing.go).
const (
	FeatureExternalAccounts = "external_accounts" // connect other mailboxes on the server
	FeatureExtraStorage     = "extra_storage"     // +10 GB to the user's space
	FeatureSendLimits       = "send_limits"       // raised daily sending limits
	FeatureMailbox          = "mailbox"           // one more mailbox (per mailbox)
)

// ExtraStorageMB is what FeatureExtraStorage adds to the free quota.
const ExtraStorageMB = 10 * 1024

// SubscriptionPeriod is one paid period; GracePeriod — how long an unpaid
// renewal keeps working before the feature is switched off.
const (
	SubscriptionPeriod = int64(30 * 24 * 3600 * 1000)
	GracePeriod        = int64(7 * 24 * 3600 * 1000)
)

// effective is the SQL condition (on alias s) of a subscription that works
// at the time in the positional parameter now: active and paid or within the
// grace period, or cancelled but still paid.
func effective(now string) string {
	return fmt.Sprintf(`((s.status = 'active' AND s.paid_until + %d > %s) OR (s.status = 'cancelled' AND s.paid_until > %s))`,
		GracePeriod, now, now)
}

// hasEntitlement reports whether userID has the feature working now.
func hasEntitlement(q querier, userID int64, feature string) (bool, error) {
	var ok bool
	err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = $1 AND s.feature = $2 AND `+
		effective("$3")+`)`, userID, feature, timeutil.Now()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("feature %s of user %d: %w", feature, userID, err)
	}
	return ok, nil
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
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = $1 AND `+
		effective("$2")+`)`, userID, timeutil.Now()).Scan(&paid); err != nil {
		return "", fmt.Errorf("outbound class of user %d: %w", userID, err)
	}
	if paid {
		return OutboundPaid, nil
	}
	return OutboundFree, nil
}
