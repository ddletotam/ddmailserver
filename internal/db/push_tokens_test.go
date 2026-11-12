package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// TestPushTokens: регистрация идемпотентна, выход удаляет только свою
// строку, отвергнутый службой токен уходит у всех, лишние устройства
// вытесняются самыми старыми.
func TestPushTokens(t *testing.T) {
	db := requireTestDB(t)
	ctx := context.Background()
	newUser := func(tag string) int64 {
		t.Helper()
		u, err := db.CreateUser(fmt.Sprintf("zzpush%s%d", tag, time.Now().UnixNano()), "x", "", "")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() {
			for _, q := range []string{`DELETE FROM push_tokens WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
				if _, err := db.DB.Exec(q, u.ID); err != nil {
					t.Logf("cleanup %q: %v", q, err)
				}
			}
		})
		return u.ID
	}
	alice, bob := newUser("a"), newUser("b")
	shared := models.PushToken{Kind: models.PushKindAPNs, Token: "aabbccddeeff00112233445566778899", Environment: models.PushEnvSandbox, Topic: "ru.letotam.ddmail"}

	for _, u := range []int64{alice, alice, bob} {
		tok := shared
		tok.UserID = u
		if err := db.UpsertPushToken(ctx, tok); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	list, err := db.PushTokensForUser(ctx, alice)
	if err != nil || len(list) != 1 {
		t.Fatalf("alice tokens: %v %v", list, err)
	}
	if got := list[0]; got.Environment != models.PushEnvSandbox || got.Topic != "ru.letotam.ddmail" || got.CreatedAt == 0 {
		t.Errorf("stored %+v", got)
	}

	// Повтор с другим окружением обновляет строку, а не добавляет вторую.
	tok := shared
	tok.UserID, tok.Environment = alice, models.PushEnvProduction
	if err := db.UpsertPushToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.PushTokensForUser(ctx, alice); len(list) != 1 || list[0].Environment != models.PushEnvProduction {
		t.Errorf("after re-register: %+v", list)
	}

	// Выход Алисы не трогает Боба.
	if ok, err := db.DeleteUserPushToken(ctx, alice, shared.Kind, shared.Token); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if ok, err := db.DeleteUserPushToken(ctx, alice, shared.Kind, shared.Token); err != nil || ok {
		t.Fatalf("repeat delete: ok=%v err=%v", ok, err)
	}
	if list, _ := db.PushTokensForUser(ctx, bob); len(list) != 1 {
		t.Fatalf("bob lost his token: %+v", list)
	}
	if err := db.MarkPushTokenSent(ctx, mustFirst(t, db, bob).ID); err != nil {
		t.Fatal(err)
	}

	// Отвергнутый APNs токен — прочь у всех.
	tok.UserID = alice
	if err := db.UpsertPushToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePushToken(ctx, shared.Kind, shared.Token); err != nil {
		t.Fatal(err)
	}
	for _, u := range []int64{alice, bob} {
		if list, _ := db.PushTokensForUser(ctx, u); len(list) != 0 {
			t.Errorf("user %d still has rejected token: %+v", u, list)
		}
	}

	// Сверх лимита вытесняются самые давно обновлённые.
	for i := 0; i < MaxPushTokensPerUser+3; i++ {
		tok := models.PushToken{UserID: bob, Kind: models.PushKindFCM, Token: fmt.Sprintf("fcm-%02d", i)}
		if err := db.UpsertPushToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // updated_at в миллисекундах
	}
	list, err = db.PushTokensForUser(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != MaxPushTokensPerUser {
		t.Fatalf("bob has %d tokens, want %d", len(list), MaxPushTokensPerUser)
	}
	for _, l := range list {
		if l.Token == "fcm-00" || l.Token == "fcm-01" || l.Token == "fcm-02" {
			t.Errorf("oldest token %s kept", l.Token)
		}
	}
}

func mustFirst(t *testing.T, db *DB, user int64) models.PushToken {
	t.Helper()
	list, err := db.PushTokensForUser(context.Background(), user)
	if err != nil || len(list) == 0 {
		t.Fatalf("tokens of %d: %v %v", user, list, err)
	}
	return list[0]
}
