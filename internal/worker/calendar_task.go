package worker

import (
	"context"
	"fmt"
	"log"
	"time"

	caldavclient "github.com/yourusername/mailserver/internal/caldav/client"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
)

// CalendarSyncTask represents a calendar synchronization task
type CalendarSyncTask struct {
	source         *models.CalendarSource
	database       *db.DB
	googleOAuth    *oauth.GoogleOAuth
	microsoftOAuth *oauth.MicrosoftOAuth
}

// NewCalendarSyncTask creates a new calendar sync task
func NewCalendarSyncTask(source *models.CalendarSource, database *db.DB, googleOAuth *oauth.GoogleOAuth, microsoftOAuth *oauth.MicrosoftOAuth) *CalendarSyncTask {
	return &CalendarSyncTask{
		source:         source,
		database:       database,
		googleOAuth:    googleOAuth,
		microsoftOAuth: microsoftOAuth,
	}
}

// Execute runs the calendar sync task
func (t *CalendarSyncTask) Execute(ctx context.Context) error {
	log.Printf("Starting CalDAV sync for source %s (ID: %d)", t.source.Name, t.source.ID)

	err := t.doSync(ctx)
	if err != nil {
		// Save error to database so user can see it
		if dbErr := t.database.UpdateCalendarSourceLastError(t.source.ID, err.Error()); dbErr != nil {
			log.Printf("Failed to save sync error: %v", dbErr)
		}
		return err
	}

	log.Printf("CalDAV sync completed for %s", t.source.Name)
	return nil
}

// doSync performs the actual synchronization
func (t *CalendarSyncTask) doSync(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Refresh OAuth tokens if needed
	if err := t.refreshOAuthTokensIfNeeded(); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	// Create CalDAV client
	client := caldavclient.New(t.source, t.database)

	// Connect to CalDAV server
	if err := client.Connect(); err != nil {
		log.Printf("Failed to connect to CalDAV server for %s: %v", t.source.Name, err)
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Discover calendars
	calendars, err := client.DiscoverCalendars(ctx)
	if err != nil {
		log.Printf("Failed to discover calendars for %s: %v", t.source.Name, err)
		return fmt.Errorf("failed to discover calendars: %w", err)
	}

	log.Printf("Discovered %d calendars for %s", len(calendars), t.source.Name)

	var syncErrors []string

	// Sync each calendar
	for _, remoteCal := range calendars {
		// Check if calendar exists in DB
		existingCal, err := t.database.GetCalendarByRemoteID(t.source.ID, remoteCal.RemoteID)
		if err != nil {
			log.Printf("Failed to check existing calendar: %v", err)
			syncErrors = append(syncErrors, fmt.Sprintf("check calendar %s: %v", remoteCal.Name, err))
			continue
		}

		var cal *models.Calendar
		if existingCal == nil {
			// Create new calendar in DB
			remoteCal.SourceID = t.source.ID
			remoteCal.UserID = t.source.UserID
			if err := t.database.CreateCalendar(remoteCal); err != nil {
				log.Printf("Failed to create calendar %s: %v", remoteCal.Name, err)
				syncErrors = append(syncErrors, fmt.Sprintf("create calendar %s: %v", remoteCal.Name, err))
				continue
			}
			cal = remoteCal
			log.Printf("Created new calendar: %s", cal.Name)
		} else {
			cal = existingCal
		}

		// Sync calendar events
		if err := client.SyncCalendar(ctx, cal); err != nil {
			log.Printf("Failed to sync calendar %s: %v", cal.Name, err)
			syncErrors = append(syncErrors, fmt.Sprintf("sync %s: %v", cal.Name, err))
			continue
		}

		log.Printf("Synced calendar: %s", cal.Name)
	}

	// Update last sync time (clears last_error on success)
	if err := t.database.UpdateCalendarSourceLastSync(t.source.ID, time.Now(), ""); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}

	// If there were partial errors, report them
	if len(syncErrors) > 0 {
		return fmt.Errorf("partial sync errors: %v", syncErrors)
	}

	return nil
}

// Type returns the task type for queue routing
func (t *CalendarSyncTask) Type() TaskType {
	return TaskTypeIMAP // Use IMAP queue for calendar sync (similar resource usage)
}

// Priority returns the task priority
func (t *CalendarSyncTask) Priority() int {
	return 5 // Default priority
}

// String returns a human-readable description
func (t *CalendarSyncTask) String() string {
	return fmt.Sprintf("CalendarSync[%s]", t.source.Name)
}

// refreshOAuthTokensIfNeeded refreshes OAuth tokens if they are expired or about to expire
func (t *CalendarSyncTask) refreshOAuthTokensIfNeeded() error {
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

	log.Printf("Refreshing OAuth token for %s (expires: %v)", t.source.Name, t.source.OAuthTokenExpiry)

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

	if err := t.database.UpdateCalendarSourceOAuthTokens(t.source.ID, tokenResp.AccessToken, newRefreshToken, expiry); err != nil {
		return fmt.Errorf("failed to save new tokens: %w", err)
	}

	// Update source in memory for immediate use
	t.source.OAuthAccessToken = tokenResp.AccessToken
	t.source.OAuthRefreshToken = newRefreshToken
	t.source.OAuthTokenExpiry = expiry

	log.Printf("OAuth token refreshed for %s, new expiry: %v", t.source.Name, expiry)
	return nil
}
