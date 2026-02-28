package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/yourusername/mailserver/internal/models"
)

// Response helpers
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

// RegisterRequest represents registration request
type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
}

// LoginRequest represents login request
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// HandleRegister handles user registration
func (s *Server) HandleRegister(w http.ResponseWriter, r *http.Request) {
	// Parse form data (for htmx form submissions) or JSON (for API)
	var req RegisterRequest

	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data" {
		// Parse form data
		if err := r.ParseForm(); err != nil {
			respondError(w, http.StatusBadRequest, "invalid form data")
			return
		}
		req.Username = r.FormValue("username")
		req.Email = r.FormValue("email")
		req.Password = r.FormValue("password")
		req.PasswordConfirm = r.FormValue("password_confirm")
	} else {
		// Parse JSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Validate input (email is now optional)
	if req.Username == "" || req.Password == "" {
		respondError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	// Generate recovery key
	recoveryKey, err := GenerateRecoveryKey()
	if err != nil {
		log.Printf("Failed to generate recovery key: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to generate recovery key")
		return
	}

	// Hash recovery key
	recoveryKeyHash, err := HashRecoveryKey(recoveryKey)
	if err != nil {
		log.Printf("Failed to hash recovery key: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to hash recovery key")
		return
	}

	// TODO: Hash password properly
	// For now, storing plain text (THIS IS NOT SECURE - FIX THIS!)
	user, err := s.database.CreateUser(req.Username, req.Password, req.Email, recoveryKeyHash)
	if err != nil {
		log.Printf("Failed to create user: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Generate token
	token, err := GenerateToken(user.ID, user.Username, s.jwtSecret)
	if err != nil {
		log.Printf("Failed to generate token: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	log.Printf("User registered: %s", user.Username)

	// Set session cookie for web UI
	s.SetSessionCookie(w, token)

	// IMPORTANT: Recovery key is returned ONLY ONCE here!
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"user":         user,
		"token":        token,
		"recovery_key": recoveryKey, // User MUST save this!
	})
}

// HandleLogin handles user login
func (s *Server) HandleLogin(w http.ResponseWriter, r *http.Request) {
	// Parse form data (for htmx form submissions) or JSON (for API)
	var req LoginRequest

	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data" {
		// Parse form data
		if err := r.ParseForm(); err != nil {
			respondError(w, http.StatusBadRequest, "invalid form data")
			return
		}
		req.Username = r.FormValue("username")
		req.Password = r.FormValue("password")
	} else {
		// Parse JSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Get user
	user, err := s.database.GetUserByUsername(req.Username)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// TODO: Verify hashed password
	// For now, comparing plain text (THIS IS NOT SECURE - FIX THIS!)
	if user.PasswordHash != req.Password {
		respondError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// Generate token
	token, err := GenerateToken(user.ID, user.Username, s.jwtSecret)
	if err != nil {
		log.Printf("Failed to generate token: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	log.Printf("User logged in: %s", user.Username)

	// Set session cookie for web UI
	s.SetSessionCookie(w, token)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"user":  user,
		"token": token,
	})
}

// HandleGetAccounts returns all accounts for the authenticated user
func (s *Server) HandleGetAccounts(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	accounts, err := s.database.GetAccountsByUserID(userID)
	if err != nil {
		log.Printf("Failed to get accounts: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to get accounts")
		return
	}

	respondJSON(w, http.StatusOK, accounts)
}

// HandleCreateAccount creates a new email account
func (s *Server) HandleCreateAccount(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var account models.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Set user ID
	account.UserID = userID
	account.Enabled = true

	// Validate required fields
	if account.Name == "" || account.Email == "" {
		respondError(w, http.StatusBadRequest, "name and email are required")
		return
	}

	if account.IMAPHost == "" || account.IMAPPort == 0 {
		respondError(w, http.StatusBadRequest, "IMAP host and port are required")
		return
	}

	if account.SMTPHost == "" || account.SMTPPort == 0 {
		respondError(w, http.StatusBadRequest, "SMTP host and port are required")
		return
	}

	// Create account
	if err := s.database.CreateAccount(&account); err != nil {
		log.Printf("Failed to create account: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to create account")
		return
	}

	log.Printf("Account created: %s for user %d", account.Email, userID)

	respondJSON(w, http.StatusCreated, account)
}

// HandleGetAccount returns a specific account
func (s *Server) HandleGetAccount(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	vars := mux.Vars(r)
	accountID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid account ID")
		return
	}

	account, err := s.database.GetAccountByID(accountID)
	if err != nil {
		respondError(w, http.StatusNotFound, "account not found")
		return
	}

	// Check ownership
	if account.UserID != userID {
		respondError(w, http.StatusForbidden, "access denied")
		return
	}

	respondJSON(w, http.StatusOK, account)
}

// HandleUpdateAccount updates an account
func (s *Server) HandleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	vars := mux.Vars(r)
	accountID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid account ID")
		return
	}

	// Get existing account
	account, err := s.database.GetAccountByID(accountID)
	if err != nil {
		respondError(w, http.StatusNotFound, "account not found")
		return
	}

	// Check ownership
	if account.UserID != userID {
		respondError(w, http.StatusForbidden, "access denied")
		return
	}

	// Decode update data
	var update models.Account
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Update fields
	account.Name = update.Name
	account.Email = update.Email
	account.IMAPHost = update.IMAPHost
	account.IMAPPort = update.IMAPPort
	account.IMAPUsername = update.IMAPUsername
	if update.IMAPPassword != "" {
		account.IMAPPassword = update.IMAPPassword
	}
	account.IMAPTLS = update.IMAPTLS
	account.SMTPHost = update.SMTPHost
	account.SMTPPort = update.SMTPPort
	account.SMTPUsername = update.SMTPUsername
	if update.SMTPPassword != "" {
		account.SMTPPassword = update.SMTPPassword
	}
	account.SMTPTLS = update.SMTPTLS
	account.Enabled = update.Enabled

	// Save
	if err := s.database.UpdateAccount(account); err != nil {
		log.Printf("Failed to update account: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to update account")
		return
	}

	log.Printf("Account updated: %s", account.Email)

	respondJSON(w, http.StatusOK, account)
}

// HandleDeleteAccount deletes an account
func (s *Server) HandleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	vars := mux.Vars(r)
	accountID, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid account ID")
		return
	}

	// Get account
	account, err := s.database.GetAccountByID(accountID)
	if err != nil {
		respondError(w, http.StatusNotFound, "account not found")
		return
	}

	// Check ownership
	if account.UserID != userID {
		respondError(w, http.StatusForbidden, "access denied")
		return
	}

	// Delete
	if err := s.database.DeleteAccount(accountID); err != nil {
		log.Printf("Failed to delete account: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to delete account")
		return
	}

	log.Printf("Account deleted: %s", account.Email)

	respondJSON(w, http.StatusOK, map[string]string{"message": "account deleted"})
}

// HandleHealthCheck returns server health status
func (s *Server) HandleHealthCheck(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}
