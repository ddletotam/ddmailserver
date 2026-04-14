package web

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
)

// HandleDeleteMessage soft-deletes a message (moves to vault)
func (s *Server) HandleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	msg, err := s.database.GetMessageByID(id)
	if err != nil || msg.UserID != user.ID {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	if err := s.database.SoftDeleteMessage(id); err != nil {
		log.Printf("Failed to soft-delete message %d: %v", id, err)
		http.Error(w, "Failed to delete message", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/inbox")
	w.WriteHeader(http.StatusOK)
}

// HandleReply returns a compose modal pre-filled for reply
func (s *Server) HandleReply(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	msg, err := s.database.GetMessageByID(id)
	if err != nil || msg.UserID != user.ID {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	accounts, _ := s.database.GetAccountsByUserID(user.ID)

	// Build reply To
	to := msg.ReplyTo
	if to == "" {
		to = msg.From
	}

	// Build subject
	subject := msg.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}

	// Build quoted body
	dateStr := msg.Date.Format("02 Jan 2006 15:04")
	body := msg.Body
	if body == "" {
		body = stripHTMLSimple(msg.BodyHTML)
	}
	var quoted strings.Builder
	quoted.WriteString("\n\n---\n")
	quoted.WriteString(fmt.Sprintf("On %s, %s wrote:\n", dateStr, msg.From))
	for _, line := range strings.Split(body, "\n") {
		quoted.WriteString("> " + line + "\n")
	}

	data := struct {
		Accounts  []*models.Account
		To        string
		Subject   string
		Body      string
		ReplyTo   string
		IsReply   bool
		IsForward bool
	}{
		Accounts: accounts,
		To:       to,
		Subject:  subject,
		Body:     quoted.String(),
		ReplyTo:  msg.MessageID,
		IsReply:  true,
	}

	s.renderTemplatePartial(w, "compose.html", "compose-modal", data)
}

// HandleForward returns a compose modal pre-filled for forwarding
func (s *Server) HandleForward(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	msg, err := s.database.GetMessageByID(id)
	if err != nil || msg.UserID != user.ID {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	accounts, _ := s.database.GetAccountsByUserID(user.ID)

	subject := msg.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "fwd:") {
		subject = "Fwd: " + subject
	}

	body := msg.Body
	if body == "" {
		body = stripHTMLSimple(msg.BodyHTML)
	}
	var fwd strings.Builder
	fwd.WriteString("\n\n--- Forwarded Message ---\n")
	fwd.WriteString(fmt.Sprintf("From: %s\n", msg.From))
	fwd.WriteString(fmt.Sprintf("To: %s\n", msg.To))
	fwd.WriteString(fmt.Sprintf("Date: %s\n", msg.Date.Format("02 Jan 2006 15:04")))
	fwd.WriteString(fmt.Sprintf("Subject: %s\n\n", msg.Subject))
	fwd.WriteString(body)

	data := struct {
		Accounts  []*models.Account
		To        string
		Subject   string
		Body      string
		ReplyTo   string
		IsReply   bool
		IsForward bool
	}{
		Accounts:  accounts,
		Subject:   subject,
		Body:      fwd.String(),
		IsForward: true,
	}

	s.renderTemplatePartial(w, "compose.html", "compose-modal", data)
}

// HandleSaveDraft saves a message as draft
func (s *Server) HandleSaveDraft(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	folder, err := s.database.GetOrCreateFolderByNameAndUser(user.ID, "Drafts", "drafts")
	if err != nil {
		log.Printf("Failed to get/create Drafts folder: %v", err)
		http.Error(w, "Failed to save draft", http.StatusInternalServerError)
		return
	}

	uid, err := s.database.GetNextUIDForFolder(folder.ID)
	if err != nil {
		log.Printf("Failed to get next UID: %v", err)
		http.Error(w, "Failed to save draft", http.StatusInternalServerError)
		return
	}

	var accountID int64
	if v := r.FormValue("account_id"); v != "" {
		accountID, _ = strconv.ParseInt(v, 10, 64)
	}

	body := r.FormValue("body")
	bodyHTML := ""
	if r.FormValue("format") == "html" {
		bodyHTML = body
		body = ""
	}

	msg := &models.Message{
		UserID:    user.ID,
		FolderID:  folder.ID,
		UID:       uid,
		Subject:   r.FormValue("subject"),
		From:      r.FormValue("from"),
		To:        r.FormValue("to"),
		Cc:        r.FormValue("cc"),
		Bcc:       r.FormValue("bcc"),
		Body:      body,
		BodyHTML:  bodyHTML,
		Draft:     true,
		Seen:      true,
		Date:      time.Now(),
		AccountID: accountID,
	}

	if err := s.database.CreateMessage(msg); err != nil {
		log.Printf("Failed to save draft: %v", err)
		http.Error(w, "Failed to save draft", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, `<div class="alert alert-success alert-dismissible" role="alert">Draft saved<a href="#" class="btn-close" data-bs-dismiss="alert"></a></div>`)
}

// HandleRefreshMessages triggers IMAP sync for all user accounts
func (s *Server) HandleRefreshMessages(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Return immediately; IDLE manager handles real-time sync.
	// Just return the updated message list.
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, `<div class="alert alert-info alert-dismissible py-1" role="alert"><i class="ti ti-refresh me-1"></i>Refreshing...<a href="#" class="btn-close" data-bs-dismiss="alert"></a></div>`)
}

// HandleInlineSearch searches messages and returns the message list partial
func (s *Server) HandleInlineSearch(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query().Get("q")
	if q == "" {
		// Empty query — return full list
		s.HandleMessagesList(w, r)
		return
	}

	messages, total, err := s.database.GetMessagesByUserFiltered(user.ID, "", 0, q, 50, 0)
	if err != nil {
		log.Printf("Error searching messages: %v", err)
		http.Error(w, "Search error", http.StatusInternalServerError)
		return
	}

	s.renderMessageList(w, messages, 1, total, 50, "", 0, q)
}

// renderMessageList renders the "message-list" template block with pagination state.
func (s *Server) renderMessageList(w http.ResponseWriter, messages []*models.Message, page, total, pageSize int, folder string, accountID int64, query string) {
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	data := struct {
		Messages    []*models.Message
		CurrentPage int
		TotalPages  int
		HasPrev     bool
		HasNext     bool
		PrevPage    int
		NextPage    int
		Folder      string
		AccountID   int64
		Query       string
	}{
		Messages:    messages,
		CurrentPage: page,
		TotalPages:  totalPages,
		HasPrev:     page > 1,
		HasNext:     page < totalPages,
		PrevPage:    page - 1,
		NextPage:    page + 1,
		Folder:      folder,
		AccountID:   accountID,
		Query:       query,
	}

	s.renderTemplatePartial(w, "inbox.html", "message-list", data)
}

// stripHTMLSimple removes HTML tags (basic implementation for quoting)
func stripHTMLSimple(html string) string {
	var result strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			result.WriteRune(r)
		}
	}
	return result.String()
}
