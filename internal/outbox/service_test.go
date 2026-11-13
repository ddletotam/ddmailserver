package outbox

import (
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
)

// memStore is an in-memory Store: one outbox row, one user's messages.
type memStore struct {
	preparedRaw []byte
	preparedID  string
	messages    map[string]*models.Message // by Message-ID
	nextID      int64
}

func newMemStore() *memStore { return &memStore{messages: map[string]*models.Message{}} }

func (m *memStore) SetOutboxPrepared(_ int64, raw []byte, id string) ([]byte, string, bool, error) {
	if m.preparedID != "" {
		return m.preparedRaw, m.preparedID, false, nil
	}
	m.preparedRaw, m.preparedID = raw, id
	return raw, id, true, nil
}

func (m *memStore) GetOutboxAttachmentsByMessageID(int64) ([]*models.OutboxAttachment, error) {
	return []*models.OutboxAttachment{{Filename: "a.txt", ContentType: "text/plain", Data: []byte("file")}}, nil
}

func (m *memStore) GetOrCreateLocalFolder(userID int64, name, folderType string) (*models.Folder, error) {
	return &models.Folder{ID: 5, UserID: userID, Name: name, Type: folderType}, nil
}

func (m *memStore) GetNextUIDForFolder(int64) (uint32, error) {
	return uint32(len(m.messages) + 1), nil
}

func (m *memStore) CreateMessage(msg *models.Message) error {
	if _, ok := m.messages[msg.MessageID]; ok {
		return db.ErrDuplicateMessage
	}
	m.nextID++
	msg.ID = m.nextID
	m.messages[msg.MessageID] = msg
	return nil
}

func (m *memStore) CreateAttachment(*models.Attachment) error     { return nil }
func (m *memStore) UpdateMessageAttachmentCount(int64, int) error { return nil }
func (m *memStore) StartMessageDelivery(_ int64, id string) (int64, bool, error) {
	msg, ok := m.messages[id]
	if !ok {
		return 0, false, nil
	}
	if msg.DeliveryStatus != "" {
		return msg.ID, false, nil
	}
	msg.DeliveryStatus = StatusPending
	return msg.ID, true, nil
}

func (m *memStore) SetMessageDelivery(_ int64, id string, d db.MessageDelivery) (int64, bool, error) {
	msg, ok := m.messages[id]
	if !ok {
		return 0, false, nil
	}
	if msg.DeliveryStatus == d.Status && msg.DeliveryReason == d.Reason && msg.DeliveryFailed == d.Failed {
		return msg.ID, false, nil
	}
	msg.DeliveryStatus, msg.DeliveryReason, msg.DeliveryFailed = d.Status, d.Reason, d.Failed
	return msg.ID, true, nil
}

func formRow() *models.OutboxMessage {
	return &models.OutboxMessage{ID: 1, UserID: 7, From: "Me <me@ours.test>", To: "bob@remote.test", Bcc: "hidden@remote.test",
		Subject: "Привет", Body: "текст"}
}

// A web-form row (no wire form) is composed once, with its attachments; the
// copy goes into Sent «pending» and the clients hear message_sent.
func TestAcceptComposesOnceAndSavesPendingCopy(t *testing.T) {
	store := newMemStore()
	hub := notify.NewHub()
	events := hub.Subscribe(7)
	s := New(store, hub)
	s.now = func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }

	msg := formRow()
	if err := s.Accept(msg); err != nil {
		t.Fatal(err)
	}
	if msg.MessageID == "" || !strings.Contains(string(msg.RawEmail), "a.txt") {
		t.Fatalf("not prepared: id %q", msg.MessageID)
	}
	copyMsg := store.messages[msg.MessageID]
	if copyMsg == nil || copyMsg.DeliveryStatus != StatusPending || copyMsg.FolderID != 5 || !copyMsg.Seen {
		t.Fatalf("copy %+v", copyMsg)
	}
	if copyMsg.Bcc != "hidden@remote.test" {
		t.Fatalf("the sender's copy must show Bcc, got %q", copyMsg.Bcc)
	}
	select {
	case ev := <-events:
		if ev.Type != notify.EventMessageSent || ev.RFCMessageID != msg.MessageID || ev.MessageID != copyMsg.ID {
			t.Fatalf("event %+v", ev)
		}
	default:
		t.Fatal("no message_sent")
	}

	// A second preparer (a send task racing the handler) gets the stored
	// version, not a new Message-ID.
	other := formRow()
	fresh, err := s.Prepare(other)
	if err != nil {
		t.Fatal(err)
	}
	if fresh || other.MessageID != msg.MessageID {
		t.Fatalf("second Prepare: fresh %v, id %q, want the stored %q", fresh, other.MessageID, msg.MessageID)
	}
}

// Accepting a message whose copy is already there (the client APPENDed it
// first, or a retried acceptance) keeps one copy and never resets a final
// status.
func TestSaveSentCopyIsIdempotent(t *testing.T) {
	store := newMemStore()
	s := New(store, nil)
	msg := formRow()
	if err := s.Accept(msg); err != nil {
		t.Fatal(err)
	}
	s.Record(msg, Delivery{Status: StatusDelivered})
	if err := s.SaveSentCopy(msg); err != nil {
		t.Fatal(err)
	}
	if len(store.messages) != 1 || store.messages[msg.MessageID].DeliveryStatus != StatusDelivered {
		t.Fatalf("messages %d, status %q", len(store.messages), store.messages[msg.MessageID].DeliveryStatus)
	}
}

// Record announces changes only.
func TestRecordPublishesOnChange(t *testing.T) {
	store := newMemStore()
	hub := notify.NewHub()
	s := New(store, hub)
	msg := formRow()
	if err := s.Accept(msg); err != nil {
		t.Fatal(err)
	}
	events := hub.Subscribe(7)

	pending := Delivery{Status: StatusPending, Reason: ReasonUnreachable}
	s.Record(msg, pending)
	s.Record(msg, pending)
	s.Record(msg, Delivery{Status: StatusFailed, Reason: ReasonUnreachable, Failed: []string{"bob@remote.test"}})

	var got []string
	for len(events) > 0 {
		ev := <-events
		got = append(got, ev.DeliveryStatus+"/"+ev.DeliveryReason+"/"+strings.Join(ev.DeliveryFailed, ","))
	}
	if strings.Join(got, " ") != "pending/unreachable/ failed/unreachable/bob@remote.test" {
		t.Fatalf("events %v", got)
	}
	if store.messages[msg.MessageID].DeliveryFailed != "bob@remote.test" {
		t.Fatalf("delivery_failed %q", store.messages[msg.MessageID].DeliveryFailed)
	}
}

func TestSplitFailed(t *testing.T) {
	if got := SplitFailed(" a@x , ,b@y"); strings.Join(got, "|") != "a@x|b@y" {
		t.Fatalf("SplitFailed = %v", got)
	}
	if SplitFailed("") != nil {
		t.Fatal("empty → nil")
	}
}
