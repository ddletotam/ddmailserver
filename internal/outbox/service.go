package outbox

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// Store is what the outbox needs from the database (*db.DB).
type Store interface {
	SetOutboxPrepared(id int64, raw []byte, messageID string) ([]byte, string, bool, error)
	GetOutboxAttachmentsByMessageID(outboxMsgID int64) ([]*models.OutboxAttachment, error)
	GetOrCreateLocalFolder(userID int64, name, folderType string) (*models.Folder, error)
	GetNextUIDForFolder(folderID int64) (uint32, error)
	CreateMessage(msg *models.Message) error
	CreateAttachment(att *models.Attachment) error
	UpdateMessageAttachmentCount(id int64, count int) error
	StartMessageDelivery(userID int64, messageID string) (int64, bool, error)
	SetMessageDelivery(userID int64, messageID string, d db.MessageDelivery) (int64, bool, error)
}

// Service accepts outgoing messages and records how their delivery went.
// It is cheap to build; the zero hub means no client notifications.
type Service struct {
	store Store
	hub   *notify.Hub
	now   func() time.Time
}

// New returns a Service over store, publishing to hub (may be nil).
func New(store Store, hub *notify.Hub) *Service {
	return &Service{store: store, hub: hub, now: time.Now}
}

// Prepare fixes the wire form and the Message-ID of a queued message and
// stores both on its outbox row, so every send attempt and the copy in Sent
// are byte-for-byte the same message. A row that came without a wire form is
// composed from its fields and stored attachments (Compose).
//
// fresh reports that this call did the work. It is false for a row prepared
// before — by the send handler, or by an earlier attempt — which is how a
// send task knows the row was accepted by a server version that had no copy
// in Sent yet.
func (s *Service) Prepare(msg *models.OutboxMessage) (fresh bool, err error) {
	if msg.MessageID != "" && len(msg.RawEmail) > 0 {
		return false, nil
	}

	raw := msg.RawEmail
	if len(raw) == 0 {
		atts, err := s.store.GetOutboxAttachmentsByMessageID(msg.ID)
		if err != nil {
			return false, fmt.Errorf("outbox message %d: load attachments: %w", msg.ID, err)
		}
		raw = Compose(msg, atts, s.now())
	}

	parsed, err := parser.New().ParseBytes(raw)
	if err != nil {
		return false, fmt.Errorf("outbox message %d: parse: %w", msg.ID, err)
	}
	if parsed.MessageID == "" {
		// Every path that queues mail writes a Message-ID (submission
		// requires one), so this is a bug, not a user error.
		return false, fmt.Errorf("outbox message %d has no Message-ID", msg.ID)
	}

	storedRaw, storedID, won, err := s.store.SetOutboxPrepared(msg.ID, raw, parsed.MessageID)
	if err != nil {
		return false, err
	}
	// Lost a race to another preparer (handler vs. send task): its version
	// is the message.
	msg.RawEmail = storedRaw
	msg.MessageID = storedID
	return won, nil
}

// Accept is what a send handler calls once the outbox row (and its
// attachments) are stored: Prepare, then the sender's copy in Sent with
// status «pending». The error means the message is queued but may lack its
// copy; it goes out regardless.
func (s *Service) Accept(msg *models.OutboxMessage) error {
	if _, err := s.Prepare(msg); err != nil {
		return err
	}
	return s.SaveSentCopy(msg)
}

// SaveSentCopy puts the sender's copy of a prepared message into the user's
// local Sent folder with delivery status «pending» and tells the user's
// clients (message_sent) that the conversation has something new.
//
// The copy may already be there: an IMAP client APPENDs its own copy to
// Sent after submitting, and a relay provider's own copy comes back with the
// sync of its Sent folder. Messages are unique per (user, Message-ID), so the
// second arrival is dropped — whichever came first stays, and only its
// delivery status is started (an existing status is never reset).
func (s *Service) SaveSentCopy(msg *models.OutboxMessage) error {
	if msg.MessageID == "" || len(msg.RawEmail) == 0 {
		return fmt.Errorf("outbox message %d is not prepared", msg.ID)
	}

	folder, err := s.store.GetOrCreateLocalFolder(msg.UserID, "Sent", "sent")
	if err != nil {
		return fmt.Errorf("outbox message %d: Sent folder: %w", msg.ID, err)
	}

	parsed, err := parser.New().ParseBytes(msg.RawEmail)
	if err != nil {
		return fmt.Errorf("outbox message %d: parse: %w", msg.ID, err)
	}

	uid, err := s.store.GetNextUIDForFolder(folder.ID)
	if err != nil {
		return fmt.Errorf("outbox message %d: next UID: %w", msg.ID, err)
	}

	date := parsed.GetDate()
	if date.IsZero() {
		date = s.now()
	}
	copyMsg := &models.Message{
		AccountID: 0, // the server's own copy, not mirrored from any account
		UserID:    msg.UserID,
		FolderID:  folder.ID,
		MessageID: parsed.MessageID,
		Subject:   parser.SanitizeUTF8(parsed.Subject),
		From:      parser.SanitizeUTF8(parser.FormatAddress(parsed.From)),
		To:        parser.SanitizeUTF8(parser.FormatAddressList(parsed.To)),
		Cc:        parser.SanitizeUTF8(parser.FormatAddressList(parsed.Cc)),
		// Bcc never travels in the headers, but the sender should see whom
		// the message went to.
		Bcc:               parser.SanitizeUTF8(msg.Bcc),
		ReplyTo:           parser.SanitizeUTF8(parser.FormatAddress(parsed.ReplyTo)),
		Date:              timeutil.ToMs(date),
		DateTZ:            timeutil.TZOffsetMinutes(date),
		Body:              parser.SanitizeUTF8(parsed.Body),
		BodyHTML:          parser.SanitizeUTF8(parsed.BodyHTML),
		RawEmail:          msg.RawEmail, // «показать исходник» shows the real RFC-822
		Attachments:       len(parsed.Attachments),
		Size:              int64(len(msg.RawEmail)),
		UID:               uid,
		Seen:              true,
		InReplyTo:         parser.SanitizeUTF8(parsed.InReplyTo),
		MessageReferences: parser.SanitizeUTF8(strings.Join(parsed.References, " ")),
		DeliveryStatus:    StatusPending,
	}
	if copyMsg.From == "" {
		copyMsg.From = msg.From
	}

	err = s.store.CreateMessage(copyMsg)
	if errors.Is(err, db.ErrDuplicateMessage) {
		id, changed, serr := s.store.StartMessageDelivery(msg.UserID, parsed.MessageID)
		if serr != nil {
			return fmt.Errorf("outbox message %d: %w", msg.ID, serr)
		}
		log.Printf("outbox: message %d already has a copy (id %d) in the user's mail, delivery status started: %v",
			msg.ID, id, changed)
		if changed {
			s.publishSent(msg.UserID, folder, id, parsed.MessageID)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("outbox message %d: save copy to Sent: %w", msg.ID, err)
	}

	// Attachments, inline images included: the copy is the sender's only
	// record of what went out.
	for _, att := range parsed.Attachments {
		a := &models.Attachment{
			MessageID:   copyMsg.ID,
			ContentID:   strings.Trim(att.ContentID, "<>"),
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Size:        int(att.Size),
			IsInline:    att.IsInline,
			Data:        att.Data,
		}
		if err := s.store.CreateAttachment(a); err != nil {
			log.Printf("outbox: message %d: save attachment of the Sent copy: %v", msg.ID, err)
		}
	}

	log.Printf("outbox: message %d accepted, copy %d in Sent (to %s)", msg.ID, copyMsg.ID, logmask.AddrList(msg.To))
	s.publishSent(msg.UserID, folder, copyMsg.ID, parsed.MessageID)
	return nil
}

// Record writes the delivery outcome onto the sender's copy and, if it
// changed, tells the user's clients (delivery_status). Best-effort: a
// failure is logged, the delivery itself is not affected.
func (s *Service) Record(msg *models.OutboxMessage, d Delivery) {
	if msg.MessageID == "" {
		return
	}
	id, changed, err := s.store.SetMessageDelivery(msg.UserID, msg.MessageID, db.MessageDelivery{
		Status: d.Status,
		Reason: d.Reason,
		Failed: d.failedList(),
	})
	if err != nil {
		log.Printf("outbox: message %d: record delivery status %s: %v", msg.ID, d.Status, err)
		return
	}
	if id == 0 {
		log.Printf("outbox: message %d: delivery status %s not recorded — the sender has no copy of it", msg.ID, d.Status)
		return
	}
	if !changed {
		return
	}
	log.Printf("outbox: message %d (copy %d): delivery status %s %s", msg.ID, id, d.Status, d.Reason)
	if s.hub == nil {
		return
	}
	s.hub.Publish(notify.Event{
		UserID:         msg.UserID,
		Type:           notify.EventDeliveryStatus,
		MessageID:      id,
		RFCMessageID:   msg.MessageID,
		DeliveryStatus: d.Status,
		DeliveryReason: d.Reason,
		DeliveryFailed: d.Failed,
	})
}

func (s *Service) publishSent(userID int64, folder *models.Folder, id int64, messageID string) {
	if s.hub == nil {
		return
	}
	s.hub.Publish(notify.Event{
		UserID:       userID,
		Type:         notify.EventMessageSent,
		FolderID:     folder.ID,
		Mailbox:      folder.Name,
		MessageID:    id,
		RFCMessageID: messageID,
	})
}
