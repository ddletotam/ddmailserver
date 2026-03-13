package web

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/search"
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

// HandleVaultPage displays soft-deleted messages
func (s *Server) HandleVaultPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get soft-deleted messages
	messages, err := s.database.GetSoftDeletedMessages(user.ID, 100, 0)
	if err != nil {
		log.Printf("Failed to get soft-deleted messages: %v", err)
		messages = nil
	}

	// Get user's language for title translation
	userLang := user.Language
	if userLang == "" {
		userLang = "en"
	}
	i18n := s.i18nManager.Get(userLang)

	data := VaultData{
		PageData: PageData{
			Title: i18n.T("vault.title"),
			User:  user,
		},
		Messages:     messages,
		MessageCount: len(messages),
	}

	s.renderTemplate(w, "vault.html", data)
}

// HandleRestoreMessage restores a soft-deleted message to its original folder
func (s *Server) HandleRestoreMessage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	messageID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	// Verify message belongs to user
	msg, err := s.database.GetMessageByID(messageID)
	if err != nil || msg == nil || msg.UserID != user.ID {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Restore message
	if err := s.database.RestoreSoftDeletedMessage(messageID); err != nil {
		log.Printf("Failed to restore message: %v", err)
		http.Error(w, "Failed to restore", http.StatusInternalServerError)
		return
	}

	// For htmx, return empty response to remove the row
	w.Header().Set("HX-Trigger", "messageRestored")
	w.WriteHeader(http.StatusOK)
}

// HandlePermanentDelete permanently deletes a soft-deleted message
func (s *Server) HandlePermanentDelete(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	messageID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	// Verify message belongs to user and is soft-deleted
	msg, err := s.database.GetMessageByID(messageID)
	if err != nil || msg == nil || msg.UserID != user.ID {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Hard delete message
	if err := s.database.HardDeleteMessage(messageID); err != nil {
		log.Printf("Failed to permanently delete message: %v", err)
		http.Error(w, "Failed to delete", http.StatusInternalServerError)
		return
	}

	// Remove from search index if available
	if s.searchIndexer != nil {
		s.searchIndexer.DeleteMessage(messageID)
	}

	w.Header().Set("HX-Trigger", "messageDeleted")
	w.WriteHeader(http.StatusOK)
}

// HandleSearchPage displays search page with results
func (s *Server) HandleSearchPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	query := r.URL.Query().Get("q")
	searchType := r.URL.Query().Get("type") // "all", "mail", "calendar"
	if searchType == "" {
		searchType = "all"
	}

	// Get user's language for title translation
	userLang := user.Language
	if userLang == "" {
		userLang = "en"
	}
	i18n := s.i18nManager.Get(userLang)

	data := SearchData{
		PageData: PageData{
			Title: i18n.T("search.title"),
			User:  user,
		},
		Query:      query,
		SearchType: searchType,
		Results:    nil,
		TotalHits:  0,
	}

	// Perform search if query provided
	if query != "" && s.searchIndexer != nil {
		result, err := s.searchIndexer.Search(user.ID, query, 50, 0)
		if err != nil {
			log.Printf("Search failed: %v", err)
		} else if result != nil {
			data.Results = result.Hits
			data.TotalHits = result.EstimatedTotal
		}
	} else if query != "" {
		// Fallback to database search if Meilisearch not available
		messages, err := s.database.SearchMessages(user.ID, query, 50, 0)
		if err != nil {
			log.Printf("DB search failed: %v", err)
		} else {
			// Convert messages to IndexDocument for template compatibility
			data.Results = messagesToIndexDocuments(messages)
			data.TotalHits = int64(len(messages))
		}
	}

	s.renderTemplate(w, "search.html", data)
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
			Date:            msg.Date.Unix(),
			DateFormatted:   msg.Date.Format("02.01.2006"),
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
