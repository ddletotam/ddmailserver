package web

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	caldavserver "github.com/yourusername/mailserver/internal/caldav/server"
	carddavserver "github.com/yourusername/mailserver/internal/carddav/server"
	"github.com/yourusername/mailserver/internal/config"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/oauth"
	"github.com/yourusername/mailserver/internal/search"
)

// Server represents the web server
type Server struct {
	database        *db.DB
	jwtSecret       string
	router          *mux.Router
	addr            string
	server          *http.Server
	i18n            *I18n        // default i18n (for backward compatibility)
	i18nManager     *I18nManager // manages all locales
	authRateLimiter *RateLimiter
	oauthConfig     *config.OAuthConfig
	googleOAuth     *oauth.GoogleOAuth
	microsoftOAuth  *oauth.MicrosoftOAuth
	caldavServer    *caldavserver.Server
	carddavServer   *carddavserver.Server
	searchIndexer   *search.Indexer
}

// New creates a new web server
func New(database *db.DB, jwtSecret string, host string, port int, locale string, oauthConfig *config.OAuthConfig) *Server {
	addr := fmt.Sprintf("%s:%d", host, port)

	// Initialize i18n manager with all locales
	i18nManager := NewI18nManager()

	// Default locale for backward compatibility
	if locale == "" {
		locale = "en"
	}

	s := &Server{
		database:        database,
		jwtSecret:       jwtSecret,
		router:          mux.NewRouter(),
		addr:            addr,
		i18n:            i18nManager.Get(locale), // default i18n
		i18nManager:     i18nManager,
		authRateLimiter: NewRateLimiter(5, time.Minute), // 5 attempts per minute
		oauthConfig:     oauthConfig,
		caldavServer:    caldavserver.New(database, "/caldav/"),
		carddavServer:   carddavserver.New(database, "/carddav/"),
	}

	// Initialize Google OAuth - check config.yaml first, then database
	if oauthConfig != nil && oauthConfig.IsGoogleOAuthConfigured() {
		s.googleOAuth = oauth.NewGoogleOAuth(&oauthConfig.Google)
		log.Printf("Google OAuth configured from config file with redirect URI: %s", oauthConfig.Google.RedirectURI)
	} else {
		// Try to load from database
		s.initGoogleOAuthFromDB()
	}

	// Initialize Microsoft OAuth - check config.yaml first, then database
	if oauthConfig != nil && oauthConfig.IsMicrosoftOAuthConfigured() {
		s.microsoftOAuth = oauth.NewMicrosoftOAuth(&oauthConfig.Microsoft)
		log.Printf("Microsoft OAuth configured from config file with redirect URI: %s", oauthConfig.Microsoft.RedirectURI)
	} else {
		// Try to load from database
		s.initMicrosoftOAuthFromDB()
	}

	s.setupRoutes()

	s.server = &http.Server{
		Addr:    addr,
		Handler: s.router,
	}

	log.Printf("Web server created, will listen on %s (default locale: %s)", addr, locale)

	return s
}

// setupRoutes configures all HTTP routes
func (s *Server) setupRoutes() {
	// Apply global middleware
	s.router.Use(s.LoggingMiddleware)
	s.router.Use(s.CORSMiddleware)
	s.router.Use(s.SessionMiddleware)

	// Serve static files
	staticFiles, _ := fs.Sub(staticFS, "static")
	s.router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))

	// Public web routes
	s.router.HandleFunc("/", s.HandleIndex).Methods("GET")
	s.router.HandleFunc("/login", s.HandleLoginPage).Methods("GET")
	s.router.HandleFunc("/logout", s.HandleLogout).Methods("GET")
	s.router.HandleFunc("/forgot-password", s.HandleForgotPasswordPage).Methods("GET")

	// Public API routes
	s.router.HandleFunc("/health", s.HandleHealthCheck).Methods("GET")

	// Auth routes with rate limiting (5 attempts per minute per IP)
	authRouter := s.router.PathPrefix("/api").Subrouter()
	authRouter.Use(s.RateLimitMiddleware(s.authRateLimiter))
	authRouter.HandleFunc("/register", s.HandleRegister).Methods("POST")
	authRouter.HandleFunc("/login", s.HandleLogin).Methods("POST")
	authRouter.HandleFunc("/forgot-password", s.HandleForgotPassword).Methods("POST")

	// OAuth routes (requires auth - user must be logged in to add accounts)
	// Always register routes - they check if OAuth is configured internally
	oauthRouter := s.router.PathPrefix("/oauth").Subrouter()
	oauthRouter.Use(s.WebAuthMiddleware)
	oauthRouter.HandleFunc("/google/start", s.HandleGoogleOAuthStart).Methods("GET")
	oauthRouter.HandleFunc("/google/callback", s.HandleGoogleOAuthCallback).Methods("GET")
	oauthRouter.HandleFunc("/google/calendar/start", s.HandleGoogleCalendarOAuthStart).Methods("GET")
	oauthRouter.HandleFunc("/google/calendar/callback", s.HandleGoogleCalendarOAuthCallback).Methods("GET")
	oauthRouter.HandleFunc("/microsoft/start", s.HandleMicrosoftOAuthStart).Methods("GET")
	oauthRouter.HandleFunc("/microsoft/callback", s.HandleMicrosoftOAuthCallback).Methods("GET")

	// Settings API (uses session cookie auth, must be registered before general API routes)
	settingsAPI := s.router.PathPrefix("/api/settings").Subrouter()
	settingsAPI.Use(s.APIAuthMiddleware) // Returns JSON error instead of redirect
	settingsAPI.HandleFunc("/password", s.HandleChangePassword).Methods("POST")
	settingsAPI.HandleFunc("/language", s.HandleChangeLanguage).Methods("POST")
	settingsAPI.HandleFunc("/account", s.HandleDeleteUserAccount).Methods("DELETE")
	settingsAPI.HandleFunc("/oauth/google", s.HandleSaveGoogleOAuthSettings).Methods("POST")
	settingsAPI.HandleFunc("/oauth/microsoft", s.HandleSaveMicrosoftOAuthSettings).Methods("POST")

	// Accounts API (uses session cookie auth, must be registered before general API routes)
	accountsAPI := s.router.PathPrefix("/api/accounts").Subrouter()
	accountsAPI.Use(s.APIAuthMiddleware) // Returns JSON error instead of redirect
	accountsAPI.HandleFunc("", s.HandleGetAccounts).Methods("GET")
	accountsAPI.HandleFunc("", s.HandleCreateAccount).Methods("POST")
	accountsAPI.HandleFunc("/{id}", s.HandleGetAccount).Methods("GET")
	accountsAPI.HandleFunc("/{id}", s.HandleUpdateAccount).Methods("PUT")
	accountsAPI.HandleFunc("/{id}", s.HandleDeleteAccount).Methods("DELETE")

	// Protected API routes (uses JWT token auth)
	api := s.router.PathPrefix("/api").Subrouter()
	api.Use(s.AuthMiddleware)

	// CalDAV server (uses Basic Auth, handles its own authentication)
	// MUST be registered BEFORE the catch-all "/" web routes
	s.router.HandleFunc("/.well-known/caldav", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" || r.Method == "REPORT" {
			// For WebDAV methods, rewrite path and proxy to CalDAV server
			r.URL.Path = "/caldav/"
			s.caldavServer.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/caldav/", http.StatusMovedPermanently)
	})
	s.router.PathPrefix("/caldav/").Handler(s.caldavServer)

	// CardDAV server (uses Basic Auth, handles its own authentication)
	s.router.HandleFunc("/.well-known/carddav", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" || r.Method == "REPORT" {
			// For WebDAV methods, rewrite path and proxy to CardDAV server
			r.URL.Path = "/carddav/"
			s.carddavServer.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/carddav/", http.StatusMovedPermanently)
	})
	s.router.PathPrefix("/carddav/").Handler(s.carddavServer)

	// Protected web routes
	web := s.router.PathPrefix("/").Subrouter()
	web.Use(s.WebAuthMiddleware)

	// Dashboard
	web.HandleFunc("/dashboard", s.HandleDashboard).Methods("GET")

	// Recovery key display (after registration)
	web.HandleFunc("/recovery-key", s.HandleShowRecoveryKeyPage).Methods("GET")

	// Accounts web pages
	web.HandleFunc("/accounts", s.HandleAccountsPage).Methods("GET")
	web.HandleFunc("/accounts/list", s.HandleAccountsList).Methods("GET")
	web.HandleFunc("/accounts/new", s.HandleAccountFormPage).Methods("GET")
	web.HandleFunc("/accounts/save", s.HandleSaveAccount).Methods("POST")
	web.HandleFunc("/accounts/{id}/edit", s.HandleAccountFormPage).Methods("GET")

	// Inbox
	web.HandleFunc("/inbox", s.HandleInboxPage).Methods("GET")
	web.HandleFunc("/messages/list", s.HandleMessagesList).Methods("GET")
	web.HandleFunc("/messages/{id}", s.HandleMessagePage).Methods("GET")
	web.HandleFunc("/messages/send", s.HandleSendMessage).Methods("POST")
	web.HandleFunc("/message/{id}", s.HandleMessagePage).Methods("GET")

	// Compose
	web.HandleFunc("/compose", s.HandleComposePage).Methods("GET")

	// Settings
	web.HandleFunc("/settings", s.HandleSettingsPage).Methods("GET")

	// Domains (for MX server)
	web.HandleFunc("/domains", s.HandleDomainsPage).Methods("GET")

	// Calendars web pages
	web.HandleFunc("/calendars", s.HandleCalendarsPage).Methods("GET")
	web.HandleFunc("/calendars/sources/list", s.HandleCalendarSourcesList).Methods("GET")
	web.HandleFunc("/calendars/list", s.HandleCalendarsList).Methods("GET")
	web.HandleFunc("/calendars/sources/create", s.HandleCreateCalendarSourceWeb).Methods("POST")
	web.HandleFunc("/calendars/sources/create-ics-url", s.HandleCreateICSURLSource).Methods("POST")
	web.HandleFunc("/calendars/create", s.HandleCreateLocalCalendar).Methods("POST")
	web.HandleFunc("/calendars/sources/{id}/sync", s.HandleSyncCalendarSource).Methods("POST")
	web.HandleFunc("/calendars/sources/{id}", s.HandleDeleteCalendarSourceWeb).Methods("DELETE")
	web.HandleFunc("/calendars/{id}", s.HandleDeleteCalendarWeb).Methods("DELETE")
	web.HandleFunc("/calendars/import", s.HandleImportICSWeb).Methods("POST")

	// Contacts web pages
	web.HandleFunc("/contacts", s.HandleContactsPage).Methods("GET")
	web.HandleFunc("/contacts/sources/list", s.HandleContactSourcesList).Methods("GET")
	web.HandleFunc("/contacts/addressbooks/list", s.HandleAddressBooksList).Methods("GET")
	web.HandleFunc("/contacts/search", s.HandleContactsList).Methods("GET")
	web.HandleFunc("/contacts/sources/create", s.HandleCreateContactSourceWeb).Methods("POST")
	web.HandleFunc("/contacts/addressbooks/create", s.HandleCreateLocalAddressBook).Methods("POST")
	web.HandleFunc("/contacts/sources/{id}/sync", s.HandleSyncContactSource).Methods("POST")
	web.HandleFunc("/contacts/sources/{id}", s.HandleDeleteContactSourceWeb).Methods("DELETE")
	web.HandleFunc("/contacts/addressbooks/{id}", s.HandleDeleteAddressBookWeb).Methods("DELETE")

	// Vault (soft-deleted messages)
	web.HandleFunc("/vault", s.HandleVaultPage).Methods("GET")
	web.HandleFunc("/vault/restore/{id}", s.HandleRestoreMessage).Methods("POST")
	web.HandleFunc("/vault/delete/{id}", s.HandlePermanentDelete).Methods("DELETE")

	// Search
	web.HandleFunc("/search", s.HandleSearchPage).Methods("GET")

	// Domains API
	domainsAPI := s.router.PathPrefix("/api/domains").Subrouter()
	domainsAPI.Use(s.APIAuthMiddleware)
	domainsAPI.HandleFunc("", s.HandleCreateDomain).Methods("POST")
	domainsAPI.HandleFunc("/{id}", s.HandleDeleteDomain).Methods("DELETE")

	// Mailboxes API
	mailboxesAPI := s.router.PathPrefix("/api/mailboxes").Subrouter()
	mailboxesAPI.Use(s.APIAuthMiddleware)
	mailboxesAPI.HandleFunc("", s.HandleCreateMailbox).Methods("POST")
	mailboxesAPI.HandleFunc("/{id}", s.HandleDeleteMailbox).Methods("DELETE")

	// Calendar Sources API
	calSourcesAPI := s.router.PathPrefix("/api/calendar-sources").Subrouter()
	calSourcesAPI.Use(s.APIAuthMiddleware)
	calSourcesAPI.HandleFunc("", s.HandleGetCalendarSources).Methods("GET")
	calSourcesAPI.HandleFunc("", s.HandleCreateCalendarSource).Methods("POST")
	calSourcesAPI.HandleFunc("/{id}", s.HandleGetCalendarSource).Methods("GET")
	calSourcesAPI.HandleFunc("/{id}", s.HandleUpdateCalendarSource).Methods("PUT")
	calSourcesAPI.HandleFunc("/{id}", s.HandleDeleteCalendarSource).Methods("DELETE")

	// Calendars API
	calendarsAPI := s.router.PathPrefix("/api/calendars").Subrouter()
	calendarsAPI.Use(s.APIAuthMiddleware)
	calendarsAPI.HandleFunc("", s.HandleGetCalendars).Methods("GET")
	calendarsAPI.HandleFunc("/{id}", s.HandleGetCalendar).Methods("GET")
	calendarsAPI.HandleFunc("/{id}", s.HandleUpdateCalendar).Methods("PUT")
	calendarsAPI.HandleFunc("/{id}", s.HandleDeleteCalendar).Methods("DELETE")
	calendarsAPI.HandleFunc("/{id}/events", s.HandleGetEvents).Methods("GET")
	calendarsAPI.HandleFunc("/{id}/events", s.HandleCreateEvent).Methods("POST")
	calendarsAPI.HandleFunc("/{id}/events/with-attendees", s.HandleCreateEventWithAttendees).Methods("POST")
	calendarsAPI.HandleFunc("/{id}/import", s.HandleImportICS).Methods("POST")

	// Events API
	eventsAPI := s.router.PathPrefix("/api/events").Subrouter()
	eventsAPI.Use(s.APIAuthMiddleware)
	eventsAPI.HandleFunc("/{id}", s.HandleGetEvent).Methods("GET")
	eventsAPI.HandleFunc("/{id}", s.HandleUpdateEvent).Methods("PUT")
	eventsAPI.HandleFunc("/{id}", s.HandleDeleteEvent).Methods("DELETE")
	eventsAPI.HandleFunc("/{id}/attendees", s.HandleGetEventAttendees).Methods("GET")
	eventsAPI.HandleFunc("/{id}/attendees", s.HandleUpdateEventAttendees).Methods("PUT")
	eventsAPI.HandleFunc("/{id}/respond", s.HandleRespondToInvite).Methods("POST")
	eventsAPI.HandleFunc("/{id}/send-invites", s.HandleSendInvites).Methods("POST")
	eventsAPI.HandleFunc("/{id}/cancel", s.HandleCancelEvent).Methods("POST")

	log.Printf("Routes configured")
}

// Start starts the web server
func (s *Server) Start() error {
	log.Printf("Starting web server on %s", s.addr)

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

// SetSearchIndexer sets the Meilisearch indexer for full-text search
func (s *Server) SetSearchIndexer(indexer *search.Indexer) {
	s.searchIndexer = indexer
}

// Stop stops the web server
func (s *Server) Stop() error {
	log.Printf("Stopping web server")
	return s.server.Close()
}

// initGoogleOAuthFromDB loads Google OAuth settings from database
func (s *Server) initGoogleOAuthFromDB() {
	settings, err := s.database.GetGoogleOAuthSettings()
	if err != nil {
		log.Printf("Failed to load Google OAuth settings from DB: %v", err)
		return
	}

	if settings.ClientID == "" || settings.ClientSecret == "" {
		return
	}

	s.googleOAuth = oauth.NewGoogleOAuth(&config.GoogleOAuthConfig{
		ClientID:     settings.ClientID,
		ClientSecret: settings.ClientSecret,
		RedirectURI:  settings.RedirectURI,
	})
	log.Printf("Google OAuth configured from database with redirect URI: %s", settings.RedirectURI)
}

// initMicrosoftOAuthFromDB loads Microsoft OAuth settings from database
func (s *Server) initMicrosoftOAuthFromDB() {
	settings, err := s.database.GetMicrosoftOAuthSettings()
	if err != nil {
		log.Printf("Failed to load Microsoft OAuth settings from DB: %v", err)
		return
	}

	if settings.ClientID == "" || settings.ClientSecret == "" {
		return
	}

	s.microsoftOAuth = oauth.NewMicrosoftOAuth(&config.MicrosoftOAuthConfig{
		ClientID:     settings.ClientID,
		ClientSecret: settings.ClientSecret,
		RedirectURI:  settings.RedirectURI,
	})
	log.Printf("Microsoft OAuth configured from database with redirect URI: %s", settings.RedirectURI)
}
