package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// Адреса Huawei Push Kit (серверный API v1).
const (
	HMSTokenURL = "https://oauth-login.cloud.huawei.com/oauth2/v3/token"
	HMSPushURL  = "https://push-api.cloud.huawei.com"
)

// HMSData — всё, что Huawei узнаёт о письме: ничего. Data-сообщение лишь
// будит приложение; отправителя и тему оно берёт синхронизацией по нашему
// каналу (android-behavior-contract.md §7).
const HMSData = `{"type":"new_mail"}`

// Обезличенный текст запасного notification-сообщения (push.hms.mode:
// notification) — тот же, что у APNs до замены расширением.
const (
	pushTitle = "ddmail"
	pushBody  = "Новые письма"
)

// Коды ответа messages:send (поле code).
const (
	hmsSuccess        = "80000000"
	hmsPartial        = "80100000" // часть токенов отвергнута, список в msg
	hmsOAuthError     = "80200001"
	hmsOAuthExpired   = "80200003"
	hmsAllTokensWrong = "80300007"
)

const (
	// hmsTokenMargin — access token обновляется раньше срока на столько:
	// запрос, начатый перед самым истечением, не должен получить отказ.
	hmsTokenMargin = 5 * time.Minute
	// pushTTL — сколько служба хранит push для выключенного устройства.
	pushTTL = "86400s"
)

// HMSOptions — настройки отправителя. AppID и ClientSecret обязательны.
type HMSOptions struct {
	AppID        string
	ClientID     string // пусто — AppID
	ClientSecret string
	Mode         string // config.HMSModeData (пусто) | config.HMSModeNotification
	// Client — HTTP-клиент; nil — свой. Тесты подставляют клиент httptest.
	Client *http.Client
	// TokenURL и PushURL — адреса OAuth и Push Kit; пусто — настоящие.
	TokenURL string
	PushURL  string
	// Now — часы (тесты); nil — time.Now.
	Now func() time.Time
}

// HMS — отправитель Huawei Push Kit: OAuth 2.0 client credentials →
// POST /v1/{appId}/messages:send.
type HMS struct {
	appID    string
	clientID string
	secret   string
	mode     string
	client   *http.Client
	tokenURL string
	pushURL  string
	now      func() time.Time

	mu      sync.Mutex
	access  string
	expires time.Time
}

// NewHMS проверяет настройки и собирает отправителя.
func NewHMS(o HMSOptions) (*HMS, error) {
	appID := strings.TrimSpace(o.AppID)
	secret := strings.TrimSpace(o.ClientSecret)
	if appID == "" || secret == "" {
		return nil, fmt.Errorf("hms: app id and client secret are required")
	}
	mode := strings.ToLower(strings.TrimSpace(o.Mode))
	if mode == "" {
		mode = config.HMSModeData
	}
	if mode != config.HMSModeData && mode != config.HMSModeNotification {
		return nil, fmt.Errorf("hms: unknown mode %q", o.Mode)
	}
	h := &HMS{
		appID:    appID,
		clientID: strings.TrimSpace(o.ClientID),
		secret:   secret,
		mode:     mode,
		client:   o.Client,
		tokenURL: o.TokenURL,
		pushURL:  strings.TrimRight(o.PushURL, "/"),
		now:      o.Now,
	}
	if h.clientID == "" {
		h.clientID = appID
	}
	if h.client == nil {
		h.client = &http.Client{Timeout: 20 * time.Second}
	}
	if h.tokenURL == "" {
		h.tokenURL = HMSTokenURL
	}
	if h.pushURL == "" {
		h.pushURL = HMSPushURL
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h, nil
}

// HMSFromConfig — отправитель по секции push.hms конфига.
func HMSFromConfig(c config.HMSConfig) (*HMS, error) {
	return NewHMS(HMSOptions{
		AppID:        c.AppID,
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Mode:         c.ModeOrDefault(),
	})
}

// Kind — служба HMS.
func (h *HMS) Kind() string { return models.PushKindHMS }

// Mode — вид сообщения (data или notification).
func (h *HMS) Mode() string { return h.mode }

// Accepts — любой токен HMS: токен выдан под App ID, а App ID у сервера один.
func (h *HMS) Accepts(t models.PushToken) bool { return t.Kind == models.PushKindHMS }

// hmsMessage — тело messages:send. data у HMS — строка, не объект.
type hmsMessage struct {
	ValidateOnly bool `json:"validate_only"`
	Message      struct {
		Data    string     `json:"data,omitempty"`
		Android hmsAndroid `json:"android"`
		Token   []string   `json:"token"`
	} `json:"message"`
}

type hmsAndroid struct {
	Urgency      string           `json:"urgency,omitempty"`
	TTL          string           `json:"ttl"`
	Notification *hmsNotification `json:"notification,omitempty"`
}

type hmsNotification struct {
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	ClickAction hmsClickAction `json:"click_action"`
}

// hmsClickAction: type 3 — открыть приложение.
type hmsClickAction struct {
	Type int `json:"type"`
}

// hmsPayload — тело запроса на один токен. Ни в одном виде нет ни
// отправителя, ни темы, ни адресов.
func (h *HMS) hmsPayload(token string) ([]byte, error) {
	var m hmsMessage
	m.Message.Token = []string{token}
	m.Message.Android.TTL = pushTTL
	if h.mode == config.HMSModeNotification {
		m.Message.Android.Notification = &hmsNotification{
			Title:       pushTitle,
			Body:        pushBody,
			ClickAction: hmsClickAction{Type: 3},
		}
	} else {
		m.Message.Data = HMSData
		// Высокий приоритет — чтобы data-сообщение будило приложение в фоне.
		m.Message.Android.Urgency = "HIGH"
	}
	return json.Marshal(m)
}

// hmsReply — ответ messages:send.
type hmsReply struct {
	Code      string `json:"code"`
	Msg       string `json:"msg"`
	RequestID string `json:"requestId"`
}

// Send — один push «есть новые письма». Отказ по OAuth (токен доступа
// истёк или отозван) — новый токен доступа и один повтор.
func (h *HMS) Send(ctx context.Context, t models.PushToken) error {
	body, err := h.hmsPayload(t.Token)
	if err != nil {
		return fmt.Errorf("hms: payload: %w", err)
	}
	endpoint := h.pushURL + "/v1/" + url.PathEscape(h.appID) + "/messages:send"
	for attempt := 0; ; attempt++ {
		access, err := h.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("hms: request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("Content-Type", "application/json;charset=utf-8")
		resp, err := h.client.Do(req)
		if err != nil {
			return fmt.Errorf("hms: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8192))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("hms: status %d, read body: %w", resp.StatusCode, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("hms: status %d, close body: %w", resp.StatusCode, closeErr)
		}
		var r hmsReply
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("hms: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		switch {
		case r.Code == hmsSuccess && resp.StatusCode == http.StatusOK:
			return nil
		case r.Code == hmsAllTokensWrong:
			return &TokenRejectedError{Status: resp.StatusCode, Reason: r.Code + " " + r.Msg}
		case r.Code == hmsPartial:
			// Токен в запросе один: «частичный успех» с ним в списке
			// недействительных — это отказ ему.
			if hmsIllegal(r.Msg, t.Token) {
				return &TokenRejectedError{Status: resp.StatusCode, Reason: r.Code + " illegal token"}
			}
			return fmt.Errorf("hms: %s %s", r.Code, r.Msg)
		case (r.Code == hmsOAuthError || r.Code == hmsOAuthExpired || resp.StatusCode == http.StatusUnauthorized) && attempt == 0:
			continue
		default:
			return fmt.Errorf("hms: status %d, code %s %s", resp.StatusCode, r.Code, r.Msg)
		}
	}
}

// hmsIllegal — есть ли токен в списке illegal_tokens частичного ответа
// (msg — JSON строкой: {"success":0,"failure":1,"illegal_tokens":[...]}).
func hmsIllegal(msg, token string) bool {
	var m struct {
		IllegalTokens []string `json:"illegal_tokens"`
	}
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		return false
	}
	for _, it := range m.IllegalTokens {
		if it == token {
			return true
		}
	}
	return false
}

// hmsTokenReply — ответ OAuth.
type hmsTokenReply struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            any    `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// accessToken — токен доступа из кэша; force — взять новый (Push Kit
// отказал по OAuth).
func (h *HMS) accessToken(ctx context.Context, force bool) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if !force && h.access != "" && now.Before(h.expires) {
		return h.access, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {h.clientID},
		"client_secret": {h.secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("hms: oauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("hms: oauth: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8192))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("hms: oauth: read body: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("hms: oauth: close body: %w", closeErr)
	}
	var r hmsTokenReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("hms: oauth: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusOK || r.AccessToken == "" {
		// Секрет в ответ не попадает; описание ошибки — для журнала.
		return "", fmt.Errorf("hms: oauth: status %d, error %v %s", resp.StatusCode, r.Error, r.ErrorDescription)
	}
	ttl := time.Duration(r.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	if ttl > 2*hmsTokenMargin {
		ttl -= hmsTokenMargin
	} else {
		ttl /= 2
	}
	h.access, h.expires = r.AccessToken, now.Add(ttl)
	return h.access, nil
}
