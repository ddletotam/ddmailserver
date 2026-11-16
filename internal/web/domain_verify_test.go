package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidDomainName(t *testing.T) {
	good := []string{"example.com", "mail.letotam.ru", "a-b.co", "xn--80ak6aa92e.xn--p1ai", "lunlun.io"}
	bad := []string{"", "com", "a.b", "localhost", "127.0.0.1", "exa mple.com", "-a.com", "a-.com",
		"a..com", ".a.com", "example.123", "ex_ample.com", "пример.рф", strings.Repeat("a", 64) + ".com"}
	for _, d := range good {
		if !validDomainName(d) {
			t.Errorf("validDomainName(%q) = false", d)
		}
	}
	for _, d := range bad {
		if validDomainName(d) {
			t.Errorf("validDomainName(%q) = true", d)
		}
	}
}

func TestRegisterDisabled(t *testing.T) {
	i18n := NewI18nManager()
	s := &Server{i18nManager: i18n, i18n: i18n.Get("en")}
	s.SetRegistrationEnabled(false)
	req := httptest.NewRequest("POST", "/api/register",
		strings.NewReader(`{"username":"u","password":"p","password_confirm":"p"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.HandleRegister(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("register with registration disabled: %d %s", rec.Code, rec.Body.String())
	}

	for _, enabled := range []bool{true, false} {
		s.SetRegistrationEnabled(enabled)
		rec := httptest.NewRecorder()
		s.HandleLoginPage(rec, httptest.NewRequest("GET", "/login", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("login page: %d", rec.Code)
		}
		if got := strings.Contains(rec.Body.String(), `id="register-tab"`); got != enabled {
			t.Errorf("registration enabled=%v: register tab shown=%v", enabled, got)
		}
	}
}
