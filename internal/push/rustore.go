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
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// RuStorePushURL — адрес RuStore Push (VK Push Notification Service).
const RuStorePushURL = "https://vkpns.rustore.ru"

// RuStoreOptions — настройки отправителя. ProjectID и ServiceToken
// обязательны.
type RuStoreOptions struct {
	ProjectID    string
	ServiceToken string
	// Client — HTTP-клиент; nil — свой. Тесты подставляют клиент httptest.
	Client *http.Client
	// PushURL — адрес службы; пусто — настоящий.
	PushURL string
}

// RuStore — отправитель RuStore Push: POST
// /v1/projects/{projectId}/messages:send с сервисным токеном проекта.
type RuStore struct {
	projectID string
	token     string
	client    *http.Client
	pushURL   string
}

// NewRuStore проверяет настройки и собирает отправителя.
func NewRuStore(o RuStoreOptions) (*RuStore, error) {
	r := &RuStore{
		projectID: strings.TrimSpace(o.ProjectID),
		token:     strings.TrimSpace(o.ServiceToken),
		client:    o.Client,
		pushURL:   strings.TrimRight(o.PushURL, "/"),
	}
	if r.projectID == "" || r.token == "" {
		return nil, fmt.Errorf("rustore: project id and service token are required")
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: 20 * time.Second}
	}
	if r.pushURL == "" {
		r.pushURL = RuStorePushURL
	}
	return r, nil
}

// RuStoreFromConfig — отправитель по секции push.rustore конфига.
func RuStoreFromConfig(c config.RuStoreConfig) (*RuStore, error) {
	return NewRuStore(RuStoreOptions{ProjectID: c.ProjectID, ServiceToken: c.ServiceToken})
}

// Kind — служба RuStore.
func (r *RuStore) Kind() string { return models.PushKindRuStore }

// Accepts — любой токен RuStore: он выдан под проект, а проект у сервера один.
func (r *RuStore) Accepts(t models.PushToken) bool { return t.Kind == models.PushKindRuStore }

// ruStoreMessage — тело messages:send. Только data: с notification SDK
// показал бы уведомление сам, а текст уведомления приложение собирает из
// своего кэша (android-behavior-contract.md §7). В data — ничего о письме.
type ruStoreMessage struct {
	Message struct {
		Token   string            `json:"token"`
		Data    map[string]string `json:"data"`
		Android struct {
			TTL string `json:"ttl"`
		} `json:"android"`
	} `json:"message"`
}

func ruStorePayload(token string) ([]byte, error) {
	var m ruStoreMessage
	m.Message.Token = token
	m.Message.Data = map[string]string{"type": "new_mail"}
	m.Message.Android.TTL = pushTTL
	return json.Marshal(m)
}

// ruStoreReply — ответ с ошибкой: {"error":{"code","message","status"}}.
type ruStoreReply struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Send — один push «есть новые письма».
func (r *RuStore) Send(ctx context.Context, t models.PushToken) error {
	body, err := ruStorePayload(t.Token)
	if err != nil {
		return fmt.Errorf("rustore: payload: %w", err)
	}
	endpoint := r.pushURL + "/v1/projects/" + url.PathEscape(r.projectID) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("rustore: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("rustore: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8192))
	closeErr := resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if readErr != nil {
		return fmt.Errorf("rustore: status %d, read body: %w", resp.StatusCode, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("rustore: status %d, close body: %w", resp.StatusCode, closeErr)
	}
	var rep ruStoreReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		rep.Error.Message = strings.TrimSpace(string(raw))
	}
	if ruStoreTokenRejected(resp.StatusCode, rep.Error.Status, rep.Error.Message) {
		return &TokenRejectedError{Status: resp.StatusCode, Reason: rep.Error.Status}
	}
	return fmt.Errorf("rustore: status %d %s %s", resp.StatusCode, rep.Error.Status, rep.Error.Message)
}

// ruStoreTokenRejected — служба назвала токен недействительным:
// NOT_FOUND (404) — токен истёк или приложение удалено, UNREGISTERED — так же;
// INVALID_ARGUMENT (400) про токен — токен испорчен. Решает поле status
// ответа, а не один HTTP-код: 404 без тела (ошибка в адресе, неверный
// project id) не повод стирать токены всех пользователей. Прочий
// INVALID_ARGUMENT (наш запрос) и PERMISSION_DENIED (сервисный токен) токен
// не трогают.
func ruStoreTokenRejected(status int, code, message string) bool {
	msg := strings.ToLower(message)
	if strings.Contains(msg, "project") {
		return false
	}
	switch code {
	case "NOT_FOUND", "UNREGISTERED":
		return status == http.StatusNotFound || status == http.StatusBadRequest
	case "INVALID_ARGUMENT":
		return status == http.StatusBadRequest && strings.Contains(msg, "token")
	}
	return false
}
