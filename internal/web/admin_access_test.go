package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/clientip"
	"github.com/ddletotam/ddmailserver/internal/models"
)

func withUser(r *http.Request, u *models.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey, u))
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

// An administrator account has no mail: every user page sends it to /admin,
// the admin pages and logout stay open.
func TestWebAuthMiddlewareSendsAdminToAdmin(t *testing.T) {
	s := &Server{}
	admin := &models.User{ID: 1, Username: "admin", IsAdminFlag: true}
	user := &models.User{ID: 2, Username: "vasya"}
	cases := []struct {
		u        *models.User
		path     string
		wantCode int
		wantLoc  string
	}{
		{admin, "/dashboard", http.StatusSeeOther, "/admin"},
		{admin, "/accounts", http.StatusSeeOther, "/admin"},
		{admin, "/admin", http.StatusTeapot, ""},
		{admin, "/admin/users", http.StatusTeapot, ""},
		{admin, "/logout", http.StatusTeapot, ""},
		{user, "/dashboard", http.StatusTeapot, ""},
		{nil, "/dashboard", http.StatusSeeOther, "/login"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		if c.u != nil {
			r = withUser(r, c.u)
		}
		w := httptest.NewRecorder()
		s.WebAuthMiddleware(okHandler).ServeHTTP(w, r)
		if w.Code != c.wantCode || w.Header().Get("Location") != c.wantLoc {
			t.Errorf("%v %s: got %d %q, want %d %q", c.u != nil && c.u.IsAdmin(), c.path, w.Code, w.Header().Get("Location"), c.wantCode, c.wantLoc)
		}
	}
}

func TestAPIAuthMiddlewareLimitsAdmin(t *testing.T) {
	s := &Server{}
	admin := &models.User{ID: 1, Username: "admin", IsAdminFlag: true}
	for path, want := range map[string]int{
		"/api/accounts":               http.StatusForbidden,
		"/api/settings/app-passwords": http.StatusForbidden,
		"/api/settings/account":       http.StatusForbidden,
		"/api/settings/password":      http.StatusTeapot,
		"/api/settings/oauth/google":  http.StatusTeapot,
		"/api/admin/users/5/ban":      http.StatusTeapot,
		"/api/administrator":          http.StatusForbidden,
	} {
		w := httptest.NewRecorder()
		s.APIAuthMiddleware(okHandler).ServeHTTP(w, withUser(httptest.NewRequest("POST", path, nil), admin))
		if w.Code != want {
			t.Errorf("%s: got %d, want %d", path, w.Code, want)
		}
	}
}

func TestAdminNetworks(t *testing.T) {
	resolver, err := clientip.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{clientIP: resolver}
	if err := s.SetAdminNetworks([]string{"10.0.0.0/8", "192.0.2.7", "bogus"}); err == nil {
		t.Fatal("bogus entry not reported")
	}
	admin := &models.User{ID: 1, Username: "admin", IsAdminFlag: true}
	for addr, want := range map[string]int{
		"10.1.2.3:5000":    http.StatusTeapot,
		"192.0.2.7:5000":   http.StatusTeapot,
		"192.0.2.8:5000":   http.StatusForbidden,
		"203.0.113.1:5000": http.StatusForbidden,
	} {
		r := withUser(httptest.NewRequest("GET", "/api/admin/users", nil), admin)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		s.APIAdminMiddleware(okHandler).ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: got %d, want %d", addr, w.Code, want)
		}
	}
	// Empty list — from anywhere.
	if err := s.SetAdminNetworks(nil); err != nil {
		t.Fatal(err)
	}
	r := withUser(httptest.NewRequest("GET", "/api/admin/users", nil), admin)
	r.RemoteAddr = "203.0.113.1:5000"
	w := httptest.NewRecorder()
	s.APIAdminMiddleware(okHandler).ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("no allowlist: got %d", w.Code)
	}
	// A user never reaches the admin API.
	r = withUser(httptest.NewRequest("GET", "/api/admin/users", nil), &models.User{ID: 2, Username: "vasya"})
	w = httptest.NewRecorder()
	s.APIAdminMiddleware(okHandler).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("user on admin API: got %d", w.Code)
	}
}

// Without an administrator's setting the config decides.
func TestRegistrationModeFallsBackToConfig(t *testing.T) {
	if m := (&Server{}).registrationMode(); m != "open" {
		t.Errorf("default: %q, want open", m)
	}
	if m := (&Server{registrationDisabled: true}).registrationMode(); m != "closed" {
		t.Errorf("disable_registration: %q, want closed", m)
	}
}

// A pending sign-up is blocked like a ban until approved.
func TestPendingUserIsBlocked(t *testing.T) {
	u := &models.User{PendingFlag: true}
	if !u.Blocked() || u.IsBanned() {
		t.Fatalf("pending: Blocked=%v IsBanned=%v", u.Blocked(), u.IsBanned())
	}
	s := &Server{}
	r := withUser(httptest.NewRequest("GET", "/accounts", nil), &models.User{ID: 3, Username: "p"})
	w := httptest.NewRecorder()
	s.WebAuthMiddleware(okHandler).ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Fatalf("approved user: %d", w.Code)
	}
}
