package worker

import (
	"context"
	"log"
	"time"

	"github.com/yourusername/mailserver/internal/db"
	imapclient "github.com/yourusername/mailserver/internal/imap/client"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
	smtpclient "github.com/yourusername/mailserver/internal/smtp/client"
)

// Scheduler schedules periodic tasks for mail synchronization
type Scheduler struct {
	pool           *Pool
	database       *db.DB
	interval       time.Duration
	ctx            context.Context
	cancel         context.CancelFunc
	googleOAuth    *oauth.GoogleOAuth
	microsoftOAuth *oauth.MicrosoftOAuth
}

// NewScheduler creates a new task scheduler
func NewScheduler(pool *Pool, database *db.DB, intervalSeconds int) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())

	return &Scheduler{
		pool:     pool,
		database: database,
		interval: time.Duration(intervalSeconds) * time.Second,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// SetOAuthClients sets the OAuth clients for token refresh
func (s *Scheduler) SetOAuthClients(google *oauth.GoogleOAuth, microsoft *oauth.MicrosoftOAuth) {
	s.googleOAuth = google
	s.microsoftOAuth = microsoft
}

// Start starts the scheduler
func (s *Scheduler) Start() {
	log.Printf("Scheduler started with interval %v", s.interval)

	// Run initial sync immediately
	s.scheduleAllAccounts()

	// Schedule periodic syncs
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			log.Printf("Scheduler shutting down")
			return

		case <-ticker.C:
			s.scheduleAllAccounts()
		}
	}
}

// Stop stops the scheduler
func (s *Scheduler) Stop() {
	log.Printf("Stopping scheduler...")
	s.cancel()
}

// scheduleAllAccounts creates sync tasks for all enabled accounts and outbox messages
func (s *Scheduler) scheduleAllAccounts() {
	log.Printf("Scheduling sync and send tasks")

	// Schedule IMAP sync tasks
	s.scheduleIMAPSync()

	// Schedule SMTP send tasks
	s.scheduleSMTPSend()

	// Schedule CalDAV sync tasks
	s.scheduleCalendarSync()

	// Schedule CardDAV contact sync tasks
	s.scheduleContactSync()

	// Schedule flag sync tasks (reverse proxy for external accounts)
	s.scheduleFlagSync()

	imapQueueLen, smtpQueueLen := s.pool.QueueLength()
	log.Printf("Current queue lengths - IMAP: %d, SMTP: %d", imapQueueLen, smtpQueueLen)
}

// scheduleIMAPSync schedules IMAP synchronization tasks
func (s *Scheduler) scheduleIMAPSync() {
	accounts, err := s.getAllEnabledAccounts()
	if err != nil {
		log.Printf("Failed to get enabled accounts: %v", err)
		return
	}

	log.Printf("Found %d enabled accounts to sync", len(accounts))

	for _, account := range accounts {
		task := imapclient.NewSyncTask(account, s.database)

		if err := s.pool.Submit(task); err != nil {
			log.Printf("Failed to submit sync task for %s: %v", account.Email, err)
		} else {
			log.Printf("Submitted sync task for %s", account.Email)
		}
	}
}

// scheduleSMTPSend schedules SMTP send tasks for pending outbox messages
func (s *Scheduler) scheduleSMTPSend() {
	// Get pending outbox messages
	messages, err := s.database.GetPendingOutboxMessages(100) // Limit to 100 at a time
	if err != nil {
		log.Printf("Failed to get pending outbox messages: %v", err)
		return
	}

	if len(messages) == 0 {
		return
	}

	log.Printf("Found %d pending messages to send", len(messages))

	for _, msg := range messages {
		// Get account for this message
		account, err := s.database.GetAccountByID(msg.AccountID)
		if err != nil {
			log.Printf("Failed to get account %d for message %d: %v", msg.AccountID, msg.ID, err)
			continue
		}

		task := smtpclient.NewSendTask(msg, account, s.database)

		if err := s.pool.Submit(task); err != nil {
			log.Printf("Failed to submit send task for message %d: %v", msg.ID, err)
		} else {
			log.Printf("Submitted send task for message %d", msg.ID)
		}
	}
}

// getAllEnabledAccounts retrieves all enabled accounts
func (s *Scheduler) getAllEnabledAccounts() ([]*models.Account, error) {
	return s.database.GetAllEnabledAccounts()
}

// TriggerCalendarSyncForUser triggers immediate calendar sync for a specific user
// This is called when a calendar invite (.ics) is received via email
func (s *Scheduler) TriggerCalendarSyncForUser(userID int64) {
	sources, err := s.database.GetCalendarSourcesByUserID(userID)
	if err != nil {
		log.Printf("Failed to get calendar sources for user %d: %v", userID, err)
		return
	}

	if len(sources) == 0 {
		return
	}

	log.Printf("Triggering immediate calendar sync for user %d (%d sources)", userID, len(sources))

	for _, source := range sources {
		if !source.SyncEnabled {
			continue
		}

		var task Task
		switch source.SourceType {
		case "caldav":
			task = NewCalendarSyncTask(source, s.database, s.googleOAuth, s.microsoftOAuth)
		case "ics_url":
			task = NewICSSyncTask(source, s.database)
		default:
			continue
		}

		if err := s.pool.Submit(task); err != nil {
			log.Printf("Failed to submit immediate calendar sync for %s: %v", source.Name, err)
		} else {
			log.Printf("Submitted immediate calendar sync for %s", source.Name)
		}
	}
}

// scheduleCalendarSync schedules CalDAV and ICS URL synchronization tasks
func (s *Scheduler) scheduleCalendarSync() {
	sources, err := s.database.GetAllEnabledCalendarSources()
	if err != nil {
		log.Printf("Failed to get enabled calendar sources: %v", err)
		return
	}

	if len(sources) == 0 {
		return
	}

	log.Printf("Found %d calendar sources to sync", len(sources))

	for _, source := range sources {
		// Check if source needs sync based on interval
		if !source.NeedsSync() {
			continue
		}

		var task Task
		switch source.SourceType {
		case "caldav":
			task = NewCalendarSyncTask(source, s.database, s.googleOAuth, s.microsoftOAuth)
		case "ics_url":
			task = NewICSSyncTask(source, s.database)
		default:
			log.Printf("Unknown source type: %s", source.SourceType)
			continue
		}

		if err := s.pool.Submit(task); err != nil {
			log.Printf("Failed to submit calendar sync task for %s: %v", source.Name, err)
		} else {
			log.Printf("Submitted calendar sync task for %s (%s)", source.Name, source.SourceType)
		}
	}
}

// scheduleContactSync schedules CardDAV contact synchronization tasks
func (s *Scheduler) scheduleContactSync() {
	sources, err := s.database.GetAllEnabledContactSources()
	if err != nil {
		log.Printf("Failed to get enabled contact sources: %v", err)
		return
	}

	if len(sources) == 0 {
		return
	}

	log.Printf("Found %d contact sources to sync", len(sources))

	for _, source := range sources {
		// Check if source needs sync based on interval
		if !source.NeedsSync() {
			continue
		}

		task := NewContactSyncTask(source, s.database, s.googleOAuth, s.microsoftOAuth)

		if err := s.pool.Submit(task); err != nil {
			log.Printf("Failed to submit contact sync task for %s: %v", source.Name, err)
		} else {
			log.Printf("Submitted contact sync task for %s (%s)", source.Name, source.SourceType)
		}
	}
}

// scheduleFlagSync schedules flag synchronization tasks for external IMAP accounts
// This implements "reverse proxy" mode - pushing local flag changes back to source servers
func (s *Scheduler) scheduleFlagSync() {
	// Get accounts that have pending flag changes
	accountIDs, err := s.database.GetAccountsWithPendingFlagSync()
	if err != nil {
		log.Printf("Failed to get accounts with pending flag sync: %v", err)
		return
	}

	if len(accountIDs) == 0 {
		return
	}

	log.Printf("Found %d accounts with pending flag sync", len(accountIDs))

	for _, accountID := range accountIDs {
		account, err := s.database.GetAccountByID(accountID)
		if err != nil {
			log.Printf("Failed to get account %d for flag sync: %v", accountID, err)
			continue
		}

		// Skip disabled accounts
		if !account.Enabled {
			continue
		}

		task := NewFlagSyncTask(account, s.database)

		if err := s.pool.Submit(task); err != nil {
			log.Printf("Failed to submit flag sync task for %s: %v", account.Email, err)
		} else {
			log.Printf("Submitted flag sync task for %s", account.Email)
		}
	}
}
