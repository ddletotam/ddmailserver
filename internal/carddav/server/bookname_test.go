package server

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// Книга без displayname (Яндекс так отдаёт личную книгу) получает имя
// «Контакты» на языке пользователя, а не пустую строку — в клиентах
// получалось « · AppSec» (аудит 2026-11-14, №7).
func TestBookNameFallback(t *testing.T) {
	s := New(nil, "/carddav/")
	unnamed := &models.AddressBook{Name: "  "}
	named := &models.AddressBook{Name: "Personal"}
	for _, tc := range []struct {
		lang string
		book *models.AddressBook
		want string
	}{
		{"ru", unnamed, "Контакты"},
		{"en", unnamed, "Contacts"},
		{"", unnamed, "Контакты"},
		{"de", unnamed, "Контакты"},
		{"en", named, "Personal"},
	} {
		if got := s.bookName(&models.User{Language: tc.lang}, tc.book); got != tc.want {
			t.Errorf("lang=%q name=%q: %q, ждали %q", tc.lang, tc.book.Name, got, tc.want)
		}
	}
	if got := s.bookName(nil, unnamed); got != "Контакты" {
		t.Errorf("без пользователя: %q", got)
	}
}
