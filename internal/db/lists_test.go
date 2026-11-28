package db

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

func listEnv(t *testing.T) (*DB, int64, *MailingList) {
	t.Helper()
	d, raw, uid := billingEnv(t)
	var domID int64
	name := fmt.Sprintf("zzlist%d.test", time.Now().UnixNano())
	if err := raw.QueryRow(`INSERT INTO domains (domain, user_id, enabled, created_at, verified) VALUES ($1, $2, true, 0, true) RETURNING id`,
		name, uid).Scan(&domID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = raw.Exec(`DELETE FROM domains WHERE id = $1`, domID) })
	l := &MailingList{UserID: uid, DomainID: domID, LocalPart: "news", Title: "Новости", Tier: 2}
	if err := d.CreateMailingList(l); err != nil {
		t.Fatal(err)
	}
	if again := (&MailingList{UserID: uid, DomainID: domID, LocalPart: "news", Title: "x", Tier: 2}); !errors.Is(d.CreateMailingList(again), ErrListAddressTaken) {
		t.Fatal("second list at the same address")
	}
	return d, uid, l
}

func TestListSubscribeConfirmUnsubscribe(t *testing.T) {
	d, _, l := listEnv(t)
	tg := int64(777000 + time.Now().UnixNano()%1000)

	s, _, err := d.Subscribe(l.Code, tg, "A@Example.org", false)
	if err != nil || s.Status != "pending" {
		t.Fatalf("subscribe: %v %+v", err, s)
	}
	if _, _, err := d.Subscribe(l.Code, tg, "a@example.org", false); !errors.Is(err, ErrConfirmPending) {
		t.Fatalf("second confirmation the same day: %v", err)
	}
	ds, err := d.ClaimListDeliveries(10)
	if err != nil || len(ds) != 1 || ds[0].IssueID != 0 || ds[0].Email != "a@example.org" {
		t.Fatalf("confirmation letter: %v %+v", err, ds)
	}
	if err := d.FinishListDelivery(ds[0], nil, false); err != nil {
		t.Fatal(err)
	}
	if err := d.ConfirmSubscriber(s.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Subscribe(l.Code, tg, "a@example.org", false); !errors.Is(err, ErrAlreadySubscribed) {
		t.Fatalf("already: %v", err)
	}
	// Own address: active at once; the tier (2) is now full.
	if s2, _, err := d.Subscribe(l.Code, tg, "me@lunlun.test", true); err != nil || s2.Status != "active" {
		t.Fatalf("own: %v", err)
	}
	if _, _, err := d.Subscribe(l.Code, tg, "c@example.org", false); !errors.Is(err, ErrListFull) {
		t.Fatalf("full: %v", err)
	}
	subs, _ := d.ListSubscriptionsOfTg(tg)
	if len(subs) != 2 {
		t.Fatalf("subscriptions of tg: %+v", subs)
	}
	if err := d.UnsubscribeToken(s.Token); err != nil {
		t.Fatal(err)
	}
	if err := d.UnsubscribeTg(tg, subs[1].ID); err != nil {
		t.Fatal(err)
	}
	if subs, _ := d.ListSubscriptionsOfTg(tg); len(subs) != 0 {
		t.Fatalf("after unsubscribing: %+v", subs)
	}
}

func TestListPostOwnerOnlyAndBounceStop(t *testing.T) {
	d, uid, l := listEnv(t)
	if _, err := d.Exec(`UPDATE mailing_lists SET tier = 100 WHERE id = $1`, l.ID); err != nil {
		t.Fatal(err)
	}
	tg := int64(888000 + time.Now().UnixNano()%1000)
	for i := 0; i < ListBounceStop+1; i++ {
		if _, _, err := d.Subscribe(l.Code, tg, fmt.Sprintf("s%d@example.org", i), true); err != nil {
			t.Fatal(err)
		}
	}
	raw := []byte("From: me@x\r\nSubject: Hi\r\n\r\nbody\r\n")
	if _, err := d.PostToList("nobody@nowhere.test", uid, 1, "Hi", raw); !errors.Is(err, ErrListNotFound) {
		t.Fatalf("not a list: %v", err)
	}
	if _, err := d.PostToList(l.Address(), uid+100000, 1, "Hi", raw); !errors.Is(err, ErrListNotOwner) {
		t.Fatalf("stranger posts: %v", err)
	}
	if _, err := d.PostToList(l.Address(), uid, 1, "Hi", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PostToList(l.Address(), uid, 1, "Hi", raw); err != nil { // retry: nothing new
		t.Fatal(err)
	}
	ds, err := d.ClaimListDeliveries(100)
	if err != nil || len(ds) != ListBounceStop+1 {
		t.Fatalf("issue letters: %v %d", err, len(ds))
	}
	if again, _ := d.ClaimListDeliveries(100); len(again) != 0 {
		t.Fatalf("claimed twice: %d", len(again))
	}
	for i := 0; i < ListBounceStop; i++ {
		if err := d.FinishListDelivery(ds[i], errors.New("550 no such user"), true); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := d.MailingListByID(l.ID)
	if got.StoppedUntil <= timeutil.Now() || got.Active != 1 {
		t.Fatalf("after %d bounces: stopped %d, active %d", ListBounceStop, got.StoppedUntil, got.Active)
	}
	if _, err := d.PostToList(l.Address(), uid, 2, "Hi", raw); !errors.Is(err, ErrListOff) {
		t.Fatalf("post to a stopped list: %v", err)
	}
	var notices int
	_ = d.QueryRow(`SELECT COUNT(*) FROM bot_notices n JOIN telegram_links t ON t.tg_user_id = n.tg_user_id
		WHERE t.user_id = $1 AND n.kind = 'list_stopped'`, uid).Scan(&notices)
	if notices != 1 {
		t.Fatalf("list_stopped notices = %d", notices)
	}
}

func TestListSuspendedWithSubscription(t *testing.T) {
	d, uid, l := listEnv(t)
	if _, err := d.AddTopUp(uid, 10, fmt.Sprintf("l%d", uid)); err != nil {
		t.Fatal(err)
	}
	sub, _, err := d.EnableFeature(uid, FeatureMailingList, l.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE subscriptions SET paid_until = $2 WHERE id = $1`, sub.ID, timeutil.Now()-GracePeriod-1); err != nil {
		t.Fatal(err)
	}
	if _, s, err := d.RenewSubscriptions(); err != nil || s != 1 {
		t.Fatalf("suspend: %d %v", s, err)
	}
	if got, _ := d.MailingListByID(l.ID); got.Enabled {
		t.Fatal("unpaid list still on")
	}
	if _, _, err := d.Subscribe(l.Code, 1, "x@example.org", false); !errors.Is(err, ErrListOff) {
		t.Fatalf("subscribe to an unpaid list: %v", err)
	}
}
