package web

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/models"
)

// Группировка вынесена из HandleDesktopConversations, чтобы спам складывался
// по тем же правилам. Проверяем, что вынос не поменял ключ беседы: письмо
// без нашего адреса в заголовках (типично для спама) попадает в ту же
// беседу, что и наш ответ.
func TestGroupConversationsKeysBccIntoOwnIdentity(t *testing.T) {
	cc := convContext{
		identities:       map[string]bool{"me@a.ru": true},
		sortedIdentities: []string{"me@a.ru"},
		accountEmail:     map[int64]string{1: "me@a.ru"},
		folderNames:      map[int64]string{10: "INBOX", 11: "Sent"},
		folderAccount:    map[int64]int64{10: 1, 11: 1},
		sentFolderIDs:    map[int64]bool{11: true},
	}
	msgs := []*models.Message{
		{ID: 1, AccountID: 1, FolderID: 10, MessageID: "<1@x>", From: "Bob <bob@x.ru>", To: "list@x.ru", Date: 1000},
		{ID: 2, AccountID: 1, FolderID: 10, MessageID: "<2@x>", From: "bob@x.ru", To: "me@a.ru", Date: 2000, Seen: true},
	}
	convs := groupConversations(msgs, cc)
	ids := map[string]int{}
	for _, c := range convs {
		ids[c.ID] = c.TotalCount
	}
	if ids["me@a.ru|bob@x.ru"] != 1 {
		t.Fatalf("1:1 беседа с bob не собрана: %v", ids)
	}
	if len(convs) != 2 {
		t.Fatalf("ожидали 2 беседы (с list@ — группа), получили %v", ids)
	}
	for _, c := range convs {
		if c.Spam {
			t.Fatalf("groupConversations сам не ставит spam: %+v", c)
		}
	}
}
