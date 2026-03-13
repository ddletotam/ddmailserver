package db

import (
	"fmt"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// CreateFakeEmailForEvent creates a fake message for a calendar event
// The message is marked as soft_deleted so it's searchable but not visible in IMAP
func (db *DB) CreateFakeEmailForEvent(event *models.CalendarEvent, userID int64, folderID int64) (*models.Message, error) {
	// Build email body from event details
	var bodyParts []string
	bodyParts = append(bodyParts, fmt.Sprintf("Event: %s", event.Summary))

	if event.Description != "" {
		bodyParts = append(bodyParts, fmt.Sprintf("\nDescription: %s", event.Description))
	}
	if event.Location != "" {
		bodyParts = append(bodyParts, fmt.Sprintf("\nLocation: %s", event.Location))
	}

	// Format date/time
	dateFormat := "02.01.2006 15:04"
	if event.AllDay {
		dateFormat = "02.01.2006"
	}
	bodyParts = append(bodyParts, fmt.Sprintf("\nStart: %s", event.DTStart.Format(dateFormat)))
	if event.DTEnd.Valid && !event.DTEnd.Time.IsZero() {
		bodyParts = append(bodyParts, fmt.Sprintf("\nEnd: %s", event.DTEnd.Time.Format(dateFormat)))
	}

	body := strings.Join(bodyParts, "")

	// Determine from address
	fromAddr := "calendar@system"
	if event.OrganizerEmail != "" {
		fromAddr = event.OrganizerEmail
		if event.OrganizerName != "" {
			fromAddr = fmt.Sprintf("%s <%s>", event.OrganizerName, event.OrganizerEmail)
		}
	}

	now := time.Now()
	eventID := event.ID

	msg := &models.Message{
		UserID:          userID,
		FolderID:        folderID,
		MessageID:       fmt.Sprintf("calendar-event-%d@system", event.ID),
		Subject:         fmt.Sprintf("[Calendar] %s", event.Summary),
		From:            fromAddr,
		To:              "",
		Date:            event.DTStart,
		Body:            body,
		Size:            int64(len(body)),
		UID:             0, // Not a real IMAP message
		Seen:            true,
		SoftDeleted:     true, // Hidden from IMAP but indexed in Meilisearch
		CalendarEventID: &eventID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	query := `
		INSERT INTO messages (
			user_id, folder_id, message_id, subject, from_addr, to_addr,
			date, body, size, uid, seen, soft_deleted, soft_deleted_at,
			calendar_event_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING id
	`

	err := db.QueryRow(
		query,
		msg.UserID, msg.FolderID, msg.MessageID, msg.Subject, msg.From, msg.To,
		msg.Date, msg.Body, msg.Size, msg.UID, msg.Seen, msg.SoftDeleted, now,
		msg.CalendarEventID, msg.CreatedAt, msg.UpdatedAt,
	).Scan(&msg.ID)

	if err != nil {
		return nil, fmt.Errorf("failed to create fake email for event: %w", err)
	}

	return msg, nil
}

// UpdateFakeEmailForEvent updates the fake message for a calendar event
func (db *DB) UpdateFakeEmailForEvent(event *models.CalendarEvent) error {
	// Build email body from event details
	var bodyParts []string
	bodyParts = append(bodyParts, fmt.Sprintf("Event: %s", event.Summary))

	if event.Description != "" {
		bodyParts = append(bodyParts, fmt.Sprintf("\nDescription: %s", event.Description))
	}
	if event.Location != "" {
		bodyParts = append(bodyParts, fmt.Sprintf("\nLocation: %s", event.Location))
	}

	// Format date/time
	dateFormat := "02.01.2006 15:04"
	if event.AllDay {
		dateFormat = "02.01.2006"
	}
	bodyParts = append(bodyParts, fmt.Sprintf("\nStart: %s", event.DTStart.Format(dateFormat)))
	if event.DTEnd.Valid && !event.DTEnd.Time.IsZero() {
		bodyParts = append(bodyParts, fmt.Sprintf("\nEnd: %s", event.DTEnd.Time.Format(dateFormat)))
	}

	body := strings.Join(bodyParts, "")

	// Determine from address
	fromAddr := "calendar@system"
	if event.OrganizerEmail != "" {
		fromAddr = event.OrganizerEmail
		if event.OrganizerName != "" {
			fromAddr = fmt.Sprintf("%s <%s>", event.OrganizerName, event.OrganizerEmail)
		}
	}

	query := `
		UPDATE messages
		SET subject = $1, from_addr = $2, date = $3, body = $4, size = $5, updated_at = $6
		WHERE calendar_event_id = $7
	`

	_, err := db.Exec(
		query,
		fmt.Sprintf("[Calendar] %s", event.Summary),
		fromAddr,
		event.DTStart,
		body,
		int64(len(body)),
		time.Now(),
		event.ID,
	)

	if err != nil {
		return fmt.Errorf("failed to update fake email for event: %w", err)
	}

	return nil
}

// DeleteFakeEmailForEvent deletes the fake message for a calendar event
func (db *DB) DeleteFakeEmailForEvent(eventID int64) error {
	query := `DELETE FROM messages WHERE calendar_event_id = $1`

	_, err := db.Exec(query, eventID)
	if err != nil {
		return fmt.Errorf("failed to delete fake email for event: %w", err)
	}

	return nil
}

// GetFakeEmailForEvent retrieves the fake message for a calendar event
func (db *DB) GetFakeEmailForEvent(eventID int64) (*models.Message, error) {
	query := `
		SELECT id, COALESCE(account_id, 0), user_id, folder_id, message_id, subject, from_addr, to_addr, cc, bcc, reply_to,
		       date, body, body_html, attachments, size, uid, seen, flagged, answered, draft, deleted,
		       in_reply_to, message_references, COALESCE(spam_score, 0), COALESCE(spam_status, 'clean'), COALESCE(spam_reasons, ''),
		       created_at, updated_at
		FROM messages
		WHERE calendar_event_id = $1
	`

	msg := &models.Message{}
	err := db.QueryRow(query, eventID).Scan(
		&msg.ID, &msg.AccountID, &msg.UserID, &msg.FolderID, &msg.MessageID, &msg.Subject, &msg.From, &msg.To,
		&msg.Cc, &msg.Bcc, &msg.ReplyTo, &msg.Date, &msg.Body, &msg.BodyHTML, &msg.Attachments, &msg.Size,
		&msg.UID, &msg.Seen, &msg.Flagged, &msg.Answered, &msg.Draft, &msg.Deleted,
		&msg.InReplyTo, &msg.MessageReferences, &msg.SpamScore, &msg.SpamStatus, &msg.SpamReasons,
		&msg.CreatedAt, &msg.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	return msg, nil
}
