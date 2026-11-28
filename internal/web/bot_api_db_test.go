package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/maillink"
	"github.com/ddletotam/ddmailserver/internal/models"
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
	m := NewI18nManager()
	s := &Server{database: database, router: mux.NewRouter(), i18nManager: m, i18n: m.Get("ru")}
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
	_, _ = database.Exec(`DELETE FROM telegram_links WHERE tg_user_id = $1`, tg)
}

func TestBotAPISignupCeiling(t *testing.T) {
	s, database, _ := newBotEnv(t)
	base := 9_000_000_000_000 + time.Now().UnixNano()%1_000_000_000
	t.Cleanup(func() {
		_, _ = database.Exec(`DELETE FROM telegram_links WHERE tg_user_id >= $1 AND tg_user_id < $2`, base, base+1000)
	})
	var recent int
	_ = database.QueryRow(`SELECT COUNT(*) FROM telegram_links WHERE created_at > $1`, time.Now().Add(-time.Hour).UnixMilli()).Scan(&recent)
	for i := recent; i < db.SignupsPerHour; i++ {
		if _, err := database.Exec(`INSERT INTO telegram_links (tg_user_id, terms_accepted_at, created_at) VALUES ($1, 0, $2)`,
			base+int64(i), time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	code, out := botCall(t, s, "POST", "/api/bot/v1/accounts", fmt.Sprintf(`{"tg_user_id":%d,"login":"ceiling%d"}`, base+999, base%100000))
	if code != 429 || out["error"] != "busy" {
		t.Fatalf("over the ceiling: %d %v", code, out)
	}
}

func TestBotAPINotices(t *testing.T) {
	s, database, _ := newBotEnv(t)
	var id int64
	if err := database.QueryRow(`INSERT INTO bot_notices (tg_user_id, kind, text, created_at) VALUES (77, 'test', 'привет', 0) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	code, out := botCall(t, s, "GET", "/api/bot/v1/notices", "")
	if code != 200 || !strings.Contains(fmt.Sprint(out["notices"]), "привет") {
		t.Fatalf("notices: %d %v", code, out)
	}
	if code, _ := botCall(t, s, "POST", fmt.Sprintf("/api/bot/v1/notices/%d/sent", id), ""); code != 204 {
		t.Fatalf("mark sent: %d", code)
	}
	if _, out := botCall(t, s, "GET", "/api/bot/v1/notices", ""); strings.Contains(fmt.Sprint(out["notices"]), "привет") {
		t.Fatal("a sent notice is still pending")
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

func TestBotAPIBalance(t *testing.T) {
	s, database, _ := newBotEnv(t)
	s.SetBotPrices(map[string]int{db.FeatureExternalAccounts: 50, db.FeatureSendLimits: 150, db.FeatureMailbox: 25})
	tg := 8_000_000_000_000 + time.Now().UnixNano()%1_000_000_000
	login := fmt.Sprintf("payer%d", tg%100000)
	if code, out := botCall(t, s, "POST", "/api/bot/v1/accounts", fmt.Sprintf(`{"tg_user_id":%d,"login":%q}`, tg, login)); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	t.Cleanup(func() { _, _ = database.Exec(`DELETE FROM telegram_links WHERE tg_user_id = $1`, tg) })
	u, _ := database.GetUserByUsername(login)
	acc := &models.Account{UserID: u.ID, Name: "x", Email: "x@example.com", IMAPHost: "imap.example.com", IMAPPort: 993,
		IMAPUsername: "x", IMAPPassword: "p", SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPUsername: "x", SMTPPassword: "p", Enabled: true}
	if err := database.CreateAccount(acc); !errors.Is(err, db.ErrAccountLimit) {
		t.Fatalf("unpaid account: %v", err)
	}
	feat := fmt.Sprintf("/api/bot/v1/accounts/%d/features/external_accounts", tg)

	// Nothing on the balance: 402.
	if code, out := botCall(t, s, "POST", feat, ""); code != 402 || out["error"] != "insufficient" {
		t.Fatalf("enable with no balance: %d %v", code, out)
	}
	top := fmt.Sprintf(`{"tg_user_id":%d,"amount":100,"charge_id":"top-%d"}`, tg, tg)
	if code, out := botCall(t, s, "POST", "/api/bot/v1/topups", top); code != 200 || out["balance"] != float64(100) {
		t.Fatalf("top-up: %d %v", code, out)
	}
	if _, out := botCall(t, s, "POST", "/api/bot/v1/topups", top); out["balance"] != float64(100) {
		t.Fatalf("the same top-up twice: %v", out)
	}
	// Refundable while untouched.
	if _, out := botCall(t, s, "GET", fmt.Sprintf("/api/bot/v1/accounts/%d/refunds", tg), ""); !strings.Contains(fmt.Sprint(out["topups"]), "top-") {
		t.Fatalf("refundable before spending: %v", out)
	}
	if code, out := botCall(t, s, "POST", feat, ""); code != 200 || out["balance"] != float64(50) {
		t.Fatalf("enable: %d %v", code, out)
	}
	if err := database.CreateAccount(acc); err != nil {
		t.Fatalf("paid account: %v", err)
	}
	// Spent from: no longer refundable.
	if code, _ := botCall(t, s, "POST", fmt.Sprintf("/api/bot/v1/accounts/%d/refunds/top-%d", tg, tg), ""); code != 409 {
		t.Fatalf("refund of a spent top-up: %d", code)
	}
	code, out := botCall(t, s, "GET", fmt.Sprintf("/api/bot/v1/accounts/%d/billing", tg), "")
	if code != 200 || out["balance"] != float64(50) || !strings.Contains(fmt.Sprint(out["features"]), "working:true") {
		t.Fatalf("billing: %d %v", code, out)
	}
	if code, out := botCall(t, s, "DELETE", feat, ""); code != 200 || out["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if code, _ := botCall(t, s, "POST", "/api/bot/v1/accounts/"+fmt.Sprint(tg)+"/features/mailbox", ""); code != 400 {
		t.Fatalf("mailbox through the bot: %d", code)
	}
	_, _ = database.Exec(`DELETE FROM accounts WHERE user_id = $1`, u.ID)
}

// The cabinet: a free domain, paid mailboxes on it and on the service domain.
func TestMyDomainsAndMailboxes(t *testing.T) {
	s, database, domain := newBotEnv(t)
	s.SetBotPrices(map[string]int{db.FeatureMailbox: 25})
	tg := 7_000_000_000_000 + time.Now().UnixNano()%1_000_000_000
	login := fmt.Sprintf("owner%d", tg%100000)
	if code, out := botCall(t, s, "POST", "/api/bot/v1/accounts", fmt.Sprintf(`{"tg_user_id":%d,"login":%q}`, tg, login)); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	t.Cleanup(func() { _, _ = database.Exec(`DELETE FROM telegram_links WHERE tg_user_id = $1`, tg) })
	u, _ := database.GetUserByUsername(login)
	s.router.HandleFunc("/domains/add", s.HandleMyAddDomain).Methods("POST")
	s.router.HandleFunc("/domains/{id:[0-9]+}/verify", s.HandleMyVerifyDomain).Methods("POST")
	s.router.HandleFunc("/mailboxes/add", s.HandleMyAddMailbox).Methods("POST")
	s.router.HandleFunc("/mailboxes/{id:[0-9]+}/delete", s.HandleMyDeleteMailbox).Methods("POST")
	call := func(path string, form url.Values) (int, map[string]any) {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, u))
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	own := fmt.Sprintf("own%d.test", tg%100000)
	code, out := call("/domains/add", url.Values{"domain": {own}})
	if code != 201 {
		t.Fatalf("add domain: %d %v", code, out)
	}
	domID := int64(out["id"].(float64))
	t.Cleanup(func() { _, _ = database.Exec(`DELETE FROM domains WHERE id = $1`, domID) })
	if code, _ := call("/domains/add", url.Values{"domain": {domain}}); code != 400 {
		t.Fatalf("the service domain as own: %d", code)
	}
	d, _ := database.GetDomainByID(domID)
	s.lookupTXT = func(context.Context, string) ([]string, error) { return []string{"nothing"}, nil }
	if code, _ := call(fmt.Sprintf("/domains/%d/verify", domID), nil); code != 409 {
		t.Fatalf("verify without the record: %d", code)
	}
	s.lookupTXT = func(context.Context, string) ([]string, error) {
		return []string{"ddmail-verify=" + d.VerifyToken}, nil
	}
	if code, out := call(fmt.Sprintf("/domains/%d/verify", domID), nil); code != 200 {
		t.Fatalf("verify: %d %v", code, out)
	}
	if k, err := database.DKIMSigner(own); err != nil || k == nil {
		t.Fatalf("dkim key of the new domain: %v %v", k, err)
	}

	// No Stars: no mailbox.
	if code, _ := call("/mailboxes/add", url.Values{"domain_id": {fmt.Sprint(domID)}, "local_part": {"info"}}); code != 402 {
		t.Fatalf("mailbox with an empty balance: %d", code)
	}
	if _, err := database.AddTopUp(u.ID, 60, fmt.Sprintf("my-%d", tg)); err != nil {
		t.Fatal(err)
	}
	code, out = call("/mailboxes/add", url.Values{"domain_id": {fmt.Sprint(domID)}, "local_part": {"info"}, "kind": {"separate"}})
	if code != 201 || out["password"] == "" {
		t.Fatalf("separate mailbox: %d %v", code, out)
	}
	if _, err := database.AuthenticateProtocol("info@"+own, out["password"].(string)); err != nil {
		t.Fatalf("sign-in with the separate mailbox: %v", err)
	}
	svc, _ := database.GetDomainByName(domain)
	code, out = call("/mailboxes/add", url.Values{"domain_id": {fmt.Sprint(svc.ID)}, "local_part": {"second" + login}})
	if code != 201 {
		t.Fatalf("extra address on the service domain: %d %v", code, out)
	}
	if b, _ := database.Balance(u.ID); b != 10 {
		t.Fatalf("balance after two mailboxes = %d, want 10", b)
	}
	if code, _ := call("/mailboxes/add", url.Values{"domain_id": {fmt.Sprint(domID)}, "local_part": {"third"}}); code != 402 {
		t.Fatalf("third mailbox with 10 Stars: %d", code)
	}
	// The separate mailbox's sending counts against the owner's limits.
	mb, _ := database.GetMailbox(domID, "info")
	if owner, err := database.OutboundClass(mb.PrincipalID); err != nil || owner != db.OutboundPaid {
		t.Fatalf("class of the separate mailbox = %q, %v", owner, err)
	}
	if code, out := call(fmt.Sprintf("/mailboxes/%d/delete", mb.ID), nil); code != 200 {
		t.Fatalf("delete mailbox: %d %v", code, out)
	}
	// The cabinet pages render with what was just made.
	for _, page := range []string{"/domains", "/billing"} {
		req := httptest.NewRequest("GET", page, nil)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, u))
		rec := httptest.NewRecorder()
		if page == "/domains" {
			s.HandleMyDomainsPage(rec, req)
		} else {
			s.HandleMyBillingPage(rec, req)
		}
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "⭐") {
			t.Fatalf("%s: %d %.300s", page, rec.Code, rec.Body.String())
		}
	}
	_, _ = database.Exec(`DELETE FROM mailboxes WHERE user_id = $1`, u.ID)
}

// The link with lunlun.io: an account there on sign-up, the same password on
// reset, space for lunlun, sign-in from lunlun without a password.
func TestLunlunLink(t *testing.T) {
	s, database, domain := newBotEnv(t)
	secret := []byte("0123456789abcdef0123456789abcdef-link")
	var created, passwords []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+string(secret) {
			http.Error(w, "no", 401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.URL.Path == "/internal/v1/users":
			created = append(created, fmt.Sprint(body["login"], "/", body["password"]))
			_, _ = w.Write([]byte(`{"id":501}`))
		case strings.HasSuffix(r.URL.Path, "/password"):
			passwords = append(passwords, fmt.Sprint(body["password"]))
			w.WriteHeader(204)
		case strings.HasPrefix(r.URL.Path, "/internal/v1/usage/"):
			_, _ = w.Write([]byte(`{"files_bytes":2000000000}`))
		}
	}))
	defer fake.Close()
	ll := &maillink.Lunlun{Base: fake.URL, Secret: secret, SSO: "https://lunlun.example/sso"}
	s.SetLunlun(ll)
	s.registerLunlunRoutes()
	database.SetFilesBytes(func(u int64) int64 { n, _ := ll.FilesBytes(context.Background(), u); return n })
	defer database.SetFilesBytes(nil)

	tg := 6_000_000_000_000 + time.Now().UnixNano()%1_000_000_000
	login := fmt.Sprintf("files%d", tg%100000)
	code, out := botCall(t, s, "POST", "/api/bot/v1/accounts", fmt.Sprintf(`{"tg_user_id":%d,"login":%q}`, tg, login))
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	t.Cleanup(func() { _, _ = database.Exec(`DELETE FROM telegram_links WHERE tg_user_id = $1`, tg) })
	if len(created) != 1 || created[0] != login+"@"+domain+"/"+out["password"].(string) {
		t.Fatalf("lunlun account: %v", created)
	}
	u, _ := database.GetUserByUsername(login)
	if ok, _ := database.HasLunlunLink(u.ID); !ok {
		t.Fatal("link not recorded")
	}
	_, out = botCall(t, s, "POST", fmt.Sprintf("/api/bot/v1/accounts/%d/password", tg), "")
	if len(passwords) != 1 || passwords[0] != out["password"] {
		t.Fatalf("lunlun password on reset: %v", passwords)
	}

	// lunlun asks about space; through nginx — not visible.
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/link/v1/space/%d", u.ID), nil)
	req.Header.Set("Authorization", "Bearer "+string(secret))
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"quota_bytes":1073741824`) {
		t.Fatalf("space: %d %s", rec.Code, rec.Body.String())
	}
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	rec = httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("space through nginx: %d", rec.Code)
	}
	// 2 GB of files fill the 1 GB space: mail is refused.
	if full, err := database.FreeTierOverQuota(u.ID); err != nil || !full {
		t.Fatalf("over quota with files: %v %v", full, err)
	}

	// From lunlun.io into the cabinet with a ticket; once only.
	tok := maillink.Sign(secret, u.ID, time.Now())
	for i, want := range []string{"/domains", "/login"} {
		rec = httptest.NewRecorder()
		s.router.ServeHTTP(rec, httptest.NewRequest("GET", "/sso?t="+tok, nil))
		if loc := rec.Header().Get("Location"); loc != want {
			t.Fatalf("sso try %d: %d → %q, want %q", i, rec.Code, loc, want)
		}
	}
}
