package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// fakeHMS — OAuth и Push Kit Huawei на одном httptest-сервере.
type fakeHMS struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	oauth  []url.Values
	pushes []recordedPush
	// reply — ответ messages:send на n-й запрос (с 1): HTTP-код и тело.
	reply func(n int, r recordedPush) (int, string)
	// oauthReply — ответ OAuth; nil — выдать токен "at-<n>" на час.
	oauthReply func(n int) (int, string)
}

func newFakeHMS(t *testing.T) *fakeHMS {
	f := &fakeHMS{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/v3/token" {
			form, err := url.ParseQuery(string(body))
			if err != nil {
				t.Errorf("oauth form: %v", err)
			}
			f.mu.Lock()
			f.oauth = append(f.oauth, form)
			n := len(f.oauth)
			f.mu.Unlock()
			status, out := http.StatusOK, fmt.Sprintf(`{"access_token":"at-%d","expires_in":3600,"token_type":"Bearer"}`, n)
			if f.oauthReply != nil {
				status, out = f.oauthReply(n)
			}
			w.WriteHeader(status)
			if _, err := io.WriteString(w, out); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		rec := recordedPush{Proto: r.ProtoMajor, Path: r.URL.Path, Headers: r.Header.Clone(), Body: string(body)}
		f.mu.Lock()
		f.pushes = append(f.pushes, rec)
		n := len(f.pushes)
		f.mu.Unlock()
		status, out := http.StatusOK, `{"code":"80000000","msg":"Success","requestId":"r1"}`
		if f.reply != nil {
			status, out = f.reply(n, rec)
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, out); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHMS) sender(t *testing.T, mode string, now func() time.Time) *HMS {
	t.Helper()
	h, err := NewHMS(HMSOptions{
		AppID:        "107000001",
		ClientSecret: "s3cret",
		Mode:         mode,
		Client:       f.srv.Client(),
		TokenURL:     f.srv.URL + "/oauth2/v3/token",
		PushURL:      f.srv.URL,
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *fakeHMS) counts() (oauth, pushes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.oauth), len(f.pushes)
}

func (f *fakeHMS) lastPush() recordedPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushes[len(f.pushes)-1]
}

const testHMSToken = "IQAAAACy0kYkAADxk2Hr_testtoken-123"

func hmsToken() models.PushToken {
	return models.PushToken{ID: 1, UserID: 7, Kind: models.PushKindHMS, Token: testHMSToken}
}

func TestHMSSendRequestShape(t *testing.T) {
	f := newFakeHMS(t)
	h := f.sender(t, "", nil)
	if err := h.Send(context.Background(), hmsToken()); err != nil {
		t.Fatalf("send: %v", err)
	}
	f.mu.Lock()
	form := f.oauth[0]
	f.mu.Unlock()
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "107000001" || form.Get("client_secret") != "s3cret" {
		t.Errorf("oauth form = %v", form)
	}
	p := f.lastPush()
	if p.Path != "/v1/107000001/messages:send" {
		t.Errorf("path = %q", p.Path)
	}
	if got := p.Headers.Get("Authorization"); got != "Bearer at-1" {
		t.Errorf("authorization = %q", got)
	}
	var m struct {
		ValidateOnly bool `json:"validate_only"`
		Message      struct {
			Data    string `json:"data"`
			Token   []string
			Android struct {
				Urgency      string          `json:"urgency"`
				TTL          string          `json:"ttl"`
				Notification json.RawMessage `json:"notification"`
			} `json:"android"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(p.Body), &m); err != nil {
		t.Fatalf("body %s: %v", p.Body, err)
	}
	if m.ValidateOnly || m.Message.Data != HMSData || len(m.Message.Token) != 1 || m.Message.Token[0] != testHMSToken {
		t.Errorf("message = %+v", m)
	}
	if m.Message.Android.Urgency != "HIGH" || m.Message.Android.TTL != "86400s" || m.Message.Android.Notification != nil {
		t.Errorf("android = %+v", m.Message.Android)
	}
}

func TestHMSNotificationMode(t *testing.T) {
	f := newFakeHMS(t)
	h := f.sender(t, config.HMSModeNotification, nil)
	if err := h.Send(context.Background(), hmsToken()); err != nil {
		t.Fatalf("send: %v", err)
	}
	body := f.lastPush().Body
	for _, want := range []string{`"title":"ddmail"`, `"body":"Новые письма"`, `"click_action":{"type":3}`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s lacks %s", body, want)
		}
	}
	if strings.Contains(body, `"data"`) {
		t.Errorf("notification mode carries data: %s", body)
	}
}

// Что бы ни случилось, кроме токена устройства, в запросе нет ничего
// изменчивого: тело одинаково для любого письма (ПДн взяться неоткуда).
func TestHMSPayloadCarriesNoPersonalData(t *testing.T) {
	for _, mode := range []string{config.HMSModeData, config.HMSModeNotification} {
		f := newFakeHMS(t)
		h := f.sender(t, mode, nil)
		if err := h.Send(context.Background(), hmsToken()); err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(f.lastPush().Body, testHMSToken, "", 1)
		if strings.Contains(body, "@") {
			t.Errorf("%s: body has an address: %s", mode, body)
		}
	}
}

func TestHMSAccessTokenCachedAndRefreshed(t *testing.T) {
	f := newFakeHMS(t)
	clock := &testClock{now: time.Unix(1_800_000_000, 0)}
	h := f.sender(t, "", clock.Now)
	for i := 0; i < 3; i++ {
		if err := h.Send(context.Background(), hmsToken()); err != nil {
			t.Fatal(err)
		}
	}
	if o, p := f.counts(); o != 1 || p != 3 {
		t.Fatalf("oauth=%d pushes=%d, want 1 and 3", o, p)
	}
	// Час жизни минус запас: через 56 минут токен уже новый.
	clock.Advance(56 * time.Minute)
	if err := h.Send(context.Background(), hmsToken()); err != nil {
		t.Fatal(err)
	}
	if o, _ := f.counts(); o != 2 {
		t.Errorf("oauth=%d after expiry, want 2", o)
	}
	if got := f.lastPush().Headers.Get("Authorization"); got != "Bearer at-2" {
		t.Errorf("authorization = %q, want the new token", got)
	}
}

func TestHMSOAuthRejectedRetriesOnce(t *testing.T) {
	f := newFakeHMS(t)
	f.reply = func(n int, _ recordedPush) (int, string) {
		if n == 1 {
			return http.StatusUnauthorized, `{"code":"80200003","msg":"OAuth token expired"}`
		}
		return http.StatusOK, `{"code":"80000000","msg":"Success"}`
	}
	h := f.sender(t, "", nil)
	if err := h.Send(context.Background(), hmsToken()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if o, p := f.counts(); o != 2 || p != 2 {
		t.Errorf("oauth=%d pushes=%d, want 2 and 2", o, p)
	}

	// Отказ и после нового токена — ошибка, без бесконечных повторов.
	f2 := newFakeHMS(t)
	f2.reply = func(int, recordedPush) (int, string) {
		return http.StatusUnauthorized, `{"code":"80200001","msg":"Oauth authentication error"}`
	}
	err := f2.sender(t, "", nil).Send(context.Background(), hmsToken())
	var rejected *TokenRejectedError
	if err == nil || errors.As(err, &rejected) {
		t.Errorf("err = %v, want a plain error", err)
	}
	if _, p := f2.counts(); p != 2 {
		t.Errorf("pushes = %d, want 2", p)
	}
}

func TestHMSRejectedTokens(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   int
		body     string
		rejected bool
	}{
		{"all tokens invalid", http.StatusOK, `{"code":"80300007","msg":"All the tokens are invalid"}`, true},
		{"partial, ours illegal", http.StatusOK, `{"code":"80100000","msg":"{\"success\":0,\"failure\":1,\"illegal_tokens\":[\"` + testHMSToken + `\"]}"}`, true},
		{"partial, other illegal", http.StatusOK, `{"code":"80100000","msg":"{\"success\":1,\"failure\":1,\"illegal_tokens\":[\"other\"]}"}`, false},
		{"no permission", http.StatusOK, `{"code":"80300002","msg":"The current app does not have the permission to send messages"}`, false},
		{"server error", http.StatusInternalServerError, `{"code":"81000001","msg":"System inner error"}`, false},
		{"not json", http.StatusBadGateway, `<html>bad gateway</html>`, false},
	} {
		f := newFakeHMS(t)
		f.reply = func(int, recordedPush) (int, string) { return c.status, c.body }
		err := f.sender(t, "", nil).Send(context.Background(), hmsToken())
		var rejected *TokenRejectedError
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if errors.As(err, &rejected) != c.rejected {
			t.Errorf("%s: err = %v, rejected want %v", c.name, err, c.rejected)
		}
	}
}

func TestHMSOAuthFailure(t *testing.T) {
	f := newFakeHMS(t)
	f.oauthReply = func(int) (int, string) {
		return http.StatusBadRequest, `{"error":1101,"sub_error":20003,"error_description":"invalid client"}`
	}
	err := f.sender(t, "", nil).Send(context.Background(), hmsToken())
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v, want an error without the secret", err)
	}
	if _, p := f.counts(); p != 0 {
		t.Errorf("pushes = %d without an access token", p)
	}
}

func TestHMSAccepts(t *testing.T) {
	h, err := NewHMS(HMSOptions{AppID: "1", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Accepts(hmsToken()) {
		t.Error("hms token not accepted")
	}
	if h.Accepts(models.PushToken{Kind: models.PushKindRuStore, Token: "x"}) {
		t.Error("rustore token accepted by hms")
	}
}

func TestNewHMSValidates(t *testing.T) {
	for _, o := range []HMSOptions{
		{ClientSecret: "s"},
		{AppID: "1"},
		{AppID: "1", ClientSecret: "s", Mode: "silent"},
	} {
		if _, err := NewHMS(o); err == nil {
			t.Errorf("NewHMS(%+v) succeeded", o)
		}
	}
	h, err := NewHMS(HMSOptions{AppID: "1", ClientSecret: "s"})
	if err != nil || h.Mode() != config.HMSModeData || h.clientID != "1" {
		t.Errorf("defaults: %+v %v", h, err)
	}
}
