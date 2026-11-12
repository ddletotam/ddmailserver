package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/golang-jwt/jwt/v5"
)

// Адреса APNs (HTTP/2, порт 443).
const (
	APNsProductionURL = "https://api.push.apple.com"
	APNsSandboxURL    = "https://api.sandbox.push.apple.com"
)

// APNsPayload — всё, что Apple узнаёт о письме: ничего. Обезличенный текст
// видимого уведомления; mutable-content будит Notification Service
// Extension, и оно заменяет текст настоящим из кэша устройства
// (ios-behavior-contract.md §7). Отправителя, темы, адресов здесь нет и не
// будет никогда.
const APNsPayload = `{"aps":{"alert":{"title":"ddmail","body":"Новые письма"},"mutable-content":1,"sound":"default","thread-id":"mail"}}`

const (
	// providerTokenTTL — сколько живёт JWT провайдера. Apple принимает токен
	// до часа и просит обновлять не чаще раза в 20 минут.
	providerTokenTTL = 50 * time.Minute
	// minProviderTokenAge — раньше этого токен не перевыпускается даже по
	// ExpiredProviderToken: иначе часы, сбитые относительно Apple, дали бы
	// TooManyProviderTokenUpdates.
	minProviderTokenAge = time.Minute
	// apnsExpiration — сколько APNs хранит push для выключенного устройства.
	apnsExpiration = 24 * time.Hour
)

// APNsOptions — настройки отправителя. Key, KeyID, TeamID, Topic обязательны.
type APNsOptions struct {
	Key    *ecdsa.PrivateKey
	KeyID  string
	TeamID string
	Topic  string
	// Environment — config.APNsEnvAuto (окружение из токена), Sandbox или
	// Production (только токены этого окружения). Пусто — auto.
	Environment string
	// Client — HTTP/2-клиент; nil — свой. Тесты подставляют клиент httptest.
	Client *http.Client
	// Endpoints — адреса по окружениям; nil — настоящие APNs.
	Endpoints map[string]string
	// Now — часы (тесты); nil — time.Now.
	Now func() time.Time
}

// APNs — отправитель Apple Push Notification service с token-based auth
// (JWT ES256 ключом .p8).
type APNs struct {
	key       *ecdsa.PrivateKey
	keyID     string
	teamID    string
	topic     string
	env       string
	client    *http.Client
	endpoints map[string]string
	now       func() time.Time

	mu      sync.Mutex
	jwt     string
	jwtTime time.Time
}

// NewAPNs проверяет настройки и собирает отправителя.
func NewAPNs(o APNsOptions) (*APNs, error) {
	if o.Key == nil || o.KeyID == "" || o.TeamID == "" || o.Topic == "" {
		return nil, fmt.Errorf("apns: key, key id, team id and topic are required")
	}
	env := strings.ToLower(strings.TrimSpace(o.Environment))
	if env == "" {
		env = config.APNsEnvAuto
	}
	switch env {
	case config.APNsEnvAuto, config.APNsEnvSandbox, config.APNsEnvProduction:
	default:
		return nil, fmt.Errorf("apns: unknown environment %q", o.Environment)
	}
	a := &APNs{
		key:       o.Key,
		keyID:     o.KeyID,
		teamID:    o.TeamID,
		topic:     o.Topic,
		env:       env,
		client:    o.Client,
		endpoints: o.Endpoints,
		now:       o.Now,
	}
	if a.client == nil {
		a.client = &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				// Свой TLSClientConfig выключает HTTP/2 по умолчанию —
				// включаем явно: APNs говорит только HTTP/2.
				ForceAttemptHTTP2:   true,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     10 * time.Minute,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		}
	}
	if a.endpoints == nil {
		a.endpoints = map[string]string{
			models.PushEnvProduction: APNsProductionURL,
			models.PushEnvSandbox:    APNsSandboxURL,
		}
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

// APNsFromConfig — отправитель по секции push.apns конфига.
func APNsFromConfig(c config.APNsConfig) (*APNs, error) {
	key, err := LoadAPNsKey(c.KeyFile)
	if err != nil {
		return nil, err
	}
	return NewAPNs(APNsOptions{
		Key:         key,
		KeyID:       c.KeyID,
		TeamID:      c.TeamID,
		Topic:       c.Topic,
		Environment: c.EnvironmentOrDefault(),
	})
}

// LoadAPNsKey читает ключ .p8 (PEM, PKCS#8, P-256).
func LoadAPNsKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("apns: read key: %w", err)
	}
	return ParseAPNsKey(data)
}

// ParseAPNsKey разбирает содержимое .p8.
func ParseAPNsKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("apns: key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: parse key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("apns: key is %T, want ECDSA P-256", k)
	}
	return ec, nil
}

// Kind — служба APNs.
func (a *APNs) Kind() string { return models.PushKindAPNs }

// Accepts — токен нашего приложения и обслуживаемого окружения.
func (a *APNs) Accepts(t models.PushToken) bool {
	if t.Kind != models.PushKindAPNs {
		return false
	}
	if t.Topic != "" && t.Topic != a.topic {
		return false
	}
	env := tokenEnv(t)
	if a.env != config.APNsEnvAuto && env != a.env {
		return false
	}
	_, ok := a.endpoints[env]
	return ok
}

// tokenEnv — окружение токена; не указано — production (так регистрирует
// релизная сборка).
func tokenEnv(t models.PushToken) string {
	if t.Environment == models.PushEnvSandbox {
		return models.PushEnvSandbox
	}
	return models.PushEnvProduction
}

// apnsReply — тело ответа APNs с ошибкой.
type apnsReply struct {
	Reason string `json:"reason"`
}

// Send — один push «есть новые письма». Истёкший токен провайдера
// перевыпускается, и запрос повторяется один раз.
func (a *APNs) Send(ctx context.Context, t models.PushToken) error {
	base, ok := a.endpoints[tokenEnv(t)]
	if !ok {
		return fmt.Errorf("apns: no endpoint for %s", tokenEnv(t))
	}
	url := base + "/3/device/" + t.Token
	for attempt := 0; ; attempt++ {
		bearer, err := a.providerToken(attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(APNsPayload))
		if err != nil {
			return fmt.Errorf("apns: request: %w", err)
		}
		req.Header.Set("authorization", "bearer "+bearer)
		req.Header.Set("apns-push-type", "alert")
		req.Header.Set("apns-topic", a.topic)
		req.Header.Set("apns-priority", "10")
		req.Header.Set("apns-expiration", strconv.FormatInt(a.now().Add(apnsExpiration).Unix(), 10))
		req.Header.Set("content-type", "application/json")

		resp, err := a.client.Do(req)
		if err != nil {
			return fmt.Errorf("apns: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		closeErr := resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("apns: status %d, read body: %w", resp.StatusCode, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("apns: status %d, close body: %w", resp.StatusCode, closeErr)
		}
		var r apnsReply
		if err := json.Unmarshal(body, &r); err != nil {
			r.Reason = strings.TrimSpace(string(body))
		}
		switch {
		case resp.StatusCode == http.StatusGone,
			resp.StatusCode == http.StatusBadRequest && r.Reason == "BadDeviceToken",
			r.Reason == "Unregistered":
			return &TokenRejectedError{Status: resp.StatusCode, Reason: r.Reason}
		case resp.StatusCode == http.StatusForbidden && r.Reason == "ExpiredProviderToken" && attempt == 0:
			continue
		default:
			return fmt.Errorf("apns: status %d %s", resp.StatusCode, r.Reason)
		}
	}
}

// providerToken — JWT провайдера из кэша; force — перевыпустить (APNs
// назвал его истёкшим), если он не совсем свежий.
func (a *APNs) providerToken(force bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	age := now.Sub(a.jwtTime)
	if a.jwt != "" && age < providerTokenTTL && (!force || age < minProviderTokenAge) {
		return a.jwt, nil
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": a.teamID,
		"iat": now.Unix(),
	})
	tok.Header["kid"] = a.keyID
	signed, err := tok.SignedString(a.key)
	if err != nil {
		return "", fmt.Errorf("apns: sign provider token: %w", err)
	}
	a.jwt, a.jwtTime = signed, now
	return signed, nil
}
