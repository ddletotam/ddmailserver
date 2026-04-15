package oauth

import (
	"fmt"
	"log"
	"time"

	"github.com/yourusername/mailserver/internal/models"
)

// AccountTokenStore is the minimal DB surface needed to persist refreshed tokens.
// Using an interface keeps this package decoupled from the full DB type.
type AccountTokenStore interface {
	UpdateAccountOAuthTokens(accountID int64, accessToken, refreshToken string, expiry time.Time) error
}

// AccountTokenRefresher refreshes OAuth tokens stored on an Account, persists
// them, and updates the in-memory struct. Used by IMAP sync, IDLE manager and
// the scheduler.
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

// Refresh refreshes the OAuth token for an account if needed. If force is true,
// the expiry check is skipped and a refresh is always performed (used when an
// auth attempt failed even though the stored expiry is in the future — providers
// can revoke tokens early).
//
// Returns refreshed=true if a network refresh was actually performed and the
// token was updated, false if the existing token was still valid or the
// account isn't an OAuth account. Returns an error if no refresh token is
// stored or if the token endpoint rejects the refresh.
func (r *AccountTokenRefresher) Refresh(account *models.Account, force bool) (refreshed bool, err error) {
	if account == nil || !account.IsOAuth() {
		return false, nil
	}
	if !force && (account.OAuthTokenExpiry.IsZero() || time.Until(account.OAuthTokenExpiry) > 5*time.Minute) {
		return false, nil
	}
	if account.OAuthRefreshToken == "" {
		return false, fmt.Errorf("no refresh token available, please re-authenticate")
	}

	log.Printf("Refreshing OAuth token for %s (force=%v, expires=%v)", account.Email, force, account.OAuthTokenExpiry)

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

	if err := r.Store.UpdateAccountOAuthTokens(account.ID, tokenResp.AccessToken, newRefreshToken, expiry); err != nil {
		return false, fmt.Errorf("failed to save new tokens: %w", err)
	}

	// Update in-memory copy so subsequent operations use the fresh token.
	account.OAuthAccessToken = tokenResp.AccessToken
	account.OAuthRefreshToken = newRefreshToken
	account.OAuthTokenExpiry = expiry

	return true, nil
}
