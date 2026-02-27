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
	var req ForgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
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

	// TODO: Hash password properly
	// For now, storing plain text (THIS IS NOT SECURE - FIX THIS!)
	err = s.database.UpdatePasswordByRecoveryKey(req.Username, req.NewPassword)
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
// Recovery key is passed via session or query parameter (one-time display)
func (s *Server) HandleShowRecoveryKeyPage(w http.ResponseWriter, r *http.Request) {
	// Get recovery key from query parameter
	recoveryKey := r.URL.Query().Get("key")
	if recoveryKey == "" {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	user := s.GetUserFromContext(r.Context())
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

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
