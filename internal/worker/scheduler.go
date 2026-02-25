package worker

import (
	"context"
	"log"
	"time"

	"github.com/yourusername/mailserver/internal/db"
	imapclient "github.com/yourusername/mailserver/internal/imap/client"
	"github.com/yourusername/mailserver/internal/models"
	smtpclient "github.com/yourusername/mailserver/internal/smtp/client"
)

// Scheduler schedules periodic tasks for mail synchronization
type Scheduler struct {
	pool     *Pool
	database *db.DB
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
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
