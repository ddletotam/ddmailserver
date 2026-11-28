package client

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
)

type fakeLists struct{ posted []string }

func (f *fakeLists) PostToList(addr string, sender, outboxID int64, subject string, raw []byte) (*db.MailingList, error) {
	switch addr {
	case "news@example.com":
		f.posted = append(f.posted, addr+"|"+subject)
		return &db.MailingList{}, nil
	case "other@example.com":
		return &db.MailingList{}, db.ErrListNotOwner
	}
	return nil, db.ErrListNotFound
}

func TestPostToListsSplitsRecipients(t *testing.T) {
	fl := &fakeLists{}
	task := &DirectSendTask{lists: fl}
	raw := []byte("From: a@b\r\nSubject: =?utf-8?b?0J/RgNC40LLQtdGC?=\r\n\r\nx\r\n")
	rest, delivered, failed, pending := task.postToLists(&models.OutboxMessage{ID: 1, UserID: 2}, raw,
		[]string{"News <news@example.com>", "friend@example.org", "other@example.com"})
	if delivered != 1 || len(fl.posted) != 1 || fl.posted[0] != "news@example.com|Привет" {
		t.Fatalf("posted %v, delivered %d", fl.posted, delivered)
	}
	if len(rest) != 1 || rest[0] != "friend@example.org" {
		t.Fatalf("rest %v", rest)
	}
	if len(failed) != 1 || failed[0].Status != "5.7.1" || len(pending) != 0 {
		t.Fatalf("failed %+v pending %+v", failed, pending)
	}
}
