package web

import (
	"fmt"
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

	// Public routes
	s.router.HandleFunc("/health", s.HandleHealthCheck).Methods("GET")
	s.router.HandleFunc("/api/register", s.HandleRegister).Methods("POST")
	s.router.HandleFunc("/api/login", s.HandleLogin).Methods("POST")

	// Protected routes
	api := s.router.PathPrefix("/api").Subrouter()
	api.Use(s.AuthMiddleware)

	// Accounts
	api.HandleFunc("/accounts", s.HandleGetAccounts).Methods("GET")
	api.HandleFunc("/accounts", s.HandleCreateAccount).Methods("POST")
	api.HandleFunc("/accounts/{id}", s.HandleGetAccount).Methods("GET")
	api.HandleFunc("/accounts/{id}", s.HandleUpdateAccount).Methods("PUT")
	api.HandleFunc("/accounts/{id}", s.HandleDeleteAccount).Methods("DELETE")

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
