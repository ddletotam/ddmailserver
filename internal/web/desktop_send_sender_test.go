package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
)

type fakeSenderStore struct {
	accounts  []*models.Account
	mailboxes []*db.MailboxWithDomain
}

func (f *fakeSenderStore) GetAccountsByUserID(int64) ([]*models.Account, error) {
	return f.accounts, nil
}

func (f *fakeSenderStore) GetMailboxesWithDomainByUserID(int64) ([]*db.MailboxWithDomain, error) {
	return f.mailboxes, nil
}

func desktopSend(t *testing.T, body map[string]interface{}) int {
	t.Helper()
	m := &db.MailboxWithDomain{DomainName: "local.test"}
	m.UserID, m.LocalPart, m.Enabled = 7, "me", true
	s := &Server{senders: &fakeSenderStore{
		accounts:  []*models.Account{{ID: 11, UserID: 7, Email: "ext@gmail.test", Enabled: true}},
		mailboxes: []*db.MailboxWithDomain{m},
	}}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/desktop/v1/send", bytes.NewReader(raw))
	req = req.WithContext(setUserContext(req.Context(), &models.User{ID: 7, Username: "alice"}))
	rec := httptest.NewRecorder()
	s.HandleDesktopSend(rec, req)
	return rec.Code
}

// Rejections happen before anything is queued: the server has no database,
// so reaching CreateOutboxMessage would panic.
func TestDesktopSendRejectsForeignSender(t *testing.T) {
	cases := []struct {
		name string
		body map[string]interface{}
		want int
	}{
		{"other address on our local domain", map[string]interface{}{"from": "CEO <ceo@local.test>", "to": []string{"bob@example.com"}}, http.StatusForbidden},
		{"unrelated domain", map[string]interface{}{"from": "someone@bank.test", "to": []string{"bob@example.com"}}, http.StatusForbidden},
		{"two senders", map[string]interface{}{"from": "me@local.test, ceo@local.test", "to": []string{"bob@example.com"}}, http.StatusBadRequest},
		{"empty sender", map[string]interface{}{"from": "", "to": []string{"bob@example.com"}}, http.StatusBadRequest},
		{"header injection via from", map[string]interface{}{"from": "me@local.test\r\nFrom: ceo@local.test", "to": []string{"bob@example.com"}}, http.StatusBadRequest},
		{"header injection via to", map[string]interface{}{"from": "me@local.test", "to": []string{"bob@example.com\r\nFrom: ceo@local.test"}}, http.StatusBadRequest},
		{"header injection via references", map[string]interface{}{"from": "me@local.test", "to": []string{"b@x.test"}, "references": "<a@b>\nSender: ceo@local.test"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := desktopSend(t, c.body); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
