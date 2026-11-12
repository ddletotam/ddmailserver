package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/golang-jwt/jwt/v5"
)

// fakeAPNs — APNs на httptest: HTTP/2 по TLS, записывает запросы и отвечает
// тем, что скажет reply.
type fakeAPNs struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []recordedPush
	reply func(n int, r recordedPush) (int, string)
}

type recordedPush struct {
	Proto   int
	Path    string
	Headers http.Header
	Body    string
}

func newFakeAPNs(t *testing.T, reply func(n int, r recordedPush) (int, string)) *fakeAPNs {
	f := &fakeAPNs{t: t, reply: reply}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		rec := recordedPush{Proto: r.ProtoMajor, Path: r.URL.Path, Headers: r.Header.Clone(), Body: string(body)}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		n := len(f.reqs)
		f.mu.Unlock()
		status, reason := http.StatusOK, ""
		if f.reply != nil {
			status, reason = f.reply(n, rec)
		}
		w.Header().Set("apns-id", "00000000-0000-0000-0000-000000000000")
		w.WriteHeader(status)
		if reason != "" {
			if err := json.NewEncoder(w).Encode(map[string]string{"reason": reason}); err != nil {
				t.Errorf("write reply: %v", err)
			}
		}
	}))
	f.srv.EnableHTTP2 = true
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPNs) requests() []recordedPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedPush(nil), f.reqs...)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newTestAPNs — отправитель на фальшивый APNs: оба окружения — на один
// сервер, но разными путями-префиксами, чтобы было видно, куда ушло.
func newTestAPNs(t *testing.T, f *fakeAPNs, env string, clock *testClock) (*APNs, *ecdsa.PrivateKey) {
	t.Helper()
	key := newTestKey(t)
	a, err := NewAPNs(APNsOptions{
		Key: key, KeyID: "KEY1234567", TeamID: "TEAM123456", Topic: "ru.letotam.ddmail",
		Environment: env,
		Client:      f.srv.Client(),
		Endpoints: map[string]string{
			models.PushEnvProduction: f.srv.URL + "/prod",
			models.PushEnvSandbox:    f.srv.URL + "/sandbox",
		},
		Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, key
}

func apnsToken(env string) models.PushToken {
	return models.PushToken{ID: 1, UserID: 7, Kind: models.PushKindAPNs, Token: strings.Repeat("ab", 32), Environment: env, Topic: "ru.letotam.ddmail"}
}

// bearerClaims проверяет подпись JWT провайдера открытым ключом и
// возвращает его заголовок и утверждения.
func bearerClaims(t *testing.T, h http.Header, key *ecdsa.PrivateKey) (map[string]any, jwt.MapClaims) {
	t.Helper()
	auth := h.Get("Authorization")
	raw, ok := strings.CutPrefix(auth, "bearer ")
	if !ok {
		t.Fatalf("authorization = %q, want bearer", auth)
	}
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"ES256"}), jwt.WithoutClaimsValidation())
	if err != nil {
		t.Fatalf("provider token: %v", err)
	}
	return tok.Header, claims
}

func TestAPNsSendRequestShape(t *testing.T) {
	f := newFakeAPNs(t, nil)
	clock := &testClock{now: time.Unix(1_800_000_000, 0)}
	a, key := newTestAPNs(t, f, config.APNsEnvAuto, clock)

	if err := a.Send(context.Background(), apnsToken(models.PushEnvSandbox)); err != nil {
		t.Fatalf("send: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Proto != 2 {
		t.Errorf("HTTP/%d, APNs needs HTTP/2", r.Proto)
	}
	if want := "/sandbox/3/device/" + strings.Repeat("ab", 32); r.Path != want {
		t.Errorf("path = %q, want %q", r.Path, want)
	}
	for k, want := range map[string]string{
		"apns-push-type": "alert",
		"apns-topic":     "ru.letotam.ddmail",
		"apns-priority":  "10",
	} {
		if got := r.Headers.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got, want := r.Headers.Get("apns-expiration"), "1800086400"; got != want {
		t.Errorf("apns-expiration = %q, want %q", got, want)
	}
	if r.Headers.Get("apns-collapse-id") != "" {
		t.Error("apns-collapse-id must not be set: a new push would replace the notification about another conversation")
	}

	header, claims := bearerClaims(t, r.Headers, key)
	if header["kid"] != "KEY1234567" || header["alg"] != "ES256" {
		t.Errorf("jwt header = %v", header)
	}
	if claims["iss"] != "TEAM123456" || claims["iat"] != float64(1_800_000_000) {
		t.Errorf("jwt claims = %v", claims)
	}
}

// Ни отправителя, ни темы, ни адресов: только обезличенный текст и
// mutable-content для расширения.
func TestAPNsPayloadCarriesNoPersonalData(t *testing.T) {
	f := newFakeAPNs(t, nil)
	a, _ := newTestAPNs(t, f, config.APNsEnvAuto, &testClock{now: time.Now()})
	if err := a.Send(context.Background(), apnsToken(models.PushEnvProduction)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(f.requests()[0].Body), &got); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	want := map[string]any{"aps": map[string]any{
		"alert":           map[string]any{"title": "ddmail", "body": "Новые письма"},
		"mutable-content": float64(1),
		"sound":           "default",
		"thread-id":       "mail",
	}}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("payload = %s\nwant      %s", gotJSON, wantJSON)
	}
	if !strings.HasPrefix(f.requests()[0].Path, "/prod/") {
		t.Errorf("production token went to %s", f.requests()[0].Path)
	}
}

func TestAPNsProviderTokenCached(t *testing.T) {
	f := newFakeAPNs(t, nil)
	clock := &testClock{now: time.Unix(1_800_000_000, 0)}
	a, _ := newTestAPNs(t, f, config.APNsEnvAuto, clock)
	tok := apnsToken(models.PushEnvSandbox)

	send := func() string {
		t.Helper()
		if err := a.Send(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
		reqs := f.requests()
		return reqs[len(reqs)-1].Headers.Get("Authorization")
	}
	first := send()
	clock.Advance(49 * time.Minute)
	if second := send(); second != first {
		t.Error("provider token re-signed before 50 minutes")
	}
	clock.Advance(2 * time.Minute)
	if third := send(); third == first {
		t.Error("provider token not refreshed after 50 minutes")
	}
}

func TestAPNsExpiredProviderTokenRetriesOnce(t *testing.T) {
	f := newFakeAPNs(t, func(n int, r recordedPush) (int, string) {
		if n == 2 { // второй запрос — первая попытка второй отправки
			return http.StatusForbidden, "ExpiredProviderToken"
		}
		return http.StatusOK, ""
	})
	clock := &testClock{now: time.Unix(1_800_000_000, 0)}
	a, _ := newTestAPNs(t, f, config.APNsEnvAuto, clock)
	tok := apnsToken(models.PushEnvSandbox)
	if err := a.Send(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Minute)
	if err := a.Send(context.Background(), tok); err != nil {
		t.Fatalf("send after expired provider token: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3 (send, expired, retry)", len(reqs))
	}
	if reqs[1].Headers.Get("Authorization") == reqs[2].Headers.Get("Authorization") {
		t.Error("retry used the same provider token")
	}
}

func TestAPNsRejectedTokens(t *testing.T) {
	cases := []struct {
		status   int
		reason   string
		rejected bool
	}{
		{http.StatusGone, "Unregistered", true},
		{http.StatusBadRequest, "BadDeviceToken", true},
		{http.StatusBadRequest, "BadTopic", false},
		{http.StatusForbidden, "InvalidProviderToken", false},
		{http.StatusTooManyRequests, "TooManyRequests", false},
		{http.StatusServiceUnavailable, "ServiceUnavailable", false},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			f := newFakeAPNs(t, func(int, recordedPush) (int, string) { return c.status, c.reason })
			a, _ := newTestAPNs(t, f, config.APNsEnvAuto, &testClock{now: time.Now()})
			err := a.Send(context.Background(), apnsToken(models.PushEnvProduction))
			if err == nil {
				t.Fatal("want error")
			}
			var rej *TokenRejectedError
			if got := errors.As(err, &rej); got != c.rejected {
				t.Errorf("rejected = %v, want %v (err %v)", got, c.rejected, err)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("got %d requests, want 1", n)
			}
		})
	}
}

func TestAPNsAccepts(t *testing.T) {
	f := newFakeAPNs(t, nil)
	clock := &testClock{now: time.Now()}
	auto, _ := newTestAPNs(t, f, config.APNsEnvAuto, clock)
	prod, _ := newTestAPNs(t, f, config.APNsEnvProduction, clock)

	sandbox := apnsToken(models.PushEnvSandbox)
	production := apnsToken(models.PushEnvProduction)
	other := apnsToken(models.PushEnvProduction)
	other.Topic = "com.example.other"
	noTopic := apnsToken(models.PushEnvProduction)
	noTopic.Topic = ""
	hms := models.PushToken{Kind: models.PushKindHMS, Token: "x"}

	for _, c := range []struct {
		name string
		a    *APNs
		t    models.PushToken
		want bool
	}{
		{"auto/sandbox", auto, sandbox, true},
		{"auto/production", auto, production, true},
		{"production-only/sandbox", prod, sandbox, false},
		{"production-only/production", prod, production, true},
		{"other app", auto, other, false},
		{"topic from config", auto, noTopic, true},
		{"not apns", auto, hms, false},
	} {
		if got := c.a.Accepts(c.t); got != c.want {
			t.Errorf("%s: Accepts = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseAPNsKey(t *testing.T) {
	key := newTestKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	got, err := ParseAPNsKey(p8)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Equal(key) {
		t.Error("parsed key differs")
	}
	if _, err := ParseAPNsKey([]byte("not a key")); err == nil {
		t.Error("garbage parsed as a key")
	}
}

func TestNewAPNsValidates(t *testing.T) {
	key := newTestKey(t)
	if _, err := NewAPNs(APNsOptions{Key: key, KeyID: "K", TeamID: "T"}); err == nil {
		t.Error("missing topic accepted")
	}
	if _, err := NewAPNs(APNsOptions{Key: key, KeyID: "K", TeamID: "T", Topic: "x", Environment: "staging"}); err == nil {
		t.Error("unknown environment accepted")
	}
}
