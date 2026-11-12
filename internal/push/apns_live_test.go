package push

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
)

// TestLiveAPNs — настоящий push через Apple на устройство или симулятор.
// Пропускается без окружения:
//
//	APNS_KEY_FILE, APNS_KEY_ID, APNS_TEAM_ID — ключ .p8 (как в apns.env);
//	DDMAIL_APNS_TOKEN — токен устройства (hex, из журнала приложения
//	«push: device token …» или из таблицы push_tokens);
//	DDMAIL_APNS_ENV — sandbox (отладочная сборка, по умолчанию) или production;
//	DDMAIL_APNS_TOPIC — bundle id (по умолчанию ru.letotam.ddmail).
func TestLiveAPNs(t *testing.T) {
	token := os.Getenv("DDMAIL_APNS_TOKEN")
	if token == "" || os.Getenv("APNS_KEY_FILE") == "" {
		t.Skip("set APNS_KEY_FILE, APNS_KEY_ID, APNS_TEAM_ID and DDMAIL_APNS_TOKEN to send a real push")
	}
	env := os.Getenv("DDMAIL_APNS_ENV")
	if env == "" {
		env = models.PushEnvSandbox
	}
	topic := os.Getenv("DDMAIL_APNS_TOPIC")
	if topic == "" {
		topic = "ru.letotam.ddmail"
	}
	a, err := APNsFromConfig(config.APNsConfig{
		KeyFile: os.Getenv("APNS_KEY_FILE"),
		KeyID:   os.Getenv("APNS_KEY_ID"),
		TeamID:  os.Getenv("APNS_TEAM_ID"),
		Topic:   topic,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Send(ctx, models.PushToken{Kind: models.PushKindAPNs, Token: token, Environment: env, Topic: topic}); err != nil {
		t.Fatalf("send: %v", err)
	}
	t.Logf("push accepted by APNs (%s)", env)
}
