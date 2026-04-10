package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/db"
	imapclient "github.com/yourusername/mailserver/internal/imap/client"
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
	AccountCount      int
	MessageCount      int
	UnreadCount       int
	CalendarCount     int
	ErrorAccountCount int
}

type AccountsData struct {
	PageData
	Accounts              []*models.Account
	GoogleOAuthEnabled    bool
	MicrosoftOAuthEnabled bool
	SyncIntervalSec       int
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
	calendars, _ := s.database.GetCalendarsByUserID(user.ID)

	errorCount := 0
	for _, acc := range accounts {
		if acc.HasSyncError() {
			errorCount++
		}
	}

	data := DashboardData{
		PageData: PageData{
			Title: "Dashboard",
			User:  user,
		},
		AccountCount:      len(accounts),
		MessageCount:      messageCount,
		UnreadCount:       unreadCount,
		CalendarCount:     len(calendars),
		ErrorAccountCount: errorCount,
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
		SyncIntervalSec:       s.syncIntervalSec,
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
		acc, err := s.database.GetAccountByID(id)
		if err != nil || acc.UserID != user.ID {
			http.Redirect(w, r, "/accounts", http.StatusSeeOther)
			return
		}
		account = acc
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
	if account.ID == 0 {
		// New accounts start enabled
		account.Enabled = true
	}
	// else: preserve existing Enabled state for edits

	// Parse sync settings
	if syncMode := r.FormValue("sync_mode"); syncMode == "poll" {
		account.SyncMode = "poll"
	} else {
		account.SyncMode = "idle"
	}
	if pi := r.FormValue("poll_interval"); pi != "" {
		if v, err := strconv.Atoi(pi); err == nil && v >= 120 {
			account.PollInterval = v
		} else {
			account.PollInterval = 300
		}
	} else if account.PollInterval < 120 {
		account.PollInterval = 300
	}

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

	// Clear sync errors on edit — credentials may have changed
	if account.ID > 0 {
		_ = s.database.ClearAccountSyncError(account.ID)
	}

	// Redirect to accounts page
	w.Header().Set("HX-Redirect", "/accounts")
	w.WriteHeader(http.StatusOK)
}

// HandleAccountLogsPage shows logs for an account
func (s *Server) HandleAccountLogsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	vars := mux.Vars(r)
	id, _ := strconv.ParseInt(vars["id"], 10, 64)
	account, err := s.database.GetAccountByID(id)
	if err != nil || account.UserID != user.ID {
		http.Error(w, "Account not found", http.StatusNotFound)
		return
	}

	errorsOnly := r.URL.Query().Get("errors") == "1"
	logs, err := s.database.GetAccountLogs(account.ID, errorsOnly, 500)
	if err != nil {
		log.Printf("Failed to get account logs: %v", err)
	}

	data := struct {
		PageData
		Account    *models.Account
		Logs       []*models.AccountLog
		ErrorsOnly bool
	}{
		PageData: PageData{
			Title: "Account Logs",
			User:  user,
		},
		Account:    account,
		Logs:       logs,
		ErrorsOnly: errorsOnly,
	}

	s.renderTemplate(w, "account_logs.html", data)
}

// HandleSyncAccountWeb triggers immediate IMAP sync for an account
func (s *Server) HandleSyncAccountWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	account, err := s.database.GetAccountByID(id)
	if err != nil || account == nil || account.UserID != user.ID {
		http.Error(w, "Account not found", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	task := imapclient.NewSyncTask(account, s.database)
	syncErr := task.Execute(ctx)

	w.Header().Set("Content-Type", "text/html")
	if syncErr != nil {
		log.Printf("Manual IMAP sync failed for %s: %v", account.Email, syncErr)
		fmt.Fprintf(w, `<span class="badge bg-danger" title="%s">Sync failed</span>`,
			template.HTMLEscapeString(syncErr.Error()))
	} else {
		fmt.Fprintf(w, `<span class="badge bg-success">Synced</span>`)
	}
}

// HandleToggleAccountWeb enables/disables an account
func (s *Server) HandleToggleAccountWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	account, err := s.database.GetAccountByID(id)
	if err != nil || account == nil || account.UserID != user.ID {
		http.Error(w, "Account not found", http.StatusNotFound)
		return
	}

	account.Enabled = !account.Enabled
	if err := s.database.UpdateAccount(account); err != nil {
		http.Error(w, "Failed to update account", http.StatusInternalServerError)
		return
	}

	// Return updated list via HTMX
	accounts, err := s.database.GetAccountsByUserID(user.ID)
	if err != nil {
		http.Error(w, "Failed to load accounts", http.StatusInternalServerError)
		return
	}

	data := AccountsData{Accounts: accounts}
	funcMap := template.FuncMap{"t": s.i18n.T}
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/accounts.html")
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	tmpl.ExecuteTemplate(w, "accounts-list", data)
}

// HandleDeleteAccountWeb deletes an account
func (s *Server) HandleDeleteAccountWeb(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	account, err := s.database.GetAccountByID(id)
	if err != nil || account == nil || account.UserID != user.ID {
		http.Error(w, "Account not found", http.StatusNotFound)
		return
	}

	if err := s.database.DeleteAccount(id); err != nil {
		http.Error(w, "Failed to delete account", http.StatusInternalServerError)
		return
	}

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
	if err != nil || message.UserID != user.ID {
		http.Redirect(w, r, "/inbox?error=message_not_found", http.StatusSeeOther)
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
