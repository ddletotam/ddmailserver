package web

import (
	"encoding/json"
	"log"
	"net/http"
)

// ForgotPasswordRequest represents forgot password request
type ForgotPasswordRequest struct {
	Username        string `json:"username"`
	RecoveryKey     string `json:"recovery_key"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// HandleForgotPasswordPage shows the forgot password page
func (s *Server) HandleForgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title: "Forgot Password",
	}
	s.renderTemplate(w, "forgot_password.html", data)
}

// HandleForgotPassword handles password reset via recovery key
func (s *Server) HandleForgotPassword(w http.ResponseWriter, r *http.Request) {
	// Parse form data (for htmx form submissions) or JSON (for API)
	var req ForgotPasswordRequest

	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data" {
		// Parse form data
		if err := r.ParseForm(); err != nil {
			respondError(w, http.StatusBadRequest, "invalid form data")
			return
		}
		req.Username = r.FormValue("username")
		req.RecoveryKey = r.FormValue("recovery_key")
		req.NewPassword = r.FormValue("new_password")
		req.ConfirmPassword = r.FormValue("confirm_password")
	} else {
		// Parse JSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// Validate input
	if req.Username == "" || req.RecoveryKey == "" || req.NewPassword == "" {
		respondError(w, http.StatusBadRequest, "username, recovery key, and new password are required")
		return
	}

	// Check passwords match
	if req.NewPassword != req.ConfirmPassword {
		respondError(w, http.StatusBadRequest, "passwords do not match")
		return
	}

	// Get user by username
	user, err := s.database.GetUserByUsername(req.Username)
	if err != nil {
		log.Printf("User not found: %v", err)
		respondError(w, http.StatusUnauthorized, "invalid username or recovery key")
		return
	}

	// Verify recovery key
	if !VerifyRecoveryKey(req.RecoveryKey, user.RecoveryKeyHash) {
		log.Printf("Invalid recovery key for user: %s", req.Username)
		respondError(w, http.StatusUnauthorized, "invalid username or recovery key")
		return
	}

	// Hash new password
	passwordHash, err := HashPassword(req.NewPassword)
	if err != nil {
		log.Printf("Failed to hash password: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}

	// Update password
	err = s.database.UpdatePasswordByRecoveryKey(req.Username, passwordHash)
	if err != nil {
		log.Printf("Failed to update password: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}

	log.Printf("Password reset for user: %s", req.Username)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Password reset successfully",
	})
}

// HandleShowRecoveryKeyPage shows recovery key after registration
// Recovery key is passed via secure session cookie (one-time display)
func (s *Server) HandleShowRecoveryKeyPage(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get recovery key from secure cookie (set during registration)
	cookie, err := r.Cookie("recovery_key_temp")
	if err != nil || cookie.Value == "" {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	recoveryKey := cookie.Value

	// Clear the cookie immediately (one-time display)
	http.SetCookie(w, &http.Cookie{
		Name:     "recovery_key_temp",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	data := struct {
		PageData
		RecoveryKey string
	}{
		PageData: PageData{
			Title: "Your Recovery Key",
			User:  user,
		},
		RecoveryKey: recoveryKey,
	}

	s.renderTemplate(w, "recovery_key.html", data)
}
