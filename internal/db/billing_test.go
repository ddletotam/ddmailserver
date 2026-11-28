package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/ddletotam/ddmailserver/migrations"
)

func billingEnv(t *testing.T) (*DB, *sql.DB, int64) {
	t.Helper()
	dsn := os.Getenv("MAILSERVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set MAILSERVER_TEST_DSN to a Postgres DSN to run DB integration tests")
	}
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	d := &DB{DB: raw}
	if err := d.RunMigrations(context.Background(), migrations.FS()); err != nil {
		t.Fatal(err)
	}
	var uid int64
	if err := raw.QueryRow(`INSERT INTO users (username, password_hash, recovery_key_hash, is_admin, created_at, updated_at)
		VALUES ($1, 'x', '', false, 0, 0) RETURNING id`, fmt.Sprintf("zzbill%d", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	tg := time.Now().UnixNano() % 1_000_000_000_000
	if _, err := raw.Exec(`INSERT INTO telegram_links (tg_user_id, user_id, terms_accepted_at, created_at) VALUES ($1, $2, 0, 0)`, tg, uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = raw.Exec(`DELETE FROM telegram_links WHERE user_id = $1`, uid)
		_, _ = raw.Exec(`DELETE FROM users WHERE id = $1`, uid)
	})
	return d, raw, uid
}

func TestRenewalGraceAndSuspend(t *testing.T) {
	d, raw, uid := billingEnv(t)
	if _, err := d.AddTopUp(uid, 60, fmt.Sprintf("c%d", uid)); err != nil {
		t.Fatal(err)
	}
	sub, bal, err := d.EnableFeature(uid, FeatureSendLimits, 0, 50)
	if err != nil || bal != 10 {
		t.Fatalf("enable: %v, balance %d", err, bal)
	}
	// The month is over, 10 Stars left: grace, a low-balance notice, still working.
	if _, err := raw.Exec(`UPDATE subscriptions SET paid_until = $2 WHERE id = $1`, sub.ID, timeutil.Now()-time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if r, s, err := d.RenewSubscriptions(); err != nil || r != 0 || s != 0 {
		t.Fatalf("renew in grace: %d %d %v", r, s, err)
	}
	if ok, _ := hasEntitlement(d, uid, FeatureSendLimits); !ok {
		t.Fatal("not working during grace")
	}
	var notices int
	_ = raw.QueryRow(`SELECT COUNT(*) FROM bot_notices n JOIN telegram_links l ON l.tg_user_id = n.tg_user_id
		WHERE l.user_id = $1 AND n.kind = 'low_balance'`, uid).Scan(&notices)
	if notices != 1 {
		t.Fatalf("low_balance notices = %d", notices)
	}
	// Topped up: renewed for a month.
	if _, err := d.AddTopUp(uid, 100, fmt.Sprintf("c%d-2", uid)); err != nil {
		t.Fatal(err)
	}
	if r, _, err := d.RenewSubscriptions(); err != nil || r != 1 {
		t.Fatalf("renew after top-up: %d %v", r, err)
	}
	if b, _ := d.Balance(uid); b != 60 {
		t.Fatalf("balance after renewal = %d, want 60", b)
	}
	// Grace over with no money: suspended.
	if _, err := raw.Exec(`UPDATE subscriptions SET paid_until = $2 WHERE id = $1`, sub.ID, timeutil.Now()-GracePeriod-time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO balance_ops (user_id, kind, amount, created_at) VALUES ($1, 'charge', -60, 0)`, uid); err != nil {
		t.Fatal(err)
	}
	if _, s, err := d.RenewSubscriptions(); err != nil || s != 1 {
		t.Fatalf("suspend: %d %v", s, err)
	}
	if ok, _ := hasEntitlement(d, uid, FeatureSendLimits); ok {
		t.Fatal("working after suspension")
	}
}

func TestRefundNewestUnspent(t *testing.T) {
	d, _, uid := billingEnv(t)
	for i, amt := range []int{100, 100} {
		if _, err := d.AddTopUp(uid, amt, fmt.Sprintf("r%d-%d", uid, i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := d.EnableFeature(uid, FeatureExtraStorage, 0, 50); err != nil {
		t.Fatal(err)
	}
	list, err := d.RefundableTopUps(uid)
	if err != nil || len(list) != 1 || list[0].ChargeID != fmt.Sprintf("r%d-1", uid) {
		t.Fatalf("refundable = %+v, %v", list, err)
	}
	if _, err := d.ReserveRefund(uid, fmt.Sprintf("r%d-0", uid)); !errors.Is(err, ErrNotRefundable) {
		t.Fatalf("refund of the spent one: %v", err)
	}
	if _, err := d.ReserveRefund(uid, list[0].ChargeID); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.Balance(uid); b != 50 {
		t.Fatalf("balance after refund = %d", b)
	}
	if err := d.CancelRefund(uid, list[0].ChargeID); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.Balance(uid); b != 150 {
		t.Fatalf("balance after cancelled refund = %d", b)
	}
	if _, _, err := d.EnableFeature(uid, FeatureSendLimits, 0, 1000); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("too expensive: %v", err)
	}
}

func TestUnpaidMailboxStops(t *testing.T) {
	d, raw, uid := billingEnv(t)
	var domID, mbID int64
	if err := raw.QueryRow(`INSERT INTO domains (domain, user_id, enabled, created_at, verified, verify_token)
		VALUES ($1, $2, true, 0, true, 'x') RETURNING id`, fmt.Sprintf("stop%d.test", uid), uid).Scan(&domID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = raw.Exec(`DELETE FROM mailboxes WHERE domain_id = $1`, domID)
		_, _ = raw.Exec(`DELETE FROM domains WHERE id = $1`, domID)
	})
	if err := raw.QueryRow(`INSERT INTO mailboxes (user_id, domain_id, local_part, enabled, created_at) VALUES ($1, $2, 'a', true, 0) RETURNING id`,
		uid, domID).Scan(&mbID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddTopUp(uid, 25, fmt.Sprintf("m%d", uid)); err != nil {
		t.Fatal(err)
	}
	sub, _, err := d.EnableFeature(uid, FeatureMailbox, mbID, 25)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE subscriptions SET paid_until = $2 WHERE id = $1`, sub.ID, timeutil.Now()-GracePeriod-1000); err != nil {
		t.Fatal(err)
	}
	if _, s, err := d.RenewSubscriptions(); err != nil || s != 1 {
		t.Fatalf("suspend: %d %v", s, err)
	}
	var on bool
	_ = raw.QueryRow(`SELECT enabled FROM mailboxes WHERE id = $1`, mbID).Scan(&on)
	if on {
		t.Fatal("an unpaid mailbox still takes mail")
	}
	// Paid again: back on.
	if _, err := d.AddTopUp(uid, 25, fmt.Sprintf("m%d-2", uid)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.EnableFeature(uid, FeatureMailbox, mbID, 25); err != nil {
		t.Fatal(err)
	}
	_ = raw.QueryRow(`SELECT enabled FROM mailboxes WHERE id = $1`, mbID).Scan(&on)
	if !on {
		t.Fatal("a paid-again mailbox is still off")
	}
}
