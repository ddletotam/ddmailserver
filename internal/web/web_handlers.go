package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	imapclient "github.com/yourusername/mailserver/internal/imap/client"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/timeutil"
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

// UserLanguage returns the user's preferred language code, or empty if unknown.
// Implementing this on PageData makes it auto-available on any data struct that
// embeds PageData (DashboardData, AccountsData, etc.).
func (p PageData) UserLanguage() string {
	if p.User != nil {
		return p.User.Language
	}
	return ""
}

// userLanguageProvider is the optional interface checked by getUserLanguage.
type userLanguageProvider interface {
	UserLanguage() string
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
	Accounts          []*models.Account
	To                string
	Cc                string
	Bcc               string
	Subject           string
	Body              string
	DraftID           int64
	SelectedAccountID int64
}

// getUserLanguage extracts the user's preferred language from template data.
// Any data struct that embeds PageData satisfies userLanguageProvider via the
// promoted UserLanguage() method, so a single type assertion handles them all.
func (s *Server) getUserLanguage(data interface{}) string {
	if p, ok := data.(userLanguageProvider); ok {
		if lang := p.UserLanguage(); lang != "" {
			return lang
		}
	}
	if m, ok := data.(map[string]interface{}); ok {
		if user, ok := m["User"].(*models.User); ok && user != nil && user.Language != "" {
			return user.Language
		}
	}
	return "en"
}

// Helper function to render templates
// buildFuncMap returns the template function map (i18n + helpers) for a request.
// Used by both renderTemplate and renderTemplatePartial so they share the same
// available functions.
func (s *Server) buildFuncMap(data interface{}) template.FuncMap {
	userLang := s.getUserLanguage(data)
	i18n := s.i18nManager.Get(userLang)
	return template.FuncMap{
		"t": i18n.T,
		"substr": func(str string, start, end int) string {
			if len(str) < end {
				return str
			}
			return str[start:end]
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
		// fmtTime formats a unix-ms timestamp. Templates used to call
		// `.Field.Format "..."` when these fields were time.Time; after the
		// 693f9ae migration to int64 ms those calls panic the template and
		// return 500. Existing pages should swap to `{{fmtTime .Field "..."}}`.
		// Returns "—" for zero so an unset timestamp doesn't render as 1970.
		"fmtTime": func(ms int64, layout string) string {
			if ms == 0 {
				return "—"
			}
			return timeutil.FromMs(ms).Local().Format(layout)
		},
		"add": func(a, b int) int { return a + b },
		"list": func(items ...interface{}) []interface{} {
			return items
		},
		"in": func(needle string, haystack []string) bool {
			for _, h := range haystack {
				if h == needle {
					return true
				}
			}
			return false
		},
	}
}

// renderTemplate renders a full page (templateName wrapped in layout.html).
func (s *Server) renderTemplate(w http.ResponseWriter, templateName string, data interface{}) {
	tmpl, err := template.New("").Funcs(s.buildFuncMap(data)).ParseFS(templatesFS, "templates/layout.html", "templates/"+templateName)
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
	messageCount, _ := s.database.GetMessageCountByUser(user.ID)
	unreadCount, _ := s.database.GetUnreadCountByUser(user.ID)
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
	tmpl, err := template.New("").Funcs(s.buildFuncMap(data)).ParseFS(templatesFS, "templates/accounts.html")
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

	// Aliases (raw textarea content; parsed lazily by Account.GetAliases)
	account.Aliases = strings.TrimSpace(r.FormValue("aliases"))
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
	tmpl, err := template.New("").Funcs(s.buildFuncMap(data)).ParseFS(templatesFS, "templates/accounts.html")
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

	unreadCount, _ := s.database.GetUnreadCountByUser(user.ID)

	data := InboxData{
		PageData: PageData{
			Title: "Inbox",
			User:  user,
		},
		Accounts:    accounts,
		Folder:      folder,
		UnreadCount: unreadCount,
	}

	s.renderTemplate(w, "inbox.html", data)
}

// HandleMessagesList returns the messages list with filtering and pagination (htmx endpoint)
func (s *Server) HandleMessagesList(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	folder := r.URL.Query().Get("folder")
	q := r.URL.Query().Get("q")
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	var accountID int64
	if v := r.URL.Query().Get("account"); v != "" {
		accountID, _ = strconv.ParseInt(v, 10, 64)
	}

	const pageSize = 50
	offset := (page - 1) * pageSize

	messages, total, err := s.database.GetMessagesByUserFiltered(user.ID, folder, accountID, q, pageSize, offset)
	if err != nil {
		log.Printf("Error getting messages: %v", err)
		http.Error(w, "Error loading messages", http.StatusInternalServerError)
		return
	}

	s.renderMessageList(w, messages, page, total, pageSize, folder, accountID, q)
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

	message, err := s.database.GetMessageByIDForUser(id, user.ID)
	if err != nil {
		http.Redirect(w, r, "/inbox?error=message_not_found", http.StatusSeeOther)
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

// HandleComposePage shows the compose email page, optionally loading a draft
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

	// Load draft if requested
	if draftIDStr := r.URL.Query().Get("draft_id"); draftIDStr != "" {
		draftID, _ := strconv.ParseInt(draftIDStr, 10, 64)
		if draftID > 0 {
			draft, err := s.database.GetMessageByID(draftID)
			if err == nil && draft.UserID == user.ID && draft.Draft {
				data.DraftID = draft.ID
				data.To = draft.To
				data.Cc = draft.Cc
				data.Bcc = draft.Bcc
				data.Subject = draft.Subject
				data.SelectedAccountID = draft.AccountID
				if draft.BodyHTML != "" {
					data.Body = draft.BodyHTML
				} else {
					data.Body = draft.Body
				}
			}
		}
	}

	s.renderTemplate(w, "compose.html", data)
}

// HandleSettingsPage shows the per-user settings page (profile, language,
// password, danger zone). Installation/server-wide settings live on /admin.
func (s *Server) HandleSettingsPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	language := user.Language
	if language == "" {
		language = "en"
	}

	data := struct {
		PageData
		Language string
	}{
		PageData: PageData{
			Title: "Settings",
			User:  user,
		},
		Language: language,
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
