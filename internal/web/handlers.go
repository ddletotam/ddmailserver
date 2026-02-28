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
	Username        string `json:"username"`
	Password        string `json:"password"`
	Email           string `json:"email"`
	PasswordConfirm string `json:"password_confirm"`
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

	// Check passwords match
	if req.Password != req.PasswordConfirm {
		respondError(w, http.StatusBadRequest, "passwords do not match")
		return
	}

	// Generate recovery key
	recoveryKey, err := GenerateRecoveryKey()
	if err != nil {
		log.Printf("Failed to generate recovery key: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to generate recovery key")
		return
	}

	// Hash password
	passwordHash, err := HashPassword(req.Password)
	if err != nil {
		log.Printf("Failed to hash password: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	// Hash recovery key
	recoveryKeyHash, err := HashRecoveryKey(recoveryKey)
	if err != nil {
		log.Printf("Failed to hash recovery key: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to hash recovery key")
		return
	}

	// Create user
	user, err := s.database.CreateUser(req.Username, passwordHash, req.Email, recoveryKeyHash)
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

	// Verify password
	if !VerifyPassword(user.PasswordHash, req.Password) {
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

// ChangePasswordRequest represents password change request
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// HandleChangePassword handles password change
func (s *Server) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	log.Printf("HandleChangePassword: userID=%d", userID)

	// Parse form data or JSON
	var req ChangePasswordRequest
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data" {
		if err := r.ParseForm(); err != nil {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="error-message">Invalid form data</div>`))
			return
		}
		req.CurrentPassword = r.FormValue("current_password")
		req.NewPassword = r.FormValue("new_password")
		req.ConfirmPassword = r.FormValue("confirm_password")
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Validate input
	if req.CurrentPassword == "" || req.NewPassword == "" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">Current and new password are required</div>`))
		return
	}

	if req.NewPassword != req.ConfirmPassword {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">New passwords do not match</div>`))
		return
	}

	if len(req.NewPassword) < 8 {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">New password must be at least 8 characters</div>`))
		return
	}

	// Get user
	user, err := s.database.GetUserByID(userID)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="error-message">User not found</div>`))
		return
	}

	// Verify current password
	if !VerifyPassword(user.PasswordHash, req.CurrentPassword) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`<div class="error-message">Current password is incorrect</div>`))
		return
	}

	// Hash new password
	newPasswordHash, err := HashPassword(req.NewPassword)
	if err != nil {
		log.Printf("Failed to hash password: %v", err)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="error-message">Failed to update password</div>`))
		return
	}

	// Update password
	if err := s.database.UpdatePassword(userID, newPasswordHash); err != nil {
		log.Printf("Failed to update password: %v", err)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="error-message">Failed to update password</div>`))
		return
	}

	log.Printf("Password updated for user ID: %d", userID)

	// Return success message
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<div class="success-message">Password updated successfully!</div>`))
}

// ChangeLanguageRequest represents language change request
type ChangeLanguageRequest struct {
	Language string `json:"language"`
}

// HandleChangeLanguage handles language preference change
func (s *Server) HandleChangeLanguage(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	log.Printf("HandleChangeLanguage: userID=%d", userID)

	// Parse form data or JSON
	var req ChangeLanguageRequest
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data" {
		if err := r.ParseForm(); err != nil {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="error-message">Invalid form data</div>`))
			return
		}
		req.Language = r.FormValue("language")
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Validate language
	if req.Language != "en" && req.Language != "ru" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">Invalid language. Must be 'en' or 'ru'</div>`))
		return
	}

	// Update language
	if err := s.database.UpdateLanguage(userID, req.Language); err != nil {
		log.Printf("Failed to update language: %v", err)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="error-message">Failed to update language</div>`))
		return
	}

	log.Printf("Language updated for user ID: %d to: %s", userID, req.Language)

	// Return success message
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<div class="success-message">Language updated successfully! Please refresh the page.</div>`))
}

// HandleDeleteUserAccount handles user account deletion
func (s *Server) HandleDeleteUserAccount(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	log.Printf("Deleting user account: %d", userID)

	// Delete user
	if err := s.database.DeleteUser(userID); err != nil {
		log.Printf("Failed to delete user: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to delete account")
		return
	}

	log.Printf("User account deleted: %d", userID)

	// Clear session cookie
	http.SetCookie(w, &http.Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Redirect to login page
	w.Header().Set("HX-Redirect", "/login")
	respondJSON(w, http.StatusOK, map[string]string{"message": "account deleted"})
}
