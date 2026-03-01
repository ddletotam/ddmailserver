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

// respondHTMXError returns an HTML error message for HTMX requests
func respondHTMXError(w http.ResponseWriter, r *http.Request, status int, message string) {
	// Check if request is from HTMX
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		// Return plain text - the target div already has error-message styling
		w.Write([]byte(message))
		return
	}
	respondJSON(w, status, map[string]string{"error": message})
}

// RegisterRequest represents registration request
type RegisterRequest struct {
	Username        string `json:"username"`
	Password        string `json:"password"`
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
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid form data")
			return
		}
		req.Username = r.FormValue("username")
		req.Password = r.FormValue("password")
		req.PasswordConfirm = r.FormValue("password_confirm")
	} else {
		// Parse JSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid request body")
			return
		}
	}

	// Validate input
	if req.Username == "" || req.Password == "" {
		respondHTMXError(w, r, http.StatusBadRequest, "Username and password are required")
		return
	}

	// Check passwords match
	if req.Password != req.PasswordConfirm {
		respondHTMXError(w, r, http.StatusBadRequest, "Passwords do not match")
		return
	}

	// Generate recovery key
	recoveryKey, err := GenerateRecoveryKey()
	if err != nil {
		log.Printf("Failed to generate recovery key: %v", err)
		respondHTMXError(w, r, http.StatusInternalServerError, "Failed to generate recovery key")
		return
	}

	// Hash password
	passwordHash, err := HashPassword(req.Password)
	if err != nil {
		log.Printf("Failed to hash password: %v", err)
		respondHTMXError(w, r, http.StatusInternalServerError, "Failed to hash password")
		return
	}

	// Hash recovery key
	recoveryKeyHash, err := HashRecoveryKey(recoveryKey)
	if err != nil {
		log.Printf("Failed to hash recovery key: %v", err)
		respondHTMXError(w, r, http.StatusInternalServerError, "Failed to hash recovery key")
		return
	}

	// Create user (email not used)
	user, err := s.database.CreateUser(req.Username, passwordHash, "", recoveryKeyHash)
	if err != nil {
		log.Printf("Failed to create user: %v", err)
		respondHTMXError(w, r, http.StatusBadRequest, "Username already exists")
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

	// Set recovery key in secure temporary cookie (one-time use)
	// This avoids exposing the key in URL parameters or logs
	http.SetCookie(w, &http.Cookie{
		Name:     "recovery_key_temp",
		Value:    recoveryKey,
		Path:     "/",
		MaxAge:   300, // 5 minutes - enough time to view and save
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	// For HTMX requests, return empty response - JS will handle redirect
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusCreated)
		return
	}

	// For API requests, return JSON with token
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"user":  user,
		"token": token,
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
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid form data")
			return
		}
		req.Username = r.FormValue("username")
		req.Password = r.FormValue("password")
	} else {
		// Parse JSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondHTMXError(w, r, http.StatusBadRequest, "Invalid request body")
			return
		}
	}

	// Get user
	user, err := s.database.GetUserByUsername(req.Username)
	if err != nil {
		respondHTMXError(w, r, http.StatusUnauthorized, "Invalid username or password")
		return
	}

	// Verify password
	if !VerifyPassword(user.PasswordHash, req.Password) {
		respondHTMXError(w, r, http.StatusUnauthorized, "Invalid username or password")
		return
	}

	// Generate token
	token, err := GenerateToken(user.ID, user.Username, s.jwtSecret)
	if err != nil {
		log.Printf("Failed to generate token: %v", err)
		respondHTMXError(w, r, http.StatusInternalServerError, "Login failed, please try again")
		return
	}

	log.Printf("User logged in: %s", user.Username)

	// Set session cookie for web UI
	s.SetSessionCookie(w, token)

	// For HTMX requests, return empty response - JS will handle redirect
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// For API requests, return JSON with token
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
	log.Printf("HandleCreateAccount: userID=%d", userID)

	var account models.Account

	// Parse form data or JSON
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data" {
		if err := r.ParseForm(); err != nil {
			respondError(w, http.StatusBadRequest, "invalid form data")
			return
		}

		account.Name = r.FormValue("name")
		account.Email = r.FormValue("email")
		account.IMAPHost = r.FormValue("imap_host")
		account.IMAPUsername = r.FormValue("imap_username")
		account.IMAPPassword = r.FormValue("imap_password")
		account.IMAPTLS = r.FormValue("imap_tls") == "true"
		account.SMTPHost = r.FormValue("smtp_host")
		account.SMTPUsername = r.FormValue("smtp_username")
		account.SMTPPassword = r.FormValue("smtp_password")
		account.SMTPTLS = r.FormValue("smtp_tls") == "true"

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
	} else {
		if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
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

	// Parse form data or JSON
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data" {
		if err := r.ParseForm(); err != nil {
			respondError(w, http.StatusBadRequest, "invalid form data")
			return
		}

		update.Name = r.FormValue("name")
		update.Email = r.FormValue("email")
		update.IMAPHost = r.FormValue("imap_host")
		update.IMAPUsername = r.FormValue("imap_username")
		update.IMAPPassword = r.FormValue("imap_password")
		update.IMAPTLS = r.FormValue("imap_tls") == "true"
		update.SMTPHost = r.FormValue("smtp_host")
		update.SMTPUsername = r.FormValue("smtp_username")
		update.SMTPPassword = r.FormValue("smtp_password")
		update.SMTPTLS = r.FormValue("smtp_tls") == "true"

		// Parse ports
		if imapPort := r.FormValue("imap_port"); imapPort != "" {
			if port, err := strconv.Atoi(imapPort); err == nil {
				update.IMAPPort = port
			}
		}
		if smtpPort := r.FormValue("smtp_port"); smtpPort != "" {
			if port, err := strconv.Atoi(smtpPort); err == nil {
				update.SMTPPort = port
			}
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
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
	log.Printf("HandleChangePassword called from %s", r.RemoteAddr)
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
	log.Printf("HandleChangeLanguage called from %s", r.RemoteAddr)
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

// HandleSendMessage handles sending an email
func (s *Server) HandleSendMessage(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	log.Printf("HandleSendMessage: userID=%d", userID)

	// Parse form data
	if err := r.ParseForm(); err != nil {
		respondError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	accountIDStr := r.FormValue("account_id")
	to := r.FormValue("to")
	subject := r.FormValue("subject")
	body := r.FormValue("body")
	format := r.FormValue("format") // "text" or "html"
	cc := r.FormValue("cc")
	bcc := r.FormValue("bcc")

	// Validate required fields
	if accountIDStr == "" || to == "" || body == "" {
		respondError(w, http.StatusBadRequest, "account_id, to, and body are required")
		return
	}

	// Parse account ID
	accountID, err := strconv.ParseInt(accountIDStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid account_id")
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

	// Create outbox message
	outboxMsg := &models.OutboxMessage{
		UserID:    userID,
		AccountID: accountID,
		From:      account.Email,
		To:        to,
		Cc:        cc,
		Bcc:       bcc,
		Subject:   subject,
		Status:    "pending",
		Retries:   0,
	}

	// Set body based on format
	if format == "html" {
		outboxMsg.BodyHTML = body
	} else {
		outboxMsg.Body = body
	}

	// Save to database
	if err := s.database.CreateOutboxMessage(outboxMsg); err != nil {
		log.Printf("Failed to create outbox message: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to queue message for sending")
		return
	}

	log.Printf("Outbox message created: %d (from %s to %s)", outboxMsg.ID, outboxMsg.From, outboxMsg.To)

	// Return success with HTMX response
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<div class="alert alert-success">✅ Email queued for sending! <a href="/inbox">View Inbox</a></div>`))
}
