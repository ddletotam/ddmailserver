package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
)

// fakeRuStore — RuStore Push на httptest.
type fakeRuStore struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []recordedPush
	reply func(n int, r recordedPush) (int, string)
}

func newFakeRuStore(t *testing.T, reply func(n int, r recordedPush) (int, string)) *fakeRuStore {
	f := &fakeRuStore{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		rec := recordedPush{Proto: r.ProtoMajor, Path: r.URL.Path, Headers: r.Header.Clone(), Body: string(body)}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		n := len(f.reqs)
		f.mu.Unlock()
		status, out := http.StatusOK, `{}`
		if f.reply != nil {
			status, out = f.reply(n, rec)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, out); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRuStore) sender(t *testing.T) *RuStore {
	t.Helper()
	r, err := NewRuStore(RuStoreOptions{ProjectID: "proj-42", ServiceToken: "svc-token", Client: f.srv.Client(), PushURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fakeRuStore) requests() []recordedPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedPush(nil), f.reqs...)
}

const testRuStoreToken = "rs_AbCdEf-0123456789_token"

func ruStoreToken() models.PushToken {
	return models.PushToken{ID: 2, UserID: 7, Kind: models.PushKindRuStore, Token: testRuStoreToken}
}

func TestRuStoreSendRequestShape(t *testing.T) {
	f := newFakeRuStore(t, nil)
	if err := f.sender(t).Send(context.Background(), ruStoreToken()); err != nil {
		t.Fatalf("send: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/v1/projects/proj-42/messages:send" {
		t.Errorf("path = %q", r.Path)
	}
	if got := r.Headers.Get("Authorization"); got != "Bearer svc-token" {
		t.Errorf("authorization = %q", got)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		t.Fatalf("body %s: %v", r.Body, err)
	}
	msg := m["message"]
	if msg["token"] != testRuStoreToken {
		t.Errorf("token = %v", msg["token"])
	}
	// Только data: notification показал бы SDK сам, мимо текста из кэша.
	if _, ok := msg["notification"]; ok {
		t.Errorf("message has notification: %s", r.Body)
	}
	data, _ := msg["data"].(map[string]any)
	if len(data) != 1 || data["type"] != "new_mail" {
		t.Errorf("data = %v, want only type=new_mail", msg["data"])
	}
	android, _ := msg["android"].(map[string]any)
	if android["ttl"] != "86400s" {
		t.Errorf("android = %v", msg["android"])
	}
}

func TestRuStoreRejectedTokens(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   int
		body     string
		rejected bool
	}{
		{"expired", http.StatusNotFound, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`, true},
		{"unregistered", http.StatusNotFound, `{"error":{"code":404,"message":"unregistered","status":"UNREGISTERED"}}`, true},
		{"malformed token", http.StatusBadRequest, `{"error":{"code":400,"message":"The registration token is not a valid FCM registration token","status":"INVALID_ARGUMENT"}}`, true},
		{"bad request", http.StatusBadRequest, `{"error":{"code":400,"message":"Invalid JSON payload","status":"INVALID_ARGUMENT"}}`, false},
		{"bad service token", http.StatusForbidden, `{"error":{"code":403,"message":"forbidden","status":"PERMISSION_DENIED"}}`, false},
		{"wrong project", http.StatusNotFound, `{"error":{"code":404,"message":"project not found","status":"NOT_FOUND"}}`, false},
		{"404 without body", http.StatusNotFound, `404 page not found`, false},
		{"rate limit", http.StatusTooManyRequests, `{"error":{"code":429,"message":"limit","status":"TOO_MANY_REQUESTS"}}`, false},
	} {
		f := newFakeRuStore(t, func(int, recordedPush) (int, string) { return c.status, c.body })
		err := f.sender(t).Send(context.Background(), ruStoreToken())
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		var rejected *TokenRejectedError
		if errors.As(err, &rejected) != c.rejected {
			t.Errorf("%s: err = %v, rejected want %v", c.name, err, c.rejected)
		}
		if strings.Contains(err.Error(), "svc-token") {
			t.Errorf("%s: error leaks the service token: %v", c.name, err)
		}
	}
}

func TestNewRuStoreValidates(t *testing.T) {
	for _, o := range []RuStoreOptions{{ProjectID: "p"}, {ServiceToken: "t"}, {ProjectID: " ", ServiceToken: " "}} {
		if _, err := NewRuStore(o); err == nil {
			t.Errorf("NewRuStore(%+v) succeeded", o)
		}
	}
	r, err := NewRuStore(RuStoreOptions{ProjectID: "p", ServiceToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Accepts(ruStoreToken()) || r.Accepts(hmsToken()) {
		t.Error("Accepts: wrong kinds")
	}
}

// От события хаба до обеих служб Android: у пользователя телефон Huawei и
// телефон со сборкой RuStore; отвергнутый RuStore токен удаляется, HMS
// доставлен. ПДн события в запросы не попадают.
func TestDispatcherFromHubToAndroidServices(t *testing.T) {
	hms := newFakeHMS(t)
	rs := newFakeRuStore(t, func(int, recordedPush) (int, string) {
		return http.StatusNotFound, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`
	})
	store := &fakeStore{tokens: map[int64][]models.PushToken{7: {hmsToken(), ruStoreToken()}}}
	d := NewDispatcher(store, time.Second, hms.sender(t, "", nil), rs.sender(t))
	hub := notify.NewHub()
	d.Attach(hub)
	hub.Publish(notify.Event{Type: notify.EventNewMessage, UserID: 7, MessageID: 100, Mailbox: "INBOX", From: "secret@example.com", Subject: "Тайна"})

	waitFor(t, "deliveries", func() bool {
		deleted, sent := store.snapshot()
		return len(deleted) == 1 && len(sent) == 1
	})
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	deleted, sent := store.snapshot()
	if deleted[0] != "rustore:"+testRuStoreToken || sent[0] != 1 {
		t.Errorf("deleted=%v sent=%v", deleted, sent)
	}
	bodies := []string{hms.lastPush().Body}
	for _, r := range rs.requests() {
		bodies = append(bodies, r.Body)
	}
	for _, b := range bodies {
		if strings.Contains(b, "secret@example.com") || strings.Contains(b, "Тайна") {
			t.Errorf("personal data in push payload: %s", b)
		}
	}
}
