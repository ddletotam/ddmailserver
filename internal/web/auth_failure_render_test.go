package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// TestAccountsList_ShowsRejectedPassword: an account whose password the
// provider rejects says so in words, in the user's language, with the time
// it started and the next attempt — not just a red "Error" badge.
func TestAccountsList_ShowsRejectedPassword(t *testing.T) {
	s := &Server{i18nManager: NewI18nManager()}
	acc := &models.Account{
		ID: 1, Name: "Yandex 360", Email: "user@example.org", Enabled: true, SyncMode: "idle",
		AuthFailures: []models.AuthFailureView{{
			Service: "imap", Since: 1_700_000_000_000, NextAttemptAt: 1_700_000_360_000, Failures: 2,
			LastError: "failed to login: LOGIN invalid credentials or IMAP is disabled",
		}},
	}
	for lang, want := range map[string]string{
		"ru": "Пароль не принят провайдером",
		"en": "Password not accepted by the provider",
	} {
		data := AccountsData{
			PageData: PageData{User: &models.User{Username: "u", Language: lang}},
			Accounts: []*models.Account{acc},
		}
		tmpl, err := template.New("").Funcs(s.buildFuncMap(data)).ParseFS(templatesFS, "templates/accounts.html")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "accounts-list", data); err != nil {
			t.Fatalf("%s: execute: %v", lang, err)
		}
		out := buf.String()
		for _, s := range []string{want, "LOGIN invalid credentials or IMAP is disabled", ">imap<"} {
			if !strings.Contains(out, s) {
				t.Errorf("%s: %q missing from the account card", lang, s)
			}
		}
	}
}

// TestSourcesList_ShowsRejectedPassword: the same for calendar and contact
// sources, which have their own credentials.
func TestSourcesList_ShowsRejectedPassword(t *testing.T) {
	s := &Server{i18nManager: NewI18nManager()}
	view := &models.AuthFailureView{Service: "caldav", Since: 1_700_000_000_000, NextAttemptAt: 1_700_003_600_000, LastError: "server rejected the credentials (HTTP 401 Unauthorized)"}
	user := &models.User{Username: "u", Language: "ru"}

	cal := CalendarSourcesListData{PageData: PageData{User: user}, Sources: []*models.CalendarSource{{ID: 3, Name: "Яндекс", SourceType: "caldav", AuthFailure: view}}}
	contacts := ContactSourcesListData{PageData: PageData{User: user}, Sources: []*models.ContactSource{{ID: 4, Name: "Яндекс", SourceType: "carddav", AuthFailure: view}}}

	for _, c := range []struct {
		file, name string
		data       interface{}
	}{
		{"templates/calendars.html", "calendar-sources-list", cal},
		{"templates/contacts.html", "contact-sources-list", contacts},
	} {
		tmpl, err := template.New("").Funcs(s.buildFuncMap(c.data)).ParseFS(templatesFS, c.file)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.file, err)
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, c.name, c.data); err != nil {
			t.Fatalf("%s: execute: %v", c.file, err)
		}
		if !strings.Contains(buf.String(), "Пароль не принят провайдером") || !strings.Contains(buf.String(), "HTTP 401") {
			t.Errorf("%s: rejected password not shown", c.file)
		}
	}
}
