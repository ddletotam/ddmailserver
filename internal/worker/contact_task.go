package worker

import (
	"context"
	"fmt"
	"log"
	"time"

	carddavclient "github.com/yourusername/mailserver/internal/carddav/client"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
)

// ContactSyncTask represents a contact synchronization task
type ContactSyncTask struct {
	source         *models.ContactSource
	database       *db.DB
	googleOAuth    *oauth.GoogleOAuth
	microsoftOAuth *oauth.MicrosoftOAuth
}

// NewContactSyncTask creates a new contact sync task
func NewContactSyncTask(source *models.ContactSource, database *db.DB, googleOAuth *oauth.GoogleOAuth, microsoftOAuth *oauth.MicrosoftOAuth) *ContactSyncTask {
	return &ContactSyncTask{
		source:         source,
		database:       database,
		googleOAuth:    googleOAuth,
		microsoftOAuth: microsoftOAuth,
	}
}

// Execute runs the contact sync task
func (t *ContactSyncTask) Execute(ctx context.Context) error {
	log.Printf("Starting CardDAV sync for contact source %s (ID: %d)", t.source.Name, t.source.ID)

	err := t.doSync(ctx)
	if err != nil {
		// Save error to database so user can see it
		if dbErr := t.database.UpdateContactSourceLastError(t.source.ID, err.Error()); dbErr != nil {
			log.Printf("Failed to save sync error: %v", dbErr)
		}
		return err
	}

	log.Printf("CardDAV contact sync completed for %s", t.source.Name)
	return nil
}

// doSync performs the actual synchronization
func (t *ContactSyncTask) doSync(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Refresh OAuth tokens if needed
	if err := t.refreshOAuthTokensIfNeeded(); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	// Create CardDAV client
	client := carddavclient.New(t.source, t.database)

	// Connect to CardDAV server
	if err := client.Connect(); err != nil {
		log.Printf("Failed to connect to CardDAV server for %s: %v", t.source.Name, err)
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Discover address books
	addressBooks, err := client.DiscoverAddressBooks(ctx)
	if err != nil {
		log.Printf("Failed to discover address books for %s: %v", t.source.Name, err)
		return fmt.Errorf("failed to discover address books: %w", err)
	}

	log.Printf("Discovered %d address books for %s", len(addressBooks), t.source.Name)

	var syncErrors []string

	// Sync each address book
	for _, remoteBook := range addressBooks {
		// Check if address book exists in DB
		existingBook, err := t.database.GetAddressBookByRemoteID(t.source.ID, remoteBook.RemoteID)
		if err != nil {
			log.Printf("Failed to check existing address book: %v", err)
			syncErrors = append(syncErrors, fmt.Sprintf("check address book %s: %v", remoteBook.Name, err))
			continue
		}

		var book *models.AddressBook
		if existingBook == nil {
			// Create new address book in DB
			remoteBook.SourceID = t.source.ID
			remoteBook.UserID = t.source.UserID
			if err := t.database.CreateAddressBook(remoteBook); err != nil {
				log.Printf("Failed to create address book %s: %v", remoteBook.Name, err)
				syncErrors = append(syncErrors, fmt.Sprintf("create address book %s: %v", remoteBook.Name, err))
				continue
			}
			book = remoteBook
			log.Printf("Created new address book: %s", book.Name)
		} else {
			book = existingBook
		}

		// Sync contacts
		if err := client.SyncAddressBook(ctx, book); err != nil {
			log.Printf("Failed to sync address book %s: %v", book.Name, err)
			syncErrors = append(syncErrors, fmt.Sprintf("sync %s: %v", book.Name, err))
			continue
		}

		log.Printf("Synced address book: %s", book.Name)
	}

	// Update last sync time
	if err := t.database.UpdateContactSourceLastSync(t.source.ID, time.Now()); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	// If there were partial errors, report them
	if len(syncErrors) > 0 {
		return fmt.Errorf("partial sync errors: %v", syncErrors)
	}

	return nil
}

// Type returns the task type for queue routing
func (t *ContactSyncTask) Type() TaskType {
	return TaskTypeIMAP // Use IMAP queue for contact sync (similar resource usage)
}

// Priority returns the task priority
func (t *ContactSyncTask) Priority() int {
	return 5 // Default priority
}

// String returns a human-readable description
func (t *ContactSyncTask) String() string {
	return fmt.Sprintf("ContactSync[%s]", t.source.Name)
}

// refreshOAuthTokensIfNeeded refreshes OAuth tokens if they are expired or about to expire
func (t *ContactSyncTask) refreshOAuthTokensIfNeeded() error {
	// Only refresh for OAuth sources
	if t.source.AuthType != "oauth2_google" && t.source.AuthType != "oauth2_microsoft" {
		return nil
	}

	// Check if token is expired or expires within 5 minutes
	if t.source.OAuthTokenExpiry.IsZero() || time.Until(t.source.OAuthTokenExpiry) > 5*time.Minute {
		return nil // Token still valid
	}

	// Need refresh token
	if t.source.OAuthRefreshToken == "" {
		return fmt.Errorf("no refresh token available, please re-authenticate")
	}

	log.Printf("Refreshing OAuth token for contact source %s (expires: %v)", t.source.Name, t.source.OAuthTokenExpiry)

	var tokenResp *oauth.TokenResponse
	var err error

	if t.source.AuthType == "oauth2_google" {
		if t.googleOAuth == nil {
			return fmt.Errorf("Google OAuth not configured")
		}
		tokenResp, err = t.googleOAuth.RefreshToken(t.source.OAuthRefreshToken)
	} else if t.source.AuthType == "oauth2_microsoft" {
		if t.microsoftOAuth == nil {
			return fmt.Errorf("Microsoft OAuth not configured")
		}
		tokenResp, err = t.microsoftOAuth.RefreshToken(t.source.OAuthRefreshToken)
	}

	if err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	// Calculate new expiry
	expiry := oauth.TokenExpiry(tokenResp.ExpiresIn)

	// Update tokens in database
	newRefreshToken := tokenResp.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = t.source.OAuthRefreshToken // Keep existing if not returned
	}

	if err := t.database.UpdateContactSourceOAuthTokens(t.source.ID, tokenResp.AccessToken, newRefreshToken, expiry); err != nil {
		return fmt.Errorf("failed to save new tokens: %w", err)
	}

	// Update source in memory for immediate use
	t.source.OAuthAccessToken = tokenResp.AccessToken
	t.source.OAuthRefreshToken = newRefreshToken
	t.source.OAuthTokenExpiry = expiry

	log.Printf("OAuth token refreshed for contact source %s, new expiry: %v", t.source.Name, expiry)
	return nil
}
