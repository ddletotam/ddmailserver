package oauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/config"
)

const (
	GoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL = "https://oauth2.googleapis.com/token"
	GoogleUserInfo = "https://www.googleapis.com/oauth2/v2/userinfo"

	// Gmail IMAP/SMTP scope
	GmailScope = "https://mail.google.com/"
	// Email scope to get user's email address
	EmailScope = "email"
)

// TokenResponse represents the response from Google's token endpoint
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token,omitempty"`
}

// UserInfo represents the response from Google's userinfo endpoint
type UserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// GoogleOAuth handles Google OAuth2 operations
type GoogleOAuth struct {
	config *config.GoogleOAuthConfig
}

// NewGoogleOAuth creates a new GoogleOAuth instance
func NewGoogleOAuth(cfg *config.GoogleOAuthConfig) *GoogleOAuth {
	return &GoogleOAuth{config: cfg}
}

// GenerateState creates a random state parameter for CSRF protection
func GenerateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// GetAuthURL returns the URL to redirect users for Google OAuth consent
func (g *GoogleOAuth) GetAuthURL(state string) string {
	params := url.Values{
		"client_id":     {g.config.ClientID},
		"redirect_uri":  {g.config.RedirectURI},
		"response_type": {"code"},
		"scope":         {GmailScope + " " + EmailScope},
		"access_type":   {"offline"}, // Request refresh token
		"prompt":        {"consent"}, // Force consent to get refresh token
		"state":         {state},
	}
	return GoogleAuthURL + "?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for tokens
func (g *GoogleOAuth) ExchangeCode(code string) (*TokenResponse, error) {
	data := url.Values{
		"client_id":     {g.config.ClientID},
		"client_secret": {g.config.ClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {g.config.RedirectURI},
	}

	resp, err := http.PostForm(GoogleTokenURL, data)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("token exchange failed: %v", errResp)
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// RefreshToken refreshes an access token using a refresh token
func (g *GoogleOAuth) RefreshToken(refreshToken string) (*TokenResponse, error) {
	data := url.Values{
		"client_id":     {g.config.ClientID},
		"client_secret": {g.config.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}

	resp, err := http.PostForm(GoogleTokenURL, data)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("token refresh failed: %v", errResp)
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// GetUserInfo retrieves the user's email address using the access token
func (g *GoogleOAuth) GetUserInfo(accessToken string) (*UserInfo, error) {
	req, err := http.NewRequest("GET", GoogleUserInfo, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo request failed with status: %d", resp.StatusCode)
	}

	var userInfo UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	return &userInfo, nil
}

// TokenExpiry calculates the token expiry time from ExpiresIn seconds
func TokenExpiry(expiresIn int) time.Time {
	return time.Now().Add(time.Duration(expiresIn) * time.Second)
}

// BuildXOAuth2String builds the XOAUTH2 authentication string for IMAP/SMTP
// Format: "user=" + user + "\x01auth=Bearer " + accessToken + "\x01\x01"
func BuildXOAuth2String(email, accessToken string) string {
	return fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", email, accessToken)
}

// GmailIMAPHost returns the Gmail IMAP server hostname
func GmailIMAPHost() string {
	return "imap.gmail.com"
}

// GmailIMAPPort returns the Gmail IMAP server port
func GmailIMAPPort() int {
	return 993
}

// GmailSMTPHost returns the Gmail SMTP server hostname
func GmailSMTPHost() string {
	return "smtp.gmail.com"
}

// GmailSMTPPort returns the Gmail SMTP server port
func GmailSMTPPort() int {
	return 587
}

// IsGmailAccount checks if the email is a Gmail account
func IsGmailAccount(email string) bool {
	email = strings.ToLower(email)
	return strings.HasSuffix(email, "@gmail.com") || strings.HasSuffix(email, "@googlemail.com")
}
