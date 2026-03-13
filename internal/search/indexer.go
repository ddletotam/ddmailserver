package search

import (
	"log"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// Indexer handles synchronization between database and Meilisearch
type Indexer struct {
	client   *Client
	database *db.DB
}

// NewIndexer creates a new indexer
func NewIndexer(client *Client, database *db.DB) *Indexer {
	return &Indexer{
		client:   client,
		database: database,
	}
}

// Initialize sets up the Meilisearch index
func (idx *Indexer) Initialize() error {
	if err := idx.client.Health(); err != nil {
		return err
	}
	return idx.client.CreateIndex()
}

// IndexAllMessages indexes all messages from database (full reindex)
func (idx *Indexer) IndexAllMessages() error {
	log.Println("Starting full message reindex...")

	// Get total count
	total, err := idx.database.GetTotalMessageCount()
	if err != nil {
		return err
	}
	log.Printf("Found %d messages to index", total)

	batchSize := 100
	indexed := 0

	for offset := 0; offset < total; offset += batchSize {
		messages, err := idx.database.GetMessagesForIndexing(batchSize, offset)
		if err != nil {
			log.Printf("Failed to get messages at offset %d: %v", offset, err)
			continue
		}

		docs := make([]IndexDocument, 0, len(messages))
		for _, msg := range messages {
			docs = append(docs, messageToDocument(msg))
		}

		if err := idx.client.IndexDocuments(docs); err != nil {
			log.Printf("Failed to index batch at offset %d: %v", offset, err)
			continue
		}

		indexed += len(docs)
		log.Printf("Indexed %d/%d messages", indexed, total)
	}

	log.Printf("Full reindex complete: %d messages indexed", indexed)
	return nil
}

// IndexMessage indexes a single message
func (idx *Indexer) IndexMessage(msg *models.Message) error {
	doc := messageToDocument(msg)
	return idx.client.IndexDocuments([]IndexDocument{doc})
}

// IndexMessages indexes multiple messages
func (idx *Indexer) IndexMessages(messages []*models.Message) error {
	if len(messages) == 0 {
		return nil
	}

	docs := make([]IndexDocument, 0, len(messages))
	for _, msg := range messages {
		docs = append(docs, messageToDocument(msg))
	}

	return idx.client.IndexDocuments(docs)
}

// DeleteMessage removes a message from the index
func (idx *Indexer) DeleteMessage(messageID int64) error {
	return idx.client.DeleteDocument(messageID)
}

// DeleteMessages removes multiple messages from the index
func (idx *Indexer) DeleteMessages(ids []int64) error {
	return idx.client.DeleteDocuments(ids)
}

// UpdateMessageFlags updates the seen/flagged status of a message
func (idx *Indexer) UpdateMessageFlags(msg *models.Message) error {
	// Re-index the message (Meilisearch will update by primary key)
	return idx.IndexMessage(msg)
}

// MarkSoftDeleted updates the soft_deleted status
func (idx *Indexer) MarkSoftDeleted(messageID int64, deleted bool) error {
	// We need to fetch the message and re-index
	msg, err := idx.database.GetMessageByID(messageID)
	if err != nil {
		return err
	}
	if msg == nil {
		return nil
	}

	doc := messageToDocument(msg)
	doc.SoftDeleted = deleted
	return idx.client.IndexDocuments([]IndexDocument{doc})
}

// Search searches for messages
func (idx *Indexer) Search(userID int64, query string, limit, offset int) (*SearchResult, error) {
	return idx.client.Search(userID, query, limit, offset)
}

// SearchInFolder searches within a folder
func (idx *Indexer) SearchInFolder(userID, folderID int64, query string, limit, offset int) (*SearchResult, error) {
	return idx.client.SearchInFolder(userID, folderID, query, limit, offset)
}

// GetStats returns indexing statistics
func (idx *Indexer) GetStats() (map[string]interface{}, error) {
	return idx.client.GetStats()
}

// messageToDocument converts a message to an index document
func messageToDocument(msg *models.Message) IndexDocument {
	// Truncate body to avoid huge documents
	body := msg.Body
	if len(body) > 50000 {
		body = body[:50000]
	}

	// Clean HTML from body if present
	if msg.BodyHTML != "" && body == "" {
		body = stripHTML(msg.BodyHTML)
		if len(body) > 50000 {
			body = body[:50000]
		}
	}

	var calendarEventID int64
	if msg.CalendarEventID != nil {
		calendarEventID = *msg.CalendarEventID
	}

	return IndexDocument{
		ID:              msg.ID,
		UserID:          msg.UserID,
		FolderID:        msg.FolderID,
		AccountID:       msg.AccountID,
		Subject:         msg.Subject,
		From:            msg.From,
		To:              msg.To,
		Cc:              msg.Cc,
		Body:            body,
		Date:            msg.Date.Unix(),
		DateISO:         msg.Date.Format(time.RFC3339),
		DateFormatted:   msg.Date.Format("02.01.2006"),
		MessageID:       msg.MessageID,
		HasAttach:       msg.Attachments > 0,
		Seen:            msg.Seen,
		Flagged:         msg.Flagged,
		SpamStatus:      msg.SpamStatus,
		SpamScore:       msg.SpamScore,
		SoftDeleted:     msg.SoftDeleted,
		Type:            "email",
		CalendarEventID: calendarEventID,
	}
}

// stripHTML removes HTML tags from a string (simple implementation)
func stripHTML(html string) string {
	// Simple tag removal
	var result strings.Builder
	inTag := false

	for _, r := range html {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			result.WriteRune(' ')
			continue
		}
		if !inTag {
			result.WriteRune(r)
		}
	}

	// Clean up multiple spaces
	text := result.String()
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}

	return strings.TrimSpace(text)
}
