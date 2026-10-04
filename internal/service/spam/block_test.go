package spam

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/service/messages"
)

// Спам-рассылка приходит с From=спамер, To=жертва (пользователь в скрытой
// копии). Клиент склеивает обоих в «участников» диалога и не знает, кто
// отправитель — поэтому блокировать нужно по реальному From из строк письма,
// а не по догадке клиента. Домен-scope обязателен против random-логинов.
func TestSenderRules(t *testing.T) {
	// From = rulane (спамер), To = hsmedia — участники диалога оба.
	from := []string{"Mакitа <uptolwh@rulane.life>"}

	addr := SenderRules(from, "", "", "address")
	if len(addr) != 1 || addr[0] != (BlockRule{"address", "uptolwh@rulane.life"}) {
		t.Fatalf("address scope: got %+v, want address=uptolwh@rulane.life", addr)
	}

	dom := SenderRules(from, "", "", "domain")
	if len(dom) != 1 || dom[0] != (BlockRule{"domain", "rulane.life"}) {
		t.Fatalf("domain scope: got %+v, want domain=rulane.life", dom)
	}

	// Fallback (IMAP: no rows) uses the client hint, not the real sender.
	fb := SenderRules(nil, "spammer@bad.tld", "", "domain")
	if len(fb) != 1 || fb[0].Value != "bad.tld" {
		t.Fatalf("fallback: got %+v, want domain=bad.tld", fb)
	}

	// Multiple distinct senders in a group → one rule each, sorted, deduped.
	multi := SenderRules([]string{"A <x@a.tld>", "B <y@b.tld>", "C <z@a.tld>"}, "", "", "domain")
	if len(multi) != 2 || multi[0].Value != "a.tld" || multi[1].Value != "b.tld" {
		t.Fatalf("multi domain: got %+v, want [a.tld b.tld]", multi)
	}

	// Nothing to block → empty (handler turns this into 400).
	if got := SenderRules(nil, "", "", "address"); len(got) != 0 {
		t.Fatalf("empty: got %+v, want none", got)
	}
}

type fakeRules struct {
	senders []string
	created []db.SpamRule
	failAll bool
}

func (f *fakeRules) GetSenderAddrsByIDs(int64, []int64) ([]string, error) { return f.senders, nil }
func (f *fakeRules) CreateSpamRule(r *db.SpamRule) error {
	if f.failAll {
		return errors.New("duplicate key")
	}
	f.created = append(f.created, *r)
	return nil
}

type fakePurger struct {
	got messages.PurgeSelector
	err error
}

func (f *fakePurger) Purge(_ context.Context, _ int64, sel messages.PurgeSelector) (messages.PurgeResult, error) {
	f.got = sel
	if f.err != nil {
		return messages.PurgeResult{}, f.err
	}
	return messages.PurgeResult{Deleted: 4, Queued: 1}, nil
}

func TestBlockAndPurge_DomainScope(t *testing.T) {
	rules := &fakeRules{senders: []string{"S <a@spam.example>", "T <b@spam.example>"}}
	purger := &fakePurger{}
	res, err := NewBlocker(rules, purger).BlockAndPurge(context.Background(), 1, BlockRequest{
		MessageIDs: []int64{10, 11}, Scope: " Domain ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.created) != 1 || rules.created[0].RuleType != "domain" || rules.created[0].RuleValue != "spam.example" ||
		rules.created[0].Action != "spam" || rules.created[0].UserID != 1 {
		t.Fatalf("rules created: %+v", rules.created)
	}
	want := messages.PurgeSelector{IDs: []int64{10, 11}, SenderDomains: []string{"spam.example"}}
	if !reflect.DeepEqual(purger.got, want) {
		t.Fatalf("purged %+v, want %+v", purger.got, want)
	}
	if res.Deleted != 4 || res.QueuedRemote != 1 || res.Rules[0].Value != "spam.example" {
		t.Fatalf("result %+v", res)
	}
}

func TestBlockAndPurge_AddressScopeAndDuplicateRule(t *testing.T) {
	rules := &fakeRules{senders: []string{"S <a@spam.example>"}, failAll: true}
	purger := &fakePurger{}
	if _, err := NewBlocker(rules, purger).BlockAndPurge(context.Background(), 1, BlockRequest{Scope: "whatever"}); err != nil {
		t.Fatalf("an already existing rule must not fail the purge: %v", err)
	}
	if !reflect.DeepEqual(purger.got.SenderAddresses, []string{"a@spam.example"}) || purger.got.SenderDomains != nil {
		t.Fatalf("purged %+v", purger.got)
	}
}

func TestBlockAndPurge_NoSender(t *testing.T) {
	purger := &fakePurger{}
	_, err := NewBlocker(&fakeRules{}, purger).BlockAndPurge(context.Background(), 1, BlockRequest{})
	if !errors.Is(err, ErrNoSender) {
		t.Fatalf("err = %v, want ErrNoSender", err)
	}
	if purger.got.IDs != nil || purger.got.SenderAddresses != nil {
		t.Fatal("purged without a sender")
	}
}

func TestBlockAndPurge_PurgeFailureIsReported(t *testing.T) {
	purger := &fakePurger{err: errors.New("db down")}
	_, err := NewBlocker(&fakeRules{senders: []string{"a@spam.example"}}, purger).
		BlockAndPurge(context.Background(), 1, BlockRequest{})
	if err == nil {
		t.Fatal("purge failure swallowed")
	}
}
