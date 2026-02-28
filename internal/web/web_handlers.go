package web

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
)

//go:embed templates/*
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Template data structures
type PageData struct {
	Title        string
	User         *models.User
	FlashSuccess string
	FlashError   string
}

type DashboardData struct {
	PageData
	AccountCount int
	MessageCount int
	UnreadCount  int
}

type AccountsData struct {
	PageData
	Accounts []*models.Account
}

type InboxData struct {
	PageData
	Messages    []*models.Message
	Accounts    []*models.Account
	UnreadCount int
	Folder      string
}

type MessageData struct {
	PageData
	Message     *models.Message
	Attachments []*models.Attachment
}

type ComposeData struct{
	PageData
	Accounts []*models.Account
	To       string
	Subject  string
	Body     string
}

// Helper function to render templates
func (s *Server) renderTemplate(w http.ResponseWriter, templateName string, data interface{}) {
	// Add template functions
	funcMap := template.FuncMap{
		"t": s.i18n.T, // Translation function
		"substr": func(s string, start, end int) string {
			if len(s) < end {
				return s
			}
			return s[start:end]
		},
		"formatSize": func(size int64) string {
			const unit = 1024
			if size < unit {
				return fmt.Sprintf("%d B", size)
			}
			div, exp := int64(unit), 0
			for n := size / unit; n >= unit; n /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/layout.html", "templates/"+templateName)
	if err != nil {
		log.Printf("Error parsing template %s: %v", templateName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("Error executing template %s: %v", templateName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// HandleIndex redirects to dashboard or login
func (s *Server) HandleIndex(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// HandleLoginPage shows the login/register page
func (s *Server) HandleLoginPage(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title: "Login",
	}
	s.renderTemplate(w, "login.html", data)
}

// HandleDashboard shows the dashboard
func (s *Server) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get stats
	accounts, _ := s.database.GetAccountsByUserID(user.ID)
	// TODO: Get actual message counts
	messageCount := 0
	unreadCount := 0

	data := DashboardData{
		PageData: PageData{
			Title: "Dashboard",
			User:  user,
		},
		AccountCount: len(accounts),
		MessageCount: messageCount,
		UnreadCount:  unreadCount,
	}

	s.renderTemplate(w, "dashboard.html", data)
}

// HandleAccountsPage shows the accounts management page
func (s *Server) HandleAccountsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	data := PageData{
		Title: "Email Accounts",
		User:  user,
	}

	s.renderTemplate(w, "accounts.html", data)
}

// HandleAccountsList returns the accounts list (htmx endpoint)
func (s *Server) HandleAccountsList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	accounts, err := s.database.GetAccountsByUserID(user.ID)
	if err != nil {
		log.Printf("Error getting accounts: %v", err)
		http.Error(w, "Error loading accounts", http.StatusInternalServerError)
		return
	}

	data := AccountsData{
		Accounts: accounts,
	}

	// Render just the accounts-list template
	funcMap := template.FuncMap{
		"t": s.i18n.T,
	}
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/accounts.html")
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "accounts-list", data); err != nil {
		log.Printf("Error executing template: %v", err)
	}
}

// HandleAccountFormPage shows the add/edit account form
func (s *Server) HandleAccountFormPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	vars := mux.Vars(r)
	idStr := vars["id"]

	var account *models.Account
	if idStr != "" {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		account, _ = s.database.GetAccountByID(id)
	}

	data := struct {
		PageData
		Account *models.Account
	}{
		PageData: PageData{
			Title: "Add/Edit Account",
			User:  user,
		},
		Account: account,
	}

	// Render the form template
	funcMap := template.FuncMap{
		"t": s.i18n.T,
	}
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/layout.html", "templates/accounts.html")
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "account-form", data); err != nil {
		log.Printf("Error executing template: %v", err)
	}
}

// HandleInboxPage shows the inbox
func (s *Server) HandleInboxPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	accounts, _ := s.database.GetAccountsByUserID(user.ID)
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		folder = "all"
	}

	data := InboxData{
		PageData: PageData{
			Title: "Inbox",
			User:  user,
		},
		Accounts:    accounts,
		Folder:      folder,
		UnreadCount: 0, // TODO: Get actual count
	}

	s.renderTemplate(w, "inbox.html", data)
}

// HandleMessagesList returns the messages list (htmx endpoint)
func (s *Server) HandleMessagesList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get messages
	messages, err := s.database.GetMessagesByUser(user.ID, 50, 0)
	if err != nil {
		log.Printf("Error getting messages: %v", err)
		http.Error(w, "Error loading messages", http.StatusInternalServerError)
		return
	}

	data := struct {
		Messages    []*models.Message
		CurrentPage int
		TotalPages  int
		HasPrev     bool
		HasNext     bool
		PrevPage    int
		NextPage    int
	}{
		Messages:    messages,
		CurrentPage: 1,
		TotalPages:  1,
		HasPrev:     false,
		HasNext:     false,
	}

	// Render just the message-list template
	funcMap := template.FuncMap{
		"t": s.i18n.T,
		"substr": func(s string, start, end int) string {
			if len(s) < end {
				return s
			}
			return s[start:end]
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/inbox.html")
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "message-list", data); err != nil {
		log.Printf("Error executing template: %v", err)
	}
}

// HandleMessagePage shows a single message
func (s *Server) HandleMessagePage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	vars := mux.Vars(r)
	idStr := vars["id"]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid message ID", http.StatusBadRequest)
		return
	}

	message, err := s.database.GetMessageByID(id)
	if err != nil {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	// Get attachments
	attachments, _ := s.database.GetAttachmentsByMessageID(id)

	// Mark as read
	message.Seen = true
	s.database.UpdateMessage(message)

	data := MessageData{
		PageData: PageData{
			Title: message.Subject,
			User:  user,
		},
		Message:     message,
		Attachments: attachments,
	}

	s.renderTemplate(w, "message.html", data)
}

// HandleComposePage shows the compose email page
func (s *Server) HandleComposePage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	accounts, _ := s.database.GetAccountsByUserID(user.ID)

	data := ComposeData{
		PageData: PageData{
			Title: "Compose Email",
			User:  user,
		},
		Accounts: accounts,
	}

	s.renderTemplate(w, "compose.html", data)
}

// HandleLogout logs out the user
func (s *Server) HandleLogout(w http.ResponseWriter, r *http.Request) {
	// Clear session cookie
	http.SetCookie(w, &http.Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
