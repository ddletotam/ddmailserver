package push

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
)

type fakeStore struct {
	mu      sync.Mutex
	tokens  map[int64][]models.PushToken
	deleted []string
	sent    []int64
}

func (s *fakeStore) PushTokensForUser(_ context.Context, userID int64) ([]models.PushToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.PushToken(nil), s.tokens[userID]...), nil
}

func (s *fakeStore) DeletePushToken(_ context.Context, kind, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, kind+":"+token)
	for u, list := range s.tokens {
		kept := list[:0]
		for _, t := range list {
			if t.Kind != kind || t.Token != token {
				kept = append(kept, t)
			}
		}
		s.tokens[u] = kept
	}
	return nil
}

func (s *fakeStore) MarkPushTokenSent(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, id)
	return nil
}

func (s *fakeStore) snapshot() (deleted []string, sent []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...), append([]int64(nil), s.sent...)
}

// countingSender считает доставки по пользователям.
type countingSender struct {
	mu    sync.Mutex
	kind  string
	sends map[int64]int
	err   func(models.PushToken) error
}

func (c *countingSender) Kind() string                    { return c.kind }
func (c *countingSender) Accepts(t models.PushToken) bool { return t.Kind == c.kind }
func (c *countingSender) Send(_ context.Context, t models.PushToken) error {
	c.mu.Lock()
	c.sends[t.UserID]++
	c.mu.Unlock()
	if c.err != nil {
		return c.err(t)
	}
	return nil
}

func (c *countingSender) count(user int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sends[user]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIsNewMail(t *testing.T) {
	for _, c := range []struct {
		name string
		ev   notify.Event
		want bool
	}{
		{"local delivery", notify.Event{Type: notify.EventNewMessage, UserID: 1, Mailbox: "INBOX", MessageID: 42}, true},
		{"restore from spam (no message id)", notify.Event{Type: notify.EventNewMessage, UserID: 1, Mailbox: "INBOX", Count: 3}, false},
		{"flags", notify.Event{Type: notify.EventFlagsChanged, UserID: 1, Mailbox: "INBOX", MessageID: 42}, false},
		{"own sent message", notify.Event{Type: notify.EventMessageSent, UserID: 1, MessageID: 42}, false},
		{"other folder", notify.Event{Type: notify.EventNewMessage, UserID: 1, Mailbox: "Archive", MessageID: 42}, false},
		{"no user", notify.Event{Type: notify.EventNewMessage, Mailbox: "INBOX", MessageID: 42}, false},
	} {
		if got := IsNewMail(c.ev); got != c.want {
			t.Errorf("%s: IsNewMail = %v, want %v", c.name, got, c.want)
		}
	}
}

// Первое письмо будит сразу, пачка в окне — одним push в его конце.
func TestDispatcherCoalescesPerUser(t *testing.T) {
	store := &fakeStore{tokens: map[int64][]models.PushToken{
		1: {{ID: 10, UserID: 1, Kind: "test", Token: "a"}},
		2: {{ID: 20, UserID: 2, Kind: "test", Token: "b"}},
	}}
	s := &countingSender{kind: "test", sends: map[int64]int{}}
	window := 300 * time.Millisecond
	d := NewDispatcher(store, window, s)
	defer d.Stop()

	start := time.Now()
	d.NewMail(1)
	waitFor(t, "first push", func() bool { return s.count(1) == 1 })
	if time.Since(start) > window/2 {
		t.Error("first push waited for the window")
	}
	for i := 0; i < 5; i++ {
		d.NewMail(1)
	}
	d.NewMail(2) // у другого пользователя своё окно
	waitFor(t, "user 2 push", func() bool { return s.count(2) == 1 })
	if got := s.count(1); got != 1 {
		t.Fatalf("user 1 got %d pushes inside the window, want 1", got)
	}
	waitFor(t, "trailing push", func() bool { return s.count(1) == 2 })
	if time.Since(start) < window {
		t.Error("trailing push came before the window closed")
	}
	time.Sleep(window + 100*time.Millisecond)
	if got := s.count(1); got != 2 {
		t.Errorf("user 1 got %d pushes, want 2 (one now, one for the batch)", got)
	}
	_, sent := store.snapshot()
	if len(sent) != 3 {
		t.Errorf("marked sent %v, want 3 deliveries", sent)
	}
}

// Токен, который служба отвергла, удаляется; остальные устройства получают push.
func TestDispatcherDropsRejectedToken(t *testing.T) {
	store := &fakeStore{tokens: map[int64][]models.PushToken{
		1: {
			{ID: 10, UserID: 1, Kind: "test", Token: "dead"},
			{ID: 11, UserID: 1, Kind: "test", Token: "alive"},
			{ID: 12, UserID: 1, Kind: "hms", Token: "no-sender"},
		},
	}}
	s := &countingSender{kind: "test", sends: map[int64]int{}, err: func(t models.PushToken) error {
		if t.Token == "dead" {
			return &TokenRejectedError{Status: http.StatusGone, Reason: "Unregistered"}
		}
		return nil
	}}
	d := NewDispatcher(store, time.Second, s)
	defer d.Stop()
	d.NewMail(1)
	waitFor(t, "delivery", func() bool { _, sent := store.snapshot(); return len(sent) == 1 })
	deleted, sent := store.snapshot()
	if len(deleted) != 1 || deleted[0] != "test:dead" {
		t.Errorf("deleted = %v, want [test:dead]", deleted)
	}
	if sent[0] != 11 {
		t.Errorf("sent = %v, want [11]", sent)
	}
	if got := s.count(1); got != 2 {
		t.Errorf("sender called %d times, want 2 (hms token has no sender)", got)
	}
}

// Весь путь: событие хаба → APNs на httptest, отвергнутый токен удалён.
func TestDispatcherFromHubToAPNs(t *testing.T) {
	f := newFakeAPNs(t, func(_ int, r recordedPush) (int, string) {
		if strings.HasSuffix(r.Path, strings.Repeat("cd", 32)) {
			return http.StatusGone, "Unregistered"
		}
		return http.StatusOK, ""
	})
	a, _ := newTestAPNs(t, f, config.APNsEnvAuto, &testClock{now: time.Now()})
	good := apnsToken(models.PushEnvSandbox)
	gone := apnsToken(models.PushEnvProduction)
	gone.ID, gone.Token = 2, strings.Repeat("cd", 32)
	store := &fakeStore{tokens: map[int64][]models.PushToken{7: {good, gone}}}

	hub := notify.NewHub()
	d := NewDispatcher(store, time.Second, a)
	d.Attach(hub)

	// Не повод: возврат из спама руками и флаги.
	hub.Publish(notify.Event{Type: notify.EventNewMessage, UserID: 7, Mailbox: "INBOX", Count: 1})
	hub.Publish(notify.Event{Type: notify.EventFlagsChanged, UserID: 7, Mailbox: "INBOX"})
	// Повод: новое письмо.
	hub.Publish(notify.Event{Type: notify.EventNewMessage, UserID: 7, Mailbox: "INBOX", MessageID: 99, From: "secret@example.com", Subject: "Тайна"})

	waitFor(t, "two APNs requests", func() bool { return len(f.requests()) == 2 })
	waitFor(t, "rejected token removed", func() bool { deleted, _ := store.snapshot(); return len(deleted) == 1 })
	for _, r := range f.requests() {
		if strings.Contains(r.Body, "secret@example.com") || strings.Contains(r.Body, "Тайна") {
			t.Errorf("personal data in push payload: %s", r.Body)
		}
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests()); n != 2 {
		t.Errorf("got %d APNs requests, want 2", n)
	}
}

// Без отправителей диспетчер ничего не делает и не мешает остановке.
func TestDispatcherWithoutSenders(t *testing.T) {
	d := NewDispatcher(&fakeStore{}, 0, nil)
	if d.Delivers(models.PushKindAPNs) || len(d.Kinds()) != 0 {
		t.Error("dispatcher without senders claims delivery")
	}
	d.NewMail(1)
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	var none *Dispatcher
	if none.Delivers(models.PushKindAPNs) {
		t.Error("nil dispatcher claims delivery")
	}
}

// После Stop отложенный push не уходит.
func TestDispatcherStopCancelsPending(t *testing.T) {
	store := &fakeStore{tokens: map[int64][]models.PushToken{1: {{ID: 1, UserID: 1, Kind: "test", Token: "a"}}}}
	s := &countingSender{kind: "test", sends: map[int64]int{}}
	d := NewDispatcher(store, 200*time.Millisecond, s)
	d.NewMail(1)
	waitFor(t, "first push", func() bool { return s.count(1) == 1 })
	d.NewMail(1) // ждёт конца окна
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := s.count(1); got != 1 {
		t.Errorf("got %d pushes after Stop, want 1", got)
	}
	d.NewMail(1)
	time.Sleep(50 * time.Millisecond)
	if got := s.count(1); got != 1 {
		t.Errorf("NewMail after Stop sent a push")
	}
}
