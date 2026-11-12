package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/push"
)

// memPushTokens — таблица push_tokens в памяти с тем же ключом (user, kind, token).
type memPushTokens struct {
	mu   sync.Mutex
	rows map[string]models.PushToken
}

func (m *memPushTokens) key(user int64, kind, token string) string {
	return fmt.Sprintf("%d|%s|%s", user, kind, token)
}

func (m *memPushTokens) UpsertPushToken(_ context.Context, t models.PushToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]models.PushToken{}
	}
	m.rows[m.key(t.UserID, t.Kind, t.Token)] = t
	return nil
}

func (m *memPushTokens) DeleteUserPushToken(_ context.Context, user int64, kind, token string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(user, kind, token)
	_, ok := m.rows[k]
	delete(m.rows, k)
	return ok, nil
}

func (m *memPushTokens) all() []models.PushToken {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.PushToken
	for _, t := range m.rows {
		out = append(out, t)
	}
	return out
}

func pushRequest(t *testing.T, s *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	req = req.WithContext(setUserContext(req.Context(), &models.User{ID: 7, Username: "alice"}))
	rec := httptest.NewRecorder()
	if method == http.MethodDelete {
		s.HandleDesktopPushTokenDelete(rec, req)
	} else {
		s.HandleDesktopPushTokenRegister(rec, req)
	}
	return rec
}

const testAPNsToken = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"

func TestPushTokenRegisterIdempotent(t *testing.T) {
	store := &memPushTokens{}
	s := &Server{pushTokens: store}
	body := map[string]string{"kind": "apns", "token": testAPNsToken, "environment": "sandbox", "topic": "ru.letotam.ddmail"}
	for i := 0; i < 2; i++ {
		rec := pushRequest(t, s, http.MethodPost, "/api/desktop/v1/push/token", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("register #%d: %d %s", i+1, rec.Code, rec.Body)
		}
		var resp pushTokenResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Kind != "apns" || resp.Delivery {
			t.Errorf("response = %+v, want apns without delivery (no sender)", resp)
		}
	}
	rows := store.all()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.UserID != 7 || r.Token != strings.ToLower(testAPNsToken) || r.Environment != "sandbox" || r.Topic != "ru.letotam.ddmail" {
		t.Errorf("stored %+v", r)
	}
}

func TestPushTokenRegisterReportsDelivery(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	apns, err := push.NewAPNs(push.APNsOptions{Key: key, KeyID: "K", TeamID: "T", Topic: "ru.letotam.ddmail"})
	if err != nil {
		t.Fatal(err)
	}
	d := push.NewDispatcher(nil, time.Second, apns)
	defer d.Stop()
	s := &Server{pushTokens: &memPushTokens{}}
	s.SetPush(d)

	rec := pushRequest(t, s, http.MethodPost, "/", map[string]string{"kind": "apns", "token": testAPNsToken, "environment": "production"})
	var resp pushTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Delivery {
		t.Error("delivery = false with an APNs sender configured")
	}
	rec = pushRequest(t, s, http.MethodPost, "/", map[string]string{"kind": "fcm", "token": "abc:DEF-123_x"})
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || resp.Delivery {
		t.Errorf("fcm: %d delivery=%v, want 200 without delivery", rec.Code, resp.Delivery)
	}
}

func TestPushTokenRegisterValidates(t *testing.T) {
	s := &Server{pushTokens: &memPushTokens{}}
	for _, c := range []struct {
		name string
		body map[string]string
	}{
		{"unknown kind", map[string]string{"kind": "webpush", "token": "x"}},
		{"apns not hex", map[string]string{"kind": "apns", "token": strings.Repeat("zz", 32), "environment": "sandbox"}},
		{"apns path injection", map[string]string{"kind": "apns", "token": "../../3/device/" + testAPNsToken, "environment": "sandbox"}},
		{"apns too short", map[string]string{"kind": "apns", "token": "abcd", "environment": "sandbox"}},
		{"apns no environment", map[string]string{"kind": "apns", "token": testAPNsToken}},
		{"apns bad environment", map[string]string{"kind": "apns", "token": testAPNsToken, "environment": "staging"}},
		{"bad topic", map[string]string{"kind": "apns", "token": testAPNsToken, "environment": "sandbox", "topic": "ru.letotam/ddmail"}},
		{"fcm empty", map[string]string{"kind": "fcm", "token": ""}},
		{"fcm with environment", map[string]string{"kind": "fcm", "token": "abc", "environment": "sandbox"}},
		{"hms with spaces", map[string]string{"kind": "hms", "token": "abc def"}},
	} {
		rec := pushRequest(t, s, http.MethodPost, "/", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", c.name, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{"))
	req = req.WithContext(setUserContext(req.Context(), &models.User{ID: 7}))
	rec := httptest.NewRecorder()
	s.HandleDesktopPushTokenRegister(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("broken JSON: %d, want 400", rec.Code)
	}
}

func TestPushTokenDelete(t *testing.T) {
	store := &memPushTokens{}
	s := &Server{pushTokens: store}
	pushRequest(t, s, http.MethodPost, "/", map[string]string{"kind": "apns", "token": testAPNsToken, "environment": "sandbox"})

	rec := pushRequest(t, s, http.MethodDelete, "/", map[string]string{"kind": "apns", "token": testAPNsToken})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if len(store.all()) != 0 {
		t.Error("token still stored")
	}
	// Повтор (выход дошёл со второго раза) — тоже успех.
	rec = pushRequest(t, s, http.MethodDelete, "/?kind=apns&token="+testAPNsToken, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":false`) {
		t.Fatalf("repeat delete: %d %s", rec.Code, rec.Body)
	}
}

func TestPushTokenNeedsUser(t *testing.T) {
	s := &Server{pushTokens: &memPushTokens{}}
	for _, h := range []http.HandlerFunc{s.HandleDesktopPushTokenRegister, s.HandleDesktopPushTokenDelete} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("no user: %d, want 401", rec.Code)
		}
	}
}
