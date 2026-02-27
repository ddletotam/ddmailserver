package web

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
)

// Server represents the web server
type Server struct {
	database  *db.DB
	jwtSecret string
	router    *mux.Router
	addr      string
	server    *http.Server
}

// New creates a new web server
func New(database *db.DB, jwtSecret string, host string, port int) *Server {
	addr := fmt.Sprintf("%s:%d", host, port)

	s := &Server{
		database:  database,
		jwtSecret: jwtSecret,
		router:    mux.NewRouter(),
		addr:      addr,
	}

	s.setupRoutes()

	s.server = &http.Server{
		Addr:    addr,
		Handler: s.router,
	}

	log.Printf("Web server created, will listen on %s", addr)

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

	// Public API routes
	s.router.HandleFunc("/health", s.HandleHealthCheck).Methods("GET")
	s.router.HandleFunc("/api/register", s.HandleRegister).Methods("POST")
	s.router.HandleFunc("/api/login", s.HandleLogin).Methods("POST")

	// Protected API routes
	api := s.router.PathPrefix("/api").Subrouter()
	api.Use(s.AuthMiddleware)

	// Accounts API
	api.HandleFunc("/accounts", s.HandleGetAccounts).Methods("GET")
	api.HandleFunc("/accounts", s.HandleCreateAccount).Methods("POST")
	api.HandleFunc("/accounts/{id}", s.HandleGetAccount).Methods("GET")
	api.HandleFunc("/accounts/{id}", s.HandleUpdateAccount).Methods("PUT")
	api.HandleFunc("/accounts/{id}", s.HandleDeleteAccount).Methods("DELETE")

	// Protected web routes
	web := s.router.PathPrefix("/").Subrouter()
	web.Use(s.WebAuthMiddleware)

	// Dashboard
	web.HandleFunc("/dashboard", s.HandleDashboard).Methods("GET")

	// Accounts web pages
	web.HandleFunc("/accounts", s.HandleAccountsPage).Methods("GET")
	web.HandleFunc("/accounts/list", s.HandleAccountsList).Methods("GET")
	web.HandleFunc("/accounts/new", s.HandleAccountFormPage).Methods("GET")
	web.HandleFunc("/accounts/{id}/edit", s.HandleAccountFormPage).Methods("GET")

	// Inbox
	web.HandleFunc("/inbox", s.HandleInboxPage).Methods("GET")
	web.HandleFunc("/messages/list", s.HandleMessagesList).Methods("GET")
	web.HandleFunc("/messages/{id}", s.HandleMessagePage).Methods("GET")
	web.HandleFunc("/message/{id}", s.HandleMessagePage).Methods("GET")

	// Compose
	web.HandleFunc("/compose", s.HandleComposePage).Methods("GET")

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
