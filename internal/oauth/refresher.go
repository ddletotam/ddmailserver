package oauth

import (
	"fmt"
	"log"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// AccountTokenStore is the minimal DB surface needed to persist refreshed tokens.
type AccountTokenStore interface {
	UpdateAccountOAuthTokens(accountID int64, accessToken, refreshToken string, expiry int64) error
}

// AccountTokenRefresher refreshes OAuth tokens stored on an Account, persists
// them, and updates the in-memory struct.
type AccountTokenRefresher struct {
	Google    *GoogleOAuth
	Microsoft *MicrosoftOAuth
	Store     AccountTokenStore
}

// NewAccountTokenRefresher constructs a refresher with the given OAuth clients
// and persistence store.
func NewAccountTokenRefresher(google *GoogleOAuth, microsoft *MicrosoftOAuth, store AccountTokenStore) *AccountTokenRefresher {
	return &AccountTokenRefresher{Google: google, Microsoft: microsoft, Store: store}
}

// Refresh refreshes the OAuth token for an account if needed.
func (r *AccountTokenRefresher) Refresh(account *models.Account, force bool) (refreshed bool, err error) {
	if account == nil || !account.IsOAuth() {
		return false, nil
	}
	// OAuthTokenExpiry is now int64 ms. Check if still valid (>5 min remaining).
	nowMs := time.Now().UnixMilli()
	if !force && (account.OAuthTokenExpiry == 0 || account.OAuthTokenExpiry-nowMs > 5*60*1000) {
		return false, nil
	}
	if account.OAuthRefreshToken == "" {
		return false, fmt.Errorf("no refresh token available, please re-authenticate")
	}

	log.Printf("Refreshing OAuth token for %s (force=%v, expires_ms=%d)", account.Email, force, account.OAuthTokenExpiry)

	var tokenResp *TokenResponse
	switch account.AuthType {
	case "oauth2_google":
		if r.Google == nil {
			return false, fmt.Errorf("Google OAuth not configured")
		}
		tokenResp, err = r.Google.RefreshToken(account.OAuthRefreshToken)
	case "oauth2_microsoft":
		if r.Microsoft == nil {
			return false, fmt.Errorf("Microsoft OAuth not configured")
		}
		tokenResp, err = r.Microsoft.RefreshToken(account.OAuthRefreshToken)
	default:
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("token refresh failed: %w", err)
	}

	expiry := TokenExpiry(tokenResp.ExpiresIn)
	newRefreshToken := tokenResp.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = account.OAuthRefreshToken
	}

	log.Printf("OAuth refresh response for %s: scope=%q expires_in=%d", account.Email, tokenResp.Scope, tokenResp.ExpiresIn)

	if err := r.Store.UpdateAccountOAuthTokens(account.ID, tokenResp.AccessToken, newRefreshToken, expiry); err != nil {
		return false, fmt.Errorf("failed to save new tokens: %w", err)
	}

	account.OAuthAccessToken = tokenResp.AccessToken
	account.OAuthRefreshToken = newRefreshToken
	account.OAuthTokenExpiry = expiry

	return true, nil
}
