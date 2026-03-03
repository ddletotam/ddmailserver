package oauth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yourusername/mailserver/internal/config"
)

const (
	// Microsoft OAuth2 endpoints (using "common" for multi-tenant)
	MicrosoftAuthURL     = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	MicrosoftTokenURL    = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	MicrosoftUserInfoURL = "https://graph.microsoft.com/v1.0/me"

	// Microsoft IMAP/SMTP scopes
	MicrosoftIMAPScope = "https://outlook.office.com/IMAP.AccessAsUser.All"
	MicrosoftSMTPScope = "https://outlook.office.com/SMTP.Send"
	// Standard OIDC scopes
	MicrosoftOpenIDScope        = "openid"
	MicrosoftEmailScope         = "email"
	MicrosoftOfflineAccessScope = "offline_access"
)

// MicrosoftUserInfo represents the response from Microsoft Graph /me endpoint
type MicrosoftUserInfo struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// GetEmail returns the user's email (mail or userPrincipalName)
func (u *MicrosoftUserInfo) GetEmail() string {
	if u.Mail != "" {
		return u.Mail
	}
	return u.UserPrincipalName
}

// MicrosoftOAuth handles Microsoft OAuth2 operations
type MicrosoftOAuth struct {
	config *config.MicrosoftOAuthConfig
}

// NewMicrosoftOAuth creates a new MicrosoftOAuth instance
func NewMicrosoftOAuth(cfg *config.MicrosoftOAuthConfig) *MicrosoftOAuth {
	return &MicrosoftOAuth{config: cfg}
}

// GetAuthURL returns the URL to redirect users for Microsoft OAuth consent
func (m *MicrosoftOAuth) GetAuthURL(state string) string {
	scopes := strings.Join([]string{
		MicrosoftIMAPScope,
		MicrosoftSMTPScope,
		MicrosoftOpenIDScope,
		MicrosoftEmailScope,
		MicrosoftOfflineAccessScope,
	}, " ")

	params := url.Values{
		"client_id":     {m.config.ClientID},
		"redirect_uri":  {m.config.RedirectURI},
		"response_type": {"code"},
		"scope":         {scopes},
		"response_mode": {"query"},
		"prompt":        {"consent"}, // Force consent to get refresh token
		"state":         {state},
	}
	return MicrosoftAuthURL + "?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for tokens
func (m *MicrosoftOAuth) ExchangeCode(code string) (*TokenResponse, error) {
	scopes := strings.Join([]string{
		MicrosoftIMAPScope,
		MicrosoftSMTPScope,
		MicrosoftOpenIDScope,
		MicrosoftEmailScope,
		MicrosoftOfflineAccessScope,
	}, " ")

	data := url.Values{
		"client_id":     {m.config.ClientID},
		"client_secret": {m.config.ClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {m.config.RedirectURI},
		"scope":         {scopes},
	}

	resp, err := http.PostForm(MicrosoftTokenURL, data)
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
func (m *MicrosoftOAuth) RefreshToken(refreshToken string) (*TokenResponse, error) {
	scopes := strings.Join([]string{
		MicrosoftIMAPScope,
		MicrosoftSMTPScope,
		MicrosoftOpenIDScope,
		MicrosoftEmailScope,
		MicrosoftOfflineAccessScope,
	}, " ")

	data := url.Values{
		"client_id":     {m.config.ClientID},
		"client_secret": {m.config.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
		"scope":         {scopes},
	}

	resp, err := http.PostForm(MicrosoftTokenURL, data)
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

// GetUserInfo retrieves the user's info using the access token
func (m *MicrosoftOAuth) GetUserInfo(accessToken string) (*MicrosoftUserInfo, error) {
	req, err := http.NewRequest("GET", MicrosoftUserInfoURL, nil)
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
		var errResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("userinfo request failed: %v", errResp)
	}

	var userInfo MicrosoftUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	return &userInfo, nil
}

// OutlookIMAPHost returns the Outlook IMAP server hostname
func OutlookIMAPHost() string {
	return "outlook.office365.com"
}

// OutlookIMAPPort returns the Outlook IMAP server port
func OutlookIMAPPort() int {
	return 993
}

// OutlookSMTPHost returns the Outlook SMTP server hostname
func OutlookSMTPHost() string {
	return "smtp.office365.com"
}

// OutlookSMTPPort returns the Outlook SMTP server port
func OutlookSMTPPort() int {
	return 587
}

// IsMicrosoftAccount checks if the email is a Microsoft account
func IsMicrosoftAccount(email string) bool {
	email = strings.ToLower(email)
	microsoftDomains := []string{
		"@outlook.com",
		"@outlook.ru",
		"@hotmail.com",
		"@hotmail.ru",
		"@live.com",
		"@live.ru",
		"@msn.com",
	}
	for _, domain := range microsoftDomains {
		if strings.HasSuffix(email, domain) {
			return true
		}
	}
	return false
}
