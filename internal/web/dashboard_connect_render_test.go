package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/config"
	"github.com/ddletotam/ddmailserver/internal/models"
)

func renderDashboard(t *testing.T, s *Server, user *models.User) string {
	t.Helper()
	data := DashboardData{
		PageData: PageData{User: user},
		Connect:  s.connectInfo(user),
	}
	tmpl, err := template.New("").Funcs(s.buildFuncMap(map[string]interface{}{"User": user, "Language": "ru"})).
		ParseFS(templatesFS, "templates/layout.html", "templates/dashboard.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}

// The dashboard is where a user setting up a phone looks for host, ports and
// login. The values must come from the public endpoints (what the firewall
// exposes), not the listen ports, and no secret may ever be printed there.
func TestDashboardShowsConnectionSettings(t *testing.T) {
	s := &Server{
		i18nManager:     NewI18nManager(),
		publicEndpoints: config.PublicEndpoints{Hostname: "mail.example.org", IMAPPort: 993, SMTPPort: 465, HTTPSPort: 443},
	}
	user := &models.User{Username: "tester", Email: "tester@example.org", PasswordHash: "$2a$10$secrethash", Language: "ru"}
	out := renderDashboard(t, s, user)

	for _, want := range []string{
		"Подключение к серверу",
		"mail.example.org</code> : 993",
		"mail.example.org</code> : 465",
		">tester<",
		">tester@example.org<",
		"https://mail.example.org/caldav/",
		"https://mail.example.org/carddav/",
		"DAVx⁵",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}
	if strings.Contains(out, "secrethash") {
		t.Error("dashboard leaks the password hash")
	}
	if strings.Contains(out, "ZgotmplZ") {
		t.Error("template refused to interpolate a value")
	}
}

func TestDashboardHidesConnectionCardWithoutHostname(t *testing.T) {
	s := &Server{i18nManager: NewI18nManager()}
	out := renderDashboard(t, s, &models.User{Username: "tester", Language: "ru"})
	if strings.Contains(out, "Подключение к серверу") {
		t.Error("connection card shown with no public hostname configured")
	}
}
