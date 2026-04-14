package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"sync"
	"time"

	idle "github.com/emersion/go-imap-idle"
	imapClient "github.com/emersion/go-imap/client"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
)

// IdleManager maintains persistent IDLE connections to external IMAP servers.
// When a server reports new mail, it triggers an immediate sync instead of
// waiting for the next polling interval.
type IdleManager struct {
	database       *db.DB
	syncCallback   func(account *models.Account)
	googleOAuth    *oauth.GoogleOAuth
	microsoftOAuth *oauth.MicrosoftOAuth
	mu             sync.Mutex
	watchers       map[int64]context.CancelFunc // account ID -> cancel
	ctx            context.Context
	cancel         context.CancelFunc
}

// NewIdleManager creates a new IDLE manager.
func NewIdleManager(database *db.DB) *IdleManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &IdleManager{
		database: database,
		watchers: make(map[int64]context.CancelFunc),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// SetSyncCallback sets the function called when new mail is detected.
func (m *IdleManager) SetSyncCallback(fn func(account *models.Account)) {
	m.syncCallback = fn
}

// SetOAuthClients sets OAuth clients for token refresh.
func (m *IdleManager) SetOAuthClients(google *oauth.GoogleOAuth, microsoft *oauth.MicrosoftOAuth) {
	m.googleOAuth = google
	m.microsoftOAuth = microsoft
}

// Start runs the IDLE manager. It launches a watcher goroutine per account
// and periodically refreshes the account list.
func (m *IdleManager) Start() {
	log.Printf("IDLE Manager: started")
	m.refreshWatchers()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			log.Printf("IDLE Manager: shutting down")
			return
		case <-ticker.C:
			m.refreshWatchers()
		}
	}
}

// Stop shuts down all watchers.
func (m *IdleManager) Stop() {
	log.Printf("IDLE Manager: stopping")
	m.cancel()
}

// refreshWatchers starts watchers for new accounts and stops removed/disabled ones.
func (m *IdleManager) refreshWatchers() {
	if m.ctx.Err() != nil {
		return
	}
	accounts, err := m.database.GetAllEnabledAccounts()
	if err != nil {
		log.Printf("IDLE Manager: failed to get accounts: %v", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Only watch accounts with sync_mode=idle (or empty, which defaults to idle)
	idleIDs := make(map[int64]bool)
	for _, acc := range accounts {
		if acc.SyncMode == "poll" {
			continue
		}
		idleIDs[acc.ID] = true
		if _, exists := m.watchers[acc.ID]; !exists {
			wCtx, wCancel := context.WithCancel(m.ctx)
			m.watchers[acc.ID] = wCancel
			go m.watchAccount(wCtx, acc)
		}
	}

	// Stop watchers for removed/disabled/switched-to-poll accounts
	for id, cancel := range m.watchers {
		if !idleIDs[id] {
			cancel()
			delete(m.watchers, id)
			log.Printf("IDLE Manager: stopped watcher for account %d", id)
		}
	}
}

// watchAccount runs a reconnect loop for a single account.
func (m *IdleManager) watchAccount(ctx context.Context, account *models.Account) {
	m.accountLog(account.ID, "info", "IDLE watcher started for %s", account.Email)
	defer m.accountLog(account.ID, "info", "IDLE watcher stopped for %s", account.Email)

	backoff := 10 * time.Second
	const maxBackoff = 5 * time.Minute

	for {
		if ctx.Err() != nil {
			return
		}

		err := m.runIdleSession(ctx, account)
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			m.accountLog(account.ID, "error", "%v, reconnecting in %v", err, backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}

		// Reload account from DB in case credentials changed (e.g. OAuth refresh)
		fresh, err := m.database.GetAccountByID(account.ID)
		if err != nil {
			log.Printf("IDLE watcher [%s]: failed to reload account: %v", account.Email, err)
			continue
		}
		if !fresh.Enabled {
			log.Printf("IDLE watcher [%s]: account disabled, stopping", account.Email)
			return
		}
		account = fresh
	}
}

// refreshOAuthToken refreshes the OAuth token for an account if needed.
// If force is true, skips the expiry check and always refreshes.
func (m *IdleManager) refreshOAuthToken(account *models.Account, force bool) error {
	if !account.IsOAuth() {
		return nil
	}
	if !force && (account.OAuthTokenExpiry.IsZero() || time.Until(account.OAuthTokenExpiry) > 5*time.Minute) {
		return nil
	}
	if account.OAuthRefreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	var tokenResp *oauth.TokenResponse
	var err error

	switch account.AuthType {
	case "oauth2_google":
		if m.googleOAuth == nil {
			return fmt.Errorf("Google OAuth not configured")
		}
		tokenResp, err = m.googleOAuth.RefreshToken(account.OAuthRefreshToken)
	case "oauth2_microsoft":
		if m.microsoftOAuth == nil {
			return fmt.Errorf("Microsoft OAuth not configured")
		}
		tokenResp, err = m.microsoftOAuth.RefreshToken(account.OAuthRefreshToken)
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	expiry := oauth.TokenExpiry(tokenResp.ExpiresIn)
	newRefreshToken := tokenResp.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = account.OAuthRefreshToken
	}

	if err := m.database.UpdateAccountOAuthTokens(account.ID, tokenResp.AccessToken, newRefreshToken, expiry); err != nil {
		return fmt.Errorf("failed to save tokens: %w", err)
	}

	account.OAuthAccessToken = tokenResp.AccessToken
	account.OAuthRefreshToken = newRefreshToken
	account.OAuthTokenExpiry = expiry

	m.accountLog(account.ID, "info", "OAuth token refreshed, new expiry: %v", expiry)
	return nil
}

// runIdleSession connects, authenticates, selects INBOX and enters an
// IDLE loop. Returns on any error (caller will reconnect).
func (m *IdleManager) runIdleSession(ctx context.Context, account *models.Account) error {
	// Refresh OAuth token before connecting (only if near expiry)
	if account.IsOAuth() {
		if err := m.refreshOAuthToken(account, false); err != nil {
			return fmt.Errorf("oauth refresh: %w", err)
		}
	}

	addr := fmt.Sprintf("%s:%d", account.IMAPHost, account.IMAPPort)

	var conn *imapClient.Client
	var err error

	if account.IMAPTLS {
		conn, err = imapClient.DialTLS(addr, &tls.Config{ServerName: account.IMAPHost})
	} else {
		conn, err = imapClient.Dial(addr)
	}
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Logout()

	// Authenticate (with one forced-refresh retry on OAuth failure,
	// since Google can silently invalidate tokens even before their stated expiry)
	if account.IsOAuth() {
		if authErr := oauthAuthenticate(conn, account); authErr != nil {
			m.accountLog(account.ID, "info", "OAuth auth failed (%v), forcing token refresh and retrying", authErr)
			if rerr := m.refreshOAuthToken(account, true); rerr != nil {
				return fmt.Errorf("auth retry refresh: %w", rerr)
			}
			if authErr2 := oauthAuthenticate(conn, account); authErr2 != nil {
				return fmt.Errorf("auth after refresh: %w", authErr2)
			}
		}
	} else {
		if err := conn.Login(account.IMAPUsername, account.IMAPPassword); err != nil {
			return fmt.Errorf("login: %w", err)
		}
	}

	// Select INBOX
	if _, err := conn.Select("INBOX", false); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}

	// Set up updates channel to receive mailbox notifications
	updates := make(chan imapClient.Update, 16)
	conn.Updates = updates

	idleClient := idle.NewClient(conn)

	supported, err := idleClient.SupportIdle()
	if err != nil {
		return fmt.Errorf("check IDLE support: %w", err)
	}

	if supported {
		m.accountLog(account.ID, "info", "connected to %s, server supports IDLE", addr)
	} else {
		m.accountLog(account.ID, "info", "connected to %s, no IDLE support — using NOOP polling", addr)
	}

	// Main IDLE/poll loop
	for {
		if ctx.Err() != nil {
			return nil
		}

		stop := make(chan struct{})
		idleDone := make(chan error, 1)

		go func() {
			if supported {
				idleDone <- idleClient.Idle(stop)
			} else {
				idleDone <- idleClient.IdleWithFallback(stop, 2*time.Minute)
			}
		}()

		// Wait for mailbox update, error, or shutdown
		triggered := false
	wait:
		for {
			select {
			case <-ctx.Done():
				close(stop)
				<-idleDone
				return nil

			case upd := <-updates:
				switch upd.(type) {
				case *imapClient.MailboxUpdate:
					triggered = true
					break wait
				}
				// Ignore other update types, keep waiting

			case err := <-idleDone:
				// IDLE ended unexpectedly (connection lost)
				if err != nil {
					return fmt.Errorf("idle: %w", err)
				}
				// Idle returned nil without us closing stop — shouldn't happen, reconnect
				return fmt.Errorf("idle returned unexpectedly")
			}
		}

		// Stop IDLE so we can issue commands again
		close(stop)
		<-idleDone

		// Drain any queued updates
		drainUpdates(updates)

		if triggered && m.syncCallback != nil {
			m.accountLog(account.ID, "info", "new mail detected, triggering sync")
			m.syncCallback(account)
		}
	}
}

// drainUpdates empties the updates channel without blocking.
func drainUpdates(ch <-chan imapClient.Update) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// WatchedAccounts returns the number of accounts currently being watched.
func (m *IdleManager) WatchedAccounts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.watchers)
}

// accountLog writes a log entry for an account (both to stderr and DB).
func (m *IdleManager) accountLog(accountID int64, level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if level == "error" {
		log.Printf("IDLE [account %d]: ERROR: %s", accountID, msg)
	} else {
		log.Printf("IDLE [account %d]: %s", accountID, msg)
	}
	if err := m.database.AddAccountLog(accountID, level, msg); err != nil {
		log.Printf("IDLE [account %d]: failed to write log to DB: %v", accountID, err)
	}
}
