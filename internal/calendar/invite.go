package calendar

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log"
	"mime/multipart"
	"net/textproto"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/caldav/generator"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	smtpclient "github.com/yourusername/mailserver/internal/smtp/client"
)

// InviteService handles calendar invite operations
type InviteService struct {
	db *db.DB
}

// NewInviteService creates a new invite service
func NewInviteService(database *db.DB) *InviteService {
	return &InviteService{db: database}
}

// SendInvites sends invites to all attendees of an event
func (s *InviteService) SendInvites(event *models.CalendarEvent, attendees []models.CalendarAttendee, account *models.Account) error {
	icsData := generator.GenerateRequest(event, attendees)

	for _, attendee := range attendees {
		// Don't send to the organizer
		if strings.EqualFold(attendee.Email, event.OrganizerEmail) {
			continue
		}

		if err := s.sendInviteEmail(event, attendee, icsData, account, "REQUEST"); err != nil {
			log.Printf("Failed to send invite to %s: %v", attendee.Email, err)
			continue
		}
		log.Printf("Sent invite to %s for event '%s'", attendee.Email, event.Summary)
	}

	return nil
}

// SendUpdate sends an updated invite to all attendees
func (s *InviteService) SendUpdate(event *models.CalendarEvent, attendees []models.CalendarAttendee, account *models.Account) error {
	// Increment sequence for updates
	event.Sequence++

	icsData := generator.GenerateRequest(event, attendees)

	for _, attendee := range attendees {
		if strings.EqualFold(attendee.Email, event.OrganizerEmail) {
			continue
		}

		if err := s.sendInviteEmail(event, attendee, icsData, account, "UPDATE"); err != nil {
			log.Printf("Failed to send update to %s: %v", attendee.Email, err)
			continue
		}
		log.Printf("Sent update to %s for event '%s'", attendee.Email, event.Summary)
	}

	return nil
}

// SendCancel sends cancellation to all attendees
func (s *InviteService) SendCancel(event *models.CalendarEvent, attendees []models.CalendarAttendee, account *models.Account) error {
	icsData := generator.GenerateCancel(event, attendees)

	for _, attendee := range attendees {
		if strings.EqualFold(attendee.Email, event.OrganizerEmail) {
			continue
		}

		if err := s.sendInviteEmail(event, attendee, icsData, account, "CANCEL"); err != nil {
			log.Printf("Failed to send cancel to %s: %v", attendee.Email, err)
			continue
		}
		log.Printf("Sent cancel to %s for event '%s'", attendee.Email, event.Summary)
	}

	return nil
}

// SendReply sends a reply (ACCEPTED/DECLINED/TENTATIVE) to the organizer
func (s *InviteService) SendReply(event *models.CalendarEvent, attendeeEmail, attendeeName, partstat string, account *models.Account) error {
	if event.OrganizerEmail == "" {
		return fmt.Errorf("event has no organizer")
	}

	icsData := generator.GenerateReply(event, attendeeEmail, attendeeName, partstat)

	subject := getReplySubject(event.Summary, partstat)

	body := fmt.Sprintf("Response to meeting invite: %s\n\nUser %s has %s the meeting.",
		event.Summary, attendeeEmail, strings.ToLower(partstat))

	msg := buildCalendarEmail(
		account.Email,
		attendeeName,
		event.OrganizerEmail,
		"",
		subject,
		body,
		icsData,
		"REPLY",
	)

	client := smtpclient.New(account)
	if err := client.Send(account.Email, []string{event.OrganizerEmail}, msg); err != nil {
		return fmt.Errorf("failed to send reply: %w", err)
	}

	log.Printf("Sent %s reply to %s for event '%s'", partstat, event.OrganizerEmail, event.Summary)
	return nil
}

// sendInviteEmail sends a single invite email
func (s *InviteService) sendInviteEmail(event *models.CalendarEvent, attendee models.CalendarAttendee, icsData string, account *models.Account, action string) error {
	subject := getInviteSubject(event.Summary, action)

	body := buildInviteBody(event, action)

	msg := buildCalendarEmail(
		account.Email,
		event.OrganizerName,
		attendee.Email,
		attendee.Name,
		subject,
		body,
		icsData,
		"REQUEST",
	)

	client := smtpclient.New(account)
	return client.Send(account.Email, []string{attendee.Email}, msg)
}

// getInviteSubject returns the subject line for an invite
func getInviteSubject(summary, action string) string {
	switch action {
	case "UPDATE":
		return fmt.Sprintf("Updated: %s", summary)
	case "CANCEL":
		return fmt.Sprintf("Cancelled: %s", summary)
	default:
		return fmt.Sprintf("Invitation: %s", summary)
	}
}

// getReplySubject returns the subject line for a reply
func getReplySubject(summary, partstat string) string {
	switch partstat {
	case "ACCEPTED":
		return fmt.Sprintf("Accepted: %s", summary)
	case "DECLINED":
		return fmt.Sprintf("Declined: %s", summary)
	case "TENTATIVE":
		return fmt.Sprintf("Tentative: %s", summary)
	default:
		return fmt.Sprintf("Response: %s", summary)
	}
}

// buildInviteBody builds the text body for an invite email
func buildInviteBody(event *models.CalendarEvent, action string) string {
	var sb strings.Builder

	switch action {
	case "UPDATE":
		sb.WriteString("This meeting has been updated.\n\n")
	case "CANCEL":
		sb.WriteString("This meeting has been cancelled.\n\n")
	default:
		sb.WriteString("You have been invited to a meeting.\n\n")
	}

	sb.WriteString(fmt.Sprintf("Title: %s\n", event.Summary))

	if event.AllDay {
		sb.WriteString(fmt.Sprintf("When: %s (all day)\n", event.DTStart.Format("Monday, January 2, 2006")))
	} else {
		sb.WriteString(fmt.Sprintf("When: %s\n", event.DTStart.Format("Monday, January 2, 2006 at 15:04")))
		if event.DTEnd.Valid {
			sb.WriteString(fmt.Sprintf("Until: %s\n", event.DTEnd.Time.Format("15:04")))
		}
	}

	if event.Location != "" {
		sb.WriteString(fmt.Sprintf("Where: %s\n", event.Location))
	}

	if event.OrganizerName != "" {
		sb.WriteString(fmt.Sprintf("Organizer: %s <%s>\n", event.OrganizerName, event.OrganizerEmail))
	} else if event.OrganizerEmail != "" {
		sb.WriteString(fmt.Sprintf("Organizer: %s\n", event.OrganizerEmail))
	}

	if event.Description != "" {
		sb.WriteString(fmt.Sprintf("\n%s\n", event.Description))
	}

	return sb.String()
}

// buildCalendarEmail builds a MIME message with iCal attachment
func buildCalendarEmail(fromEmail, fromName, toEmail, toName, subject, body, icsData, method string) []byte {
	var buf bytes.Buffer

	// Generate boundary
	boundary := fmt.Sprintf("----=_Part_%d", time.Now().UnixNano())

	// Headers
	if fromName != "" {
		buf.WriteString(fmt.Sprintf("From: %s <%s>\r\n", fromName, fromEmail))
	} else {
		buf.WriteString(fmt.Sprintf("From: %s\r\n", fromEmail))
	}

	if toName != "" {
		buf.WriteString(fmt.Sprintf("To: %s <%s>\r\n", toName, toEmail))
	} else {
		buf.WriteString(fmt.Sprintf("To: %s\r\n", toEmail))
	}

	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeHeader(subject)))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n", boundary))
	buf.WriteString("\r\n")

	// Multipart body
	writer := multipart.NewWriter(&buf)
	writer.SetBoundary(boundary)

	// Text part
	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	textPart, _ := writer.CreatePart(textHeader)
	textPart.Write([]byte(body))

	// iCal part (text/calendar)
	icsHeader := textproto.MIMEHeader{}
	icsHeader.Set("Content-Type", fmt.Sprintf("text/calendar; charset=utf-8; method=%s", method))
	icsHeader.Set("Content-Transfer-Encoding", "base64")
	icsHeader.Set("Content-Disposition", "attachment; filename=\"invite.ics\"")
	icsPart, _ := writer.CreatePart(icsHeader)
	icsPart.Write([]byte(base64Encode(icsData)))

	writer.Close()

	return buf.Bytes()
}

// base64Encode encodes data as base64 with line breaks
func base64Encode(data string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(data))
	// Add line breaks every 76 characters
	var result strings.Builder
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		result.WriteString(encoded[i:end])
		result.WriteString("\r\n")
	}
	return result.String()
}

// encodeHeader encodes a header value for non-ASCII characters
func encodeHeader(s string) string {
	// Check if encoding is needed
	needsEncoding := false
	for _, c := range s {
		if c > 127 {
			needsEncoding = true
			break
		}
	}

	if !needsEncoding {
		return s
	}

	// Use base64 encoding for non-ASCII
	encoded := base64.StdEncoding.EncodeToString([]byte(s))
	return fmt.Sprintf("=?UTF-8?B?%s?=", encoded)
}
