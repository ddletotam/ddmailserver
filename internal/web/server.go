package web

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/db"
)

// Server represents the web server
type Server struct {
	database  *db.DB
	jwtSecret string
	router    *mux.Router
	addr      string
	server    *http.Server
	i18n      *I18n
}

// New creates a new web server
func New(database *db.DB, jwtSecret string, host string, port int, locale string) *Server {
	addr := fmt.Sprintf("%s:%d", host, port)

	// Initialize i18n
	if locale == "" {
		locale = "en" // default to English
	}
	i18n, err := NewI18n(locale)
	if err != nil {
		log.Printf("Failed to initialize i18n: %v, using English", err)
		i18n, _ = NewI18n("en")
	}

	s := &Server{
		database:  database,
		jwtSecret: jwtSecret,
		router:    mux.NewRouter(),
		addr:      addr,
		i18n:      i18n,
	}

	s.setupRoutes()

	s.server = &http.Server{
		Addr:    addr,
		Handler: s.router,
	}

	log.Printf("Web server created, will listen on %s (locale: %s)", addr, i18n.GetLocale())

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
	s.router.HandleFunc("/api/register", s.HandleRegister).Methods("POST")
	s.router.HandleFunc("/api/login", s.HandleLogin).Methods("POST")
	s.router.HandleFunc("/api/forgot-password", s.HandleForgotPassword).Methods("POST")

	// Settings API (uses session cookie auth, must be registered before general API routes)
	settingsAPI := s.router.PathPrefix("/api/settings").Subrouter()
	settingsAPI.Use(s.APIAuthMiddleware) // Returns JSON error instead of redirect
	settingsAPI.HandleFunc("/password", s.HandleChangePassword).Methods("POST")
	settingsAPI.HandleFunc("/language", s.HandleChangeLanguage).Methods("POST")
	settingsAPI.HandleFunc("/account", s.HandleDeleteUserAccount).Methods("DELETE")

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

// Stop stops the web server
func (s *Server) Stop() error {
	log.Printf("Stopping web server")
	return s.server.Close()
}
