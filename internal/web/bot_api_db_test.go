package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/migrations"
	"github.com/gorilla/mux"
)

const botTestSecret = "0123456789abcdef0123456789abcdef-test"

// newBotEnv: a Server with only what the bot API needs, on the test database
// (MAILSERVER_TEST_DSN), and a fresh sign-up domain.
func newBotEnv(t *testing.T) (*Server, *db.DB, string) {
	t.Helper()
	dsn := os.Getenv("MAILSERVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set MAILSERVER_TEST_DSN to a Postgres DSN to run DB integration tests")
	}
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	database := &db.DB{DB: raw}
	if err := database.RunMigrations(context.Background(), migrations.FS()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	// A first user, so no sign-up below is "the first user ever".
	if _, err := database.CreateUser(fmt.Sprintf("zzbotadmin%d", time.Now().UnixNano()), "x", "", ""); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	domain := fmt.Sprintf("bot%d.test", time.Now().UnixNano())
	if _, err := raw.Exec(`INSERT INTO domains (domain, user_id, enabled, created_at, verified, verify_token)
		VALUES ($1, (SELECT min(id) FROM users), true, 0, true, 'x')`, domain); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	s := &Server{database: database, router: mux.NewRouter()}
	s.registerBotRoutes()
	if err := s.SetBotAPI(botTestSecret, domain, nil); err != nil {
		t.Fatalf("SetBotAPI: %v", err)
	}
	return s, database, domain
}

func botCall(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+botTestSecret)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestBotAPISignUpFlow(t *testing.T) {
	s, database, domain := newBotEnv(t)
	tg := time.Now().UnixNano() % 1_000_000_000_000
	login := fmt.Sprintf("vasya%d", tg%100000)

	if code, _ := botCall(t, s, "GET", fmt.Sprintf("/api/bot/v1/accounts/%d", tg), ""); code != 404 {
		t.Fatalf("unknown tg: %d, want 404", code)
	}
	code, out := botCall(t, s, "GET", "/api/bot/v1/logins/"+strings.ToUpper(login), "")
	if code != 200 || out["available"] != true || out["address"] != login+"@"+domain {
		t.Fatalf("check login: %d %v", code, out)
	}
	if _, out := botCall(t, s, "GET", "/api/bot/v1/logins/postmaster", ""); out["reason"] != "reserved" {
		t.Fatalf("postmaster: %v", out)
	}

	body := fmt.Sprintf(`{"tg_user_id":%d,"login":%q,"language":"ru"}`, tg, login)
	code, out = botCall(t, s, "POST", "/api/bot/v1/accounts", body)
	if code != 201 || out["address"] != login+"@"+domain || len(fmt.Sprint(out["password"])) != botPasswordLen {
		t.Fatalf("create: %d %v", code, out)
	}
	password := out["password"].(string)
	u, err := database.GetUserByUsername(login)
	if err != nil || u.IsAdmin() || !VerifyPassword(u.PasswordHash, password) {
		t.Fatalf("created user: %+v %v", u, err)
	}

	// Mail clients sign in with the login or with the full address.
	for _, name := range []string{login, login + "@" + domain} {
		if _, err := database.AuthenticateProtocol(name, password); err != nil {
			t.Fatalf("AuthenticateProtocol(%q): %v", name, err)
		}
	}

	// The same Telegram user can't take a second free address.
	code, out = botCall(t, s, "POST", "/api/bot/v1/accounts", fmt.Sprintf(`{"tg_user_id":%d,"login":"other%s"}`, tg, login))
	if code != 409 || out["error"] != "registered" {
		t.Fatalf("second account: %d %v", code, out)
	}
	// Another Telegram user can't take the login.
	code, out = botCall(t, s, "POST", "/api/bot/v1/accounts", fmt.Sprintf(`{"tg_user_id":%d,"login":%q}`, tg+1, login))
	if code != 409 || out["error"] != "taken" {
		t.Fatalf("taken login: %d %v", code, out)
	}
	if _, out := botCall(t, s, "GET", "/api/bot/v1/logins/"+login, ""); out["reason"] != "taken" {
		t.Fatalf("check taken: %v", out)
	}

	code, out = botCall(t, s, "GET", fmt.Sprintf("/api/bot/v1/accounts/%d", tg), "")
	if code != 200 || out["login"] != login {
		t.Fatalf("get account: %d %v", code, out)
	}

	code, out = botCall(t, s, "POST", fmt.Sprintf("/api/bot/v1/accounts/%d/password", tg), "")
	if code != 200 || out["password"] == password {
		t.Fatalf("reset: %d %v", code, out)
	}
	u, _ = database.GetUserByUsername(login)
	if !VerifyPassword(u.PasswordHash, out["password"].(string)) {
		t.Fatal("new password does not verify")
	}

	// A banned Telegram id is refused everything.
	if _, err := database.Exec(`UPDATE telegram_links SET banned = true WHERE tg_user_id = $1`, tg); err != nil {
		t.Fatal(err)
	}
	if code, out := botCall(t, s, "GET", fmt.Sprintf("/api/bot/v1/accounts/%d", tg), ""); code != 403 || out["error"] != "banned" {
		t.Fatalf("banned get: %d %v", code, out)
	}
}

func TestBotAPIAuth(t *testing.T) {
	s := &Server{router: mux.NewRouter()}
	s.registerBotRoutes()
	req := httptest.NewRequest("GET", "/api/bot/v1/logins/vasya", nil)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("API off: %d, want 404", rec.Code)
	}
	if err := s.SetBotAPI("short", "lunlun.email", nil); err == nil || s.bot != nil {
		t.Fatal("short secret must keep the API off")
	}
	if err := s.SetBotAPI(botTestSecret, "lunlun.email", nil); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer wrong", "Basic " + botTestSecret} {
		req := httptest.NewRequest("GET", "/api/bot/v1/logins/vasya", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("auth %q: %d, want 401", auth, rec.Code)
		}
	}
}
