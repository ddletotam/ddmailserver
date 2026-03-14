package web

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/db"
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
	Accounts              []*models.Account
	GoogleOAuthEnabled    bool
	MicrosoftOAuthEnabled bool
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

type ComposeData struct {
	PageData
	Accounts []*models.Account
	To       string
	Subject  string
	Body     string
	ShowCc   bool
	ShowBcc  bool
}

// getUserLanguage extracts user's language preference from template data
func (s *Server) getUserLanguage(data interface{}) string {
	// Try to extract User from common data structures
	switch d := data.(type) {
	case PageData:
		if d.User != nil && d.User.Language != "" {
			return d.User.Language
		}
	case DashboardData:
		if d.User != nil && d.User.Language != "" {
			return d.User.Language
		}
	case AccountsData:
		if d.User != nil && d.User.Language != "" {
			return d.User.Language
		}
	case InboxData:
		if d.User != nil && d.User.Language != "" {
			return d.User.Language
		}
	case MessageData:
		if d.User != nil && d.User.Language != "" {
			return d.User.Language
		}
	case ComposeData:
		if d.User != nil && d.User.Language != "" {
			return d.User.Language
		}
	case map[string]interface{}:
		if user, ok := d["User"].(*models.User); ok && user != nil && user.Language != "" {
			return user.Language
		}
	default:
		// Use reflection for anonymous structs
		if lang := s.extractLanguageViaReflection(data); lang != "" {
			return lang
		}
	}
	return "en" // default to English
}

// extractLanguageViaReflection extracts user language from anonymous structs using reflection
func (s *Server) extractLanguageViaReflection(data interface{}) string {
	v := reflect.ValueOf(data)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}

	// Try to find User field
	userField := v.FieldByName("User")
	if !userField.IsValid() {
		// Try PageData embedded struct
		pageDataField := v.FieldByName("PageData")
		if pageDataField.IsValid() && pageDataField.Kind() == reflect.Struct {
			userField = pageDataField.FieldByName("User")
		}
	}

	if userField.IsValid() && !userField.IsNil() {
		if user, ok := userField.Interface().(*models.User); ok && user != nil && user.Language != "" {
			return user.Language
		}
	}

	return ""
}

// Helper function to render templates
func (s *Server) renderTemplate(w http.ResponseWriter, templateName string, data interface{}) {
	// Get user's language preference
	userLang := s.getUserLanguage(data)
	i18n := s.i18nManager.Get(userLang)

	// Add template functions
	funcMap := template.FuncMap{
		"t": i18n.T, // Translation function using user's language
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

	data := AccountsData{
		PageData: PageData{
			Title: "Email Accounts",
			User:  user,
		},
		GoogleOAuthEnabled:    s.googleOAuth != nil,
		MicrosoftOAuthEnabled: s.microsoftOAuth != nil,
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

	// Use the standard renderTemplate method to get full layout with styles
	s.renderTemplate(w, "account_form.html", data)
}

// HandleSaveAccount handles the account form submission (HTMX endpoint)
func (s *Server) HandleSaveAccount(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	var account models.Account

	// Check if editing existing account
	if idStr := r.FormValue("id"); idStr != "" {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		existing, err := s.database.GetAccountByID(id)
		if err != nil || existing.UserID != user.ID {
			http.Error(w, "Account not found", http.StatusNotFound)
			return
		}
		account = *existing
	}

	// Parse form values
	account.UserID = user.ID
	account.Name = r.FormValue("name")
	account.Email = r.FormValue("email")
	account.IMAPHost = r.FormValue("imap_host")
	account.IMAPUsername = r.FormValue("imap_username")
	if pwd := r.FormValue("imap_password"); pwd != "" {
		account.IMAPPassword = pwd
	}
	account.IMAPTLS = r.FormValue("imap_tls") == "true"
	account.SMTPHost = r.FormValue("smtp_host")
	account.SMTPUsername = r.FormValue("smtp_username")
	if pwd := r.FormValue("smtp_password"); pwd != "" {
		account.SMTPPassword = pwd
	}
	account.SMTPTLS = r.FormValue("smtp_tls") == "true"
	account.Enabled = true

	// Parse ports
	if imapPort := r.FormValue("imap_port"); imapPort != "" {
		if port, err := strconv.Atoi(imapPort); err == nil {
			account.IMAPPort = port
		}
	}
	if smtpPort := r.FormValue("smtp_port"); smtpPort != "" {
		if port, err := strconv.Atoi(smtpPort); err == nil {
			account.SMTPPort = port
		}
	}

	// Validate
	if account.Name == "" || account.Email == "" {
		http.Error(w, "Name and email are required", http.StatusBadRequest)
		return
	}
	if account.IMAPHost == "" || account.IMAPPort == 0 {
		http.Error(w, "IMAP server and port are required", http.StatusBadRequest)
		return
	}
	if account.SMTPHost == "" || account.SMTPPort == 0 {
		http.Error(w, "SMTP server and port are required", http.StatusBadRequest)
		return
	}

	// Save account
	var err error
	if account.ID > 0 {
		err = s.database.UpdateAccount(&account)
	} else {
		err = s.database.CreateAccount(&account)
	}

	if err != nil {
		log.Printf("Failed to save account: %v", err)
		http.Error(w, "Failed to save account", http.StatusInternalServerError)
		return
	}

	log.Printf("Account saved: %s for user %d", account.Email, user.ID)

	// Redirect to accounts page
	w.Header().Set("HX-Redirect", "/accounts")
	w.WriteHeader(http.StatusOK)
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

	// Check ownership - prevent reading other users' messages
	if message.UserID != user.ID {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}

	// Get attachments
	attachments, err := s.database.GetAttachmentsByMessageID(id)
	if err != nil {
		log.Printf("Failed to get attachments for message %d: %v", id, err)
		// Continue without attachments
	}

	// Replace cid: URLs with actual attachment URLs
	if message.BodyHTML != "" {
		message.BodyHTML = replaceCIDURLs(message.BodyHTML, message.ID)
	}

	// Mark as read
	message.Seen = true
	if err := s.database.UpdateMessage(message); err != nil {
		log.Printf("Failed to mark message %d as read: %v", id, err)
	}

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

// HandleSettingsPage shows the settings page
func (s *Server) HandleSettingsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get user's language preference (default to English)
	language := user.Language
	if language == "" {
		language = "en"
	}

	// Get OAuth settings for admin
	var oauthSettings *db.GoogleOAuthSettings
	var microsoftOAuthSettings *db.MicrosoftOAuthSettings
	var redirectURI, microsoftRedirectURI string
	if user.IsAdmin() {
		oauthSettings, _ = s.database.GetGoogleOAuthSettings()
		if oauthSettings == nil {
			oauthSettings = &db.GoogleOAuthSettings{}
		}
		microsoftOAuthSettings, _ = s.database.GetMicrosoftOAuthSettings()
		if microsoftOAuthSettings == nil {
			microsoftOAuthSettings = &db.MicrosoftOAuthSettings{}
		}
		// Build default redirect URIs from request
		scheme := "https"
		host := r.Host
		// Check for reverse proxy headers
		if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
			host = fwdHost
		}
		if fwdProto := r.Header.Get("X-Forwarded-Proto"); fwdProto != "" {
			scheme = fwdProto
		} else if r.TLS == nil && (strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1")) {
			scheme = "http"
		}
		redirectURI = fmt.Sprintf("%s://%s/oauth/google/callback", scheme, host)
		microsoftRedirectURI = fmt.Sprintf("%s://%s/oauth/microsoft/callback", scheme, host)
	}

	data := struct {
		PageData
		Language               string
		OAuthSettings          *db.GoogleOAuthSettings
		MicrosoftOAuthSettings *db.MicrosoftOAuthSettings
		RedirectURI            string
		MicrosoftRedirectURI   string
		GoogleOAuthEnabled     bool
		MicrosoftOAuthEnabled  bool
	}{
		PageData: PageData{
			Title: "Settings",
			User:  user,
		},
		Language:               language,
		OAuthSettings:          oauthSettings,
		MicrosoftOAuthSettings: microsoftOAuthSettings,
		RedirectURI:            redirectURI,
		MicrosoftRedirectURI:   microsoftRedirectURI,
		GoogleOAuthEnabled:     s.googleOAuth != nil,
		MicrosoftOAuthEnabled:  s.microsoftOAuth != nil,
	}

	s.renderTemplate(w, "settings.html", data)
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
