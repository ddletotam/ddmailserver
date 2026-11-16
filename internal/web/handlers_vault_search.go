package web

import (
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/search"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// VaultData holds data for the vault page
type VaultData struct {
	PageData
	Messages     []*models.Message
	MessageCount int
}

// SearchData holds data for the search page
type SearchData struct {
	PageData
	Query      string
	SearchType string
	Results    []search.IndexDocument
	TotalHits  int64
}

// messagesToIndexDocuments converts messages to IndexDocument for template compatibility
func messagesToIndexDocuments(messages []*models.Message) []search.IndexDocument {
	docs := make([]search.IndexDocument, 0, len(messages))
	for _, msg := range messages {
		var calendarEventID int64
		if msg.CalendarEventID != nil {
			calendarEventID = *msg.CalendarEventID
		}

		// Truncate body for display
		body := msg.Body
		if len(body) > 200 {
			body = body[:200] + "..."
		}

		docs = append(docs, search.IndexDocument{
			ID:              msg.ID,
			UserID:          msg.UserID,
			FolderID:        msg.FolderID,
			AccountID:       msg.AccountID,
			Subject:         msg.Subject,
			From:            msg.From,
			To:              msg.To,
			Cc:              msg.Cc,
			Body:            body,
			Date:            msg.Date / 1000,
			DateFormatted:   timeutil.FromMs(msg.Date).Format("02.01.2006"),
			Seen:            msg.Seen,
			Flagged:         msg.Flagged,
			SpamStatus:      msg.SpamStatus,
			SpamScore:       msg.SpamScore,
			SoftDeleted:     msg.SoftDeleted,
			Type:            "email",
			CalendarEventID: calendarEventID,
		})
	}
	return docs
}
