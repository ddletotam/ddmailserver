package web

import (
	"log"
	"net/http"
	"time"

	"github.com/yourusername/mailserver/internal/config"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
)

// HandleGoogleOAuthStart initiates the Google OAuth2 flow
func (s *Server) HandleGoogleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.googleOAuth == nil {
		http.Error(w, "Google OAuth not configured", http.StatusServiceUnavailable)
		return
	}

	// Generate state for CSRF protection
	state, err := oauth.GenerateState()
	if err != nil {
		log.Printf("Failed to generate OAuth state: %v", err)
		http.Error(w, "Failed to start OAuth flow", http.StatusInternalServerError)
		return
	}

	// Store state in session cookie (will be verified in callback)
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600, // 10 minutes
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // Lax needed for OAuth redirect
		Secure:   r.TLS != nil,
	})

	// Redirect to Google
	authURL := s.googleOAuth.GetAuthURL(state)
	log.Printf("Redirecting to Google OAuth: %s", authURL)
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

// HandleGoogleOAuthCallback handles the OAuth2 callback from Google
func (s *Server) HandleGoogleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.googleOAuth == nil {
		http.Error(w, "Google OAuth not configured", http.StatusServiceUnavailable)
		return
	}

	userID := getUserID(r)
	if userID == 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Verify state parameter
	state := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value != state {
		log.Printf("OAuth state mismatch: expected %s, got %s", stateCookie.Value, state)
		http.Error(w, "Invalid OAuth state - possible CSRF attack", http.StatusBadRequest)
		return
	}

	// Clear state cookie
	http.SetCookie(w, &http.Cookie{
		Name:   "oauth_state",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Check for error from Google
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		errDesc := r.URL.Query().Get("error_description")
		log.Printf("OAuth error from Google: %s - %s", errParam, errDesc)
		http.Redirect(w, r, "/accounts?error=oauth_denied", http.StatusSeeOther)
		return
	}

	// Get authorization code
	code := r.URL.Query().Get("code")
	if code == "" {
		log.Printf("No authorization code in callback")
		http.Error(w, "No authorization code received", http.StatusBadRequest)
		return
	}

	// Exchange code for tokens
	tokenResp, err := s.googleOAuth.ExchangeCode(code)
	if err != nil {
		log.Printf("Failed to exchange OAuth code: %v", err)
		http.Redirect(w, r, "/accounts?error=oauth_failed", http.StatusSeeOther)
		return
	}

	// Get user info (email)
	userInfo, err := s.googleOAuth.GetUserInfo(tokenResp.AccessToken)
	if err != nil {
		log.Printf("Failed to get user info: %v", err)
		http.Redirect(w, r, "/accounts?error=oauth_failed", http.StatusSeeOther)
		return
	}

	if !userInfo.VerifiedEmail {
		log.Printf("Email not verified for %s", userInfo.Email)
		http.Redirect(w, r, "/accounts?error=email_not_verified", http.StatusSeeOther)
		return
	}

	log.Printf("OAuth successful for email: %s", userInfo.Email)

	// Create account with OAuth credentials
	account := &models.Account{
		UserID:            userID,
		Name:              userInfo.Name,
		Email:             userInfo.Email,
		IMAPHost:          oauth.GmailIMAPHost(),
		IMAPPort:          oauth.GmailIMAPPort(),
		IMAPUsername:      userInfo.Email,
		IMAPTLS:           true,
		SMTPHost:          oauth.GmailSMTPHost(),
		SMTPPort:          oauth.GmailSMTPPort(),
		SMTPUsername:      userInfo.Email,
		SMTPTLS:           true,
		Enabled:           true,
		AuthType:          "oauth2_google",
		OAuthAccessToken:  tokenResp.AccessToken,
		OAuthRefreshToken: tokenResp.RefreshToken,
		OAuthTokenExpiry:  oauth.TokenExpiry(tokenResp.ExpiresIn),
	}

	// If name is empty, use email
	if account.Name == "" {
		account.Name = userInfo.Email
	}

	// Create account in database
	if err := s.database.CreateAccount(account); err != nil {
		log.Printf("Failed to create Gmail account: %v", err)
		http.Redirect(w, r, "/accounts?error=account_create_failed", http.StatusSeeOther)
		return
	}

	log.Printf("Gmail account created: %s for user %d", account.Email, userID)

	// Redirect to accounts page with success message
	http.Redirect(w, r, "/accounts?success=gmail_added", http.StatusSeeOther)
}

// HandleRefreshOAuthToken refreshes the OAuth token for an account
// This is called internally when token is expired
func (s *Server) RefreshOAuthToken(account *models.Account) error {
	if s.googleOAuth == nil {
		return nil
	}

	if !account.NeedsTokenRefresh() {
		return nil
	}

	tokenResp, err := s.googleOAuth.RefreshToken(account.OAuthRefreshToken)
	if err != nil {
		return err
	}

	// Update tokens in database
	expiry := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if err := s.database.UpdateAccountOAuthTokens(account.ID, tokenResp.AccessToken, tokenResp.RefreshToken, expiry); err != nil {
		return err
	}

	// Update account in memory
	account.OAuthAccessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		account.OAuthRefreshToken = tokenResp.RefreshToken
	}
	account.OAuthTokenExpiry = expiry

	log.Printf("OAuth token refreshed for account %d (%s)", account.ID, account.Email)
	return nil
}

// HandleSaveGoogleOAuthSettings saves Google OAuth configuration (admin only)
func (s *Server) HandleSaveGoogleOAuthSettings(w http.ResponseWriter, r *http.Request) {
	user := s.GetUserFromContext(r.Context())
	if user == nil || !user.IsAdmin() {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<div class="error-message">Access denied. Admin only.</div>`))
		return
	}

	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">Invalid form data</div>`))
		return
	}

	clientID := r.FormValue("client_id")
	clientSecret := r.FormValue("client_secret")
	redirectURI := r.FormValue("redirect_uri")

	// Get existing settings to preserve secret if not provided
	existingSettings, _ := s.database.GetGoogleOAuthSettings()

	settings := &db.GoogleOAuthSettings{
		ClientID:    clientID,
		RedirectURI: redirectURI,
	}

	// Only update secret if provided
	if clientSecret != "" {
		settings.ClientSecret = clientSecret
	} else if existingSettings != nil {
		settings.ClientSecret = existingSettings.ClientSecret
	}

	// Validate
	if settings.ClientID == "" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">Client ID is required</div>`))
		return
	}

	if settings.ClientSecret == "" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">Client Secret is required</div>`))
		return
	}

	if settings.RedirectURI == "" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="error-message">Redirect URI is required</div>`))
		return
	}

	// Save settings
	if err := s.database.SetGoogleOAuthSettings(settings); err != nil {
		log.Printf("Failed to save OAuth settings: %v", err)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="error-message">Failed to save settings</div>`))
		return
	}

	// Reinitialize Google OAuth with new settings
	s.googleOAuth = oauth.NewGoogleOAuth(&config.GoogleOAuthConfig{
		ClientID:     settings.ClientID,
		ClientSecret: settings.ClientSecret,
		RedirectURI:  settings.RedirectURI,
	})

	log.Printf("Google OAuth settings updated by admin")

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<div class="success-message">OAuth settings saved successfully! Gmail integration is now active.</div>`))
}
