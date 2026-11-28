package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Balance and subscriptions (docs/lunlun-bot-plan.md, решения 2026-11-28).
// The balance is the sum of balance_ops; every change of it locks the user's
// row first, so two charges can't both spend the same Stars.

var (
	// ErrInsufficientFunds: the balance does not cover the price.
	ErrInsufficientFunds = errors.New("insufficient balance")
	// ErrNotRefundable: the top-up was already spent (in part) or refunded.
	ErrNotRefundable = errors.New("top-up is not refundable")
)

// Subscription is one paid feature of a user (one mailbox for FeatureMailbox).
type Subscription struct {
	ID        int64  `json:"id"`
	Feature   string `json:"feature"`
	RefID     int64  `json:"ref_id,omitempty"`
	Price     int    `json:"price"`
	PaidUntil int64  `json:"paid_until"`
	Status    string `json:"status"`
}

// Working: the feature works at now (ms) — paid, within grace, or cancelled
// but still paid.
func (s Subscription) Working(now int64) bool {
	switch s.Status {
	case "active":
		return s.PaidUntil+GracePeriod > now
	case "cancelled":
		return s.PaidUntil > now
	}
	return false
}

// TopUp is one Stars payment onto the balance.
type TopUp struct {
	ID       int64  `json:"id"`
	Amount   int    `json:"amount"`
	ChargeID string `json:"charge_id"`
}

func lockUser(q querier, userID int64) error {
	var id int64
	if err := q.QueryRow(`SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&id); err != nil {
		return fmt.Errorf("lock user %d: %w", userID, err)
	}
	return nil
}

func balanceOf(q querier, userID int64) (int, error) {
	var b int
	if err := q.QueryRow(`SELECT COALESCE(SUM(amount), 0) FROM balance_ops WHERE user_id = $1`, userID).Scan(&b); err != nil {
		return 0, fmt.Errorf("balance of user %d: %w", userID, err)
	}
	return b, nil
}

// Balance returns the user's balance in Stars.
func (db *DB) Balance(userID int64) (int, error) { return balanceOf(db, userID) }

// AddTopUp puts a Stars payment on the balance; a charge seen before (the bot
// retried) changes nothing. Returns the new balance.
func (db *DB) AddTopUp(userID int64, amount int, chargeID string) (int, error) {
	if amount <= 0 || chargeID == "" {
		return 0, fmt.Errorf("top-up: bad amount %d or charge %q", amount, chargeID)
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := lockUser(tx, userID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO balance_ops (user_id, kind, amount, telegram_charge_id, created_at)
		VALUES ($1, 'topup', $2, $3, $4) ON CONFLICT (telegram_charge_id) DO NOTHING`,
		userID, amount, chargeID, timeutil.Now()); err != nil {
		return 0, fmt.Errorf("top-up: %w", err)
	}
	b, err := balanceOf(tx, userID)
	if err != nil {
		return 0, err
	}
	return b, tx.Commit()
}

// Subscriptions lists the user's subscriptions, newest first.
func (db *DB) Subscriptions(userID int64) ([]Subscription, error) {
	rows, err := db.Query(`SELECT id, feature, COALESCE(ref_id, 0), price, paid_until, status
		FROM subscriptions WHERE user_id = $1 ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("subscriptions of user %d: %w", userID, err)
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.ID, &s.Feature, &s.RefID, &s.Price, &s.PaidUntil, &s.Status); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// EnableFeature subscribes the user to a feature (refID — the mailbox, for
// per-mailbox ones) and charges the first month at price. A cancelled but
// still paid subscription of the same feature is simply resumed, without a
// charge; a working one is returned as is.
func (db *DB) EnableFeature(userID int64, feature string, refID int64, price int) (*Subscription, int, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := lockUser(tx, userID); err != nil {
		return nil, 0, err
	}
	now := timeutil.Now()
	var ref any
	if refID != 0 {
		ref = refID
	}
	var s Subscription
	err = tx.QueryRow(`SELECT id, feature, COALESCE(ref_id, 0), price, paid_until, status FROM subscriptions
		WHERE user_id = $1 AND feature = $2 AND ref_id IS NOT DISTINCT FROM $3 ORDER BY id DESC LIMIT 1`,
		userID, feature, ref).Scan(&s.ID, &s.Feature, &s.RefID, &s.Price, &s.PaidUntil, &s.Status)
	switch {
	case err == nil && s.Status == "active" && s.PaidUntil > now:
		b, err := balanceOf(tx, userID)
		return &s, b, err
	case err == nil && s.Status == "cancelled" && s.PaidUntil > now:
		if _, err := tx.Exec(`UPDATE subscriptions SET status = 'active', updated_at = $2 WHERE id = $1`, s.ID, now); err != nil {
			return nil, 0, err
		}
		s.Status = "active"
		b, err := balanceOf(tx, userID)
		if err != nil {
			return nil, 0, err
		}
		return &s, b, tx.Commit()
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, 0, fmt.Errorf("subscription lookup: %w", err)
	}

	b, err := balanceOf(tx, userID)
	if err != nil {
		return nil, 0, err
	}
	if b < price {
		return nil, b, ErrInsufficientFunds
	}
	s = Subscription{Feature: feature, RefID: refID, Price: price, PaidUntil: now + SubscriptionPeriod, Status: "active"}
	if err := tx.QueryRow(`INSERT INTO subscriptions (user_id, feature, ref_id, price, paid_until, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'active', $6, $6) RETURNING id`,
		userID, feature, ref, price, s.PaidUntil, now).Scan(&s.ID); err != nil {
		return nil, 0, fmt.Errorf("subscribe %s: %w", feature, err)
	}
	if _, err := tx.Exec(`INSERT INTO balance_ops (user_id, kind, amount, subscription_id, note, created_at)
		VALUES ($1, 'charge', $2, $3, $4, $5)`, userID, -price, s.ID, feature, now); err != nil {
		return nil, 0, fmt.Errorf("charge %s: %w", feature, err)
	}
	return &s, b - price, tx.Commit()
}

// CancelSubscription stops renewing: the feature works until paid_until.
func (db *DB) CancelSubscription(userID, subID int64) error {
	res, err := db.Exec(`UPDATE subscriptions SET status = 'cancelled', updated_at = $3
		WHERE id = $1 AND user_id = $2 AND status = 'active'`, subID, userID, timeutil.Now())
	if err != nil {
		return fmt.Errorf("cancel subscription %d: %w", subID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Renewal notices: kinds of bot_notices, at most one a day each.
const (
	noticeLowBalance = "low_balance"
	noticeSuspended  = "suspended"
)

// RenewSubscriptions charges every active subscription whose month is over.
// Not enough Stars: the subscription keeps working through the grace period
// (the user is told once a day), then it is suspended. Subscriptions ending
// within 3 days with a balance that won't cover them are warned of too.
// Returns how many were renewed and suspended.
func (db *DB) RenewSubscriptions() (renewed, suspended int, err error) {
	now := timeutil.Now()
	rows, err := db.Query(`SELECT DISTINCT user_id FROM subscriptions WHERE status = 'active' AND paid_until <= $1`,
		now+3*24*3600*1000)
	if err != nil {
		return 0, 0, fmt.Errorf("due subscriptions: %w", err)
	}
	var users []int64
	for rows.Next() {
		var u int64
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return 0, 0, err
		}
		users = append(users, u)
	}
	rows.Close()
	for _, u := range users {
		r, s, err := db.renewUser(u, now)
		if err != nil {
			log.Printf("billing: user %d: %v", u, err)
			continue
		}
		renewed += r
		suspended += s
	}
	return renewed, suspended, nil
}

func (db *DB) renewUser(userID, now int64) (renewed, suspended int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := lockUser(tx, userID); err != nil {
		return 0, 0, err
	}
	b, err := balanceOf(tx, userID)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.Query(`SELECT id, feature, price, paid_until FROM subscriptions
		WHERE user_id = $1 AND status = 'active' AND paid_until <= $2 ORDER BY paid_until, id`, userID, now+3*24*3600*1000)
	if err != nil {
		return 0, 0, err
	}
	type due struct {
		id        int64
		feature   string
		price     int
		paidUntil int64
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.feature, &d.price, &d.paidUntil); err != nil {
			rows.Close()
			return 0, 0, err
		}
		list = append(list, d)
	}
	rows.Close()

	short := 0 // Stars missing for what comes due within 3 days
	for _, d := range list {
		if d.paidUntil > now { // not due yet — only check it will be covered
			if b < d.price {
				short += d.price - b
			} else {
				b -= d.price // reserve, so the next one is checked against the rest
			}
			continue
		}
		if b >= d.price {
			if _, err := tx.Exec(`UPDATE subscriptions SET paid_until = paid_until + $2, updated_at = $3 WHERE id = $1`,
				d.id, SubscriptionPeriod, now); err != nil {
				return 0, 0, err
			}
			if _, err := tx.Exec(`INSERT INTO balance_ops (user_id, kind, amount, subscription_id, note, created_at)
				VALUES ($1, 'charge', $2, $3, $4, $5)`, userID, -d.price, d.id, d.feature+" renewal", now); err != nil {
				return 0, 0, err
			}
			b -= d.price
			renewed++
			continue
		}
		if d.paidUntil+GracePeriod <= now {
			if _, err := tx.Exec(`UPDATE subscriptions SET status = 'suspended', updated_at = $2 WHERE id = $1`, d.id, now); err != nil {
				return 0, 0, err
			}
			suspended++
			continue
		}
		short += d.price - b
	}
	if suspended > 0 {
		if err := notifyUserOnce(tx, userID, noticeSuspended,
			"Платные возможности отключены: на балансе не хватило звёзд для продления. Данные сохранены — пополните баланс и подключите снова: /balance",
			24*3600*1000); err != nil {
			log.Printf("billing: %v", err)
		}
	}
	if short > 0 {
		if err := notifyUserOnce(tx, userID, noticeLowBalance, fmt.Sprintf(
			"На балансе не хватает %d ⭐ для продления платных возможностей. Без пополнения через 7 дней после окончания оплаченного месяца они отключатся. Пополнить: /balance", short),
			24*3600*1000); err != nil {
			log.Printf("billing: %v", err)
		}
	}
	return renewed, suspended, tx.Commit()
}

// RefundableTopUps: top-ups nothing was spent from — the newest ones whose sum
// fits in the balance (spending takes the oldest Stars first).
func (db *DB) RefundableTopUps(userID int64) ([]TopUp, error) { return refundable(db, userID) }

func refundable(q querier, userID int64) ([]TopUp, error) {
	b, err := balanceOf(q, userID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(`SELECT id, amount, telegram_charge_id FROM balance_ops
		WHERE user_id = $1 AND kind = 'topup' AND NOT refunded ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("top-ups of user %d: %w", userID, err)
	}
	defer rows.Close()
	out := []TopUp{}
	sum := 0
	for rows.Next() {
		var t TopUp
		if err := rows.Scan(&t.ID, &t.Amount, &t.ChargeID); err != nil {
			return nil, err
		}
		if sum+t.Amount > b {
			break
		}
		sum += t.Amount
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReserveRefund takes a refundable top-up off the balance before the bot asks
// Telegram to give the Stars back; CancelRefund puts it back if Telegram
// refused. Returns the top-up.
func (db *DB) ReserveRefund(userID int64, chargeID string) (*TopUp, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := lockUser(tx, userID); err != nil {
		return nil, err
	}
	list, err := refundable(tx, userID)
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		if t.ChargeID != chargeID {
			continue
		}
		if _, err := tx.Exec(`UPDATE balance_ops SET refunded = true WHERE id = $1`, t.ID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO balance_ops (user_id, kind, amount, refund_of, created_at)
			VALUES ($1, 'refund', $2, $3, $4)`, userID, -t.Amount, t.ID, timeutil.Now()); err != nil {
			return nil, err
		}
		return &t, tx.Commit()
	}
	return nil, ErrNotRefundable
}

// CancelRefund undoes ReserveRefund for a refund Telegram did not make.
func (db *DB) CancelRefund(userID int64, chargeID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var topID int64
	if err := tx.QueryRow(`SELECT id FROM balance_ops WHERE user_id = $1 AND telegram_charge_id = $2 AND refunded`,
		userID, chargeID).Scan(&topID); err != nil {
		return fmt.Errorf("refunded top-up %s: %w", chargeID, err)
	}
	if _, err := tx.Exec(`DELETE FROM balance_ops WHERE refund_of = $1`, topID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE balance_ops SET refunded = false WHERE id = $1`, topID); err != nil {
		return err
	}
	return tx.Commit()
}

// BalanceOp is a line of the balance history.
type BalanceOp struct {
	Kind      string `json:"kind"`
	Amount    int    `json:"amount"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
}

// BalanceHistory returns the last limit movements of the balance.
func (db *DB) BalanceHistory(userID int64, limit int) ([]BalanceOp, error) {
	rows, err := db.Query(`SELECT kind, amount, note, created_at FROM balance_ops WHERE user_id = $1 ORDER BY id DESC LIMIT $2`,
		userID, limit)
	if err != nil {
		return nil, fmt.Errorf("balance history of user %d: %w", userID, err)
	}
	defer rows.Close()
	out := []BalanceOp{}
	for rows.Next() {
		var o BalanceOp
		if err := rows.Scan(&o.Kind, &o.Amount, &o.Note, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
