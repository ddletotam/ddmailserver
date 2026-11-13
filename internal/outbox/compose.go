package outbox

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
)

// randomBoundary generates a random MIME boundary string.
func randomBoundary() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("----=_Part_%x", b)
}

// Compose builds an RFC 5322 message from the fields of an outbox row that
// came without a wire form (the web compose form; rows queued before
// migration 056). attachments become MIME parts inside multipart/mixed.
//
// It used to run on every send attempt, minting a new Message-ID each time;
// now Service.Prepare runs it once and stores the result, so every attempt
// and the copy in Sent are the same message.
func Compose(msg *models.OutboxMessage, attachments []*models.OutboxAttachment, now time.Time) []byte {
	var email strings.Builder

	// Generate Message-ID
	domain := "localhost"
	if parts := strings.SplitN(msg.From, "@", 2); len(parts) == 2 {
		// Extract domain from "Name <user@domain>" or "user@domain"
		d := parts[1]
		d = strings.TrimRight(d, ">")
		d = strings.TrimSpace(d)
		if d != "" {
			domain = d
		}
	}
	randBytes := make([]byte, 8)
	_, _ = rand.Read(randBytes)
	messageID := fmt.Sprintf("<%d.%s@%s>", now.UnixNano(), hex.EncodeToString(randBytes), domain)

	// Headers. Free-text (Subject) and display-names (From/To/Cc) are
	// RFC 2047-encoded so the header section stays 7-bit ASCII — otherwise a
	// Cyrillic Subject forces relays to require SMTPUTF8, which legacy
	// receivers reject (see parser.EncodeHeaderWord).
	email.WriteString(fmt.Sprintf("Message-ID: %s\r\n", messageID))
	email.WriteString(fmt.Sprintf("Date: %s\r\n", now.Format("Mon, 02 Jan 2006 15:04:05 -0700")))
	email.WriteString(fmt.Sprintf("From: %s\r\n", parser.EncodeAddressHeader(msg.From)))
	email.WriteString(fmt.Sprintf("To: %s\r\n", parser.EncodeAddressHeader(msg.To)))
	if msg.Cc != "" {
		email.WriteString(fmt.Sprintf("Cc: %s\r\n", parser.EncodeAddressHeader(msg.Cc)))
	}
	if msg.Subject != "" {
		email.WriteString(fmt.Sprintf("Subject: %s\r\n", parser.EncodeHeaderWord(msg.Subject)))
	}
	email.WriteString("MIME-Version: 1.0\r\n")

	// Build body part
	var bodyBuf strings.Builder
	hasPlain := msg.Body != ""
	hasHTML := msg.BodyHTML != ""

	if hasPlain && hasHTML {
		altBoundary := randomBoundary()
		bodyBuf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", altBoundary))
		bodyBuf.WriteString(fmt.Sprintf("--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", altBoundary, msg.Body))
		bodyBuf.WriteString(fmt.Sprintf("--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", altBoundary, msg.BodyHTML))
		bodyBuf.WriteString(fmt.Sprintf("--%s--\r\n", altBoundary))
	} else if hasHTML {
		bodyBuf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		bodyBuf.WriteString(msg.BodyHTML)
	} else {
		bodyBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		bodyBuf.WriteString(msg.Body)
	}

	if len(attachments) > 0 {
		mixedBoundary := randomBoundary()
		email.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", mixedBoundary))

		// Body part
		email.WriteString(fmt.Sprintf("--%s\r\n%s\r\n", mixedBoundary, bodyBuf.String()))

		// Attachment parts
		for _, att := range attachments {
			email.WriteString(fmt.Sprintf("--%s\r\n", mixedBoundary))
			email.WriteString(fmt.Sprintf("Content-Type: %s; name=\"%s\"\r\n", att.ContentType, att.Filename))
			email.WriteString("Content-Transfer-Encoding: base64\r\n")
			email.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", att.Filename))
			encoded := base64.StdEncoding.EncodeToString(att.Data)
			for i := 0; i < len(encoded); i += 76 {
				end := i + 76
				if end > len(encoded) {
					end = len(encoded)
				}
				email.WriteString(encoded[i:end])
				email.WriteString("\r\n")
			}
		}
		email.WriteString(fmt.Sprintf("--%s--\r\n", mixedBoundary))
	} else {
		// No attachments — write body directly
		email.WriteString(bodyBuf.String())
	}

	return []byte(email.String())
}
