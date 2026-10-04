package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/dkimsign"
	"github.com/ddletotam/ddmailserver/internal/authfail"
	imapclient "github.com/ddletotam/ddmailserver/internal/imap/client"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/oauth"
	"github.com/ddletotam/ddmailserver/internal/parser"
	smtpclient "github.com/ddletotam/ddmailserver/internal/smtp/client"
	taskpkg "github.com/ddletotam/ddmailserver/internal/task"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// SchedulerDeps bundles all dependencies the Scheduler needs.
// Required fields are checked in NewScheduler; passing zero/nil for those
// is a programming error.
type SchedulerDeps struct {
	Pool            *Pool  // required
	Database        *db.DB // required
	IntervalSeconds int    // required, must be > 0
	GoogleOAuth     *oauth.GoogleOAuth
	MicrosoftOAuth  *oauth.MicrosoftOAuth
	NotifyHub       *notify.Hub
	Hostname        string
	Analyzer        *parser.Analyzer
	DKIMSigner      *dkimsign.Signer // nil → direct delivery goes unsigned
}

// Scheduler schedules periodic tasks for mail synchronization
type Scheduler struct {
	pool                     *Pool
	database                 *db.DB
	interval                 time.Duration
	ctx                      context.Context
	cancel                   context.CancelFunc
	googleOAuth              *oauth.GoogleOAuth
	microsoftOAuth           *oauth.MicrosoftOAuth
	notifyHub                *notify.Hub
	hostname                 string
	analyzer                 *parser.Analyzer
	dkimSigner               *dkimsign.Signer
	spamCleanupLastRun       time.Time
	accountLogCleanupLastRun time.Time
	vaultCleanupLastRun      time.Time
	journalCompactLastRun    time.Time

	// outboxKick coalesces TriggerOutbox calls: capacity 1, so any number of
	// sends while a scan is pending or running cost one more scan, not one
	// goroutine each. Drained by outboxLoop.
	outboxKick chan struct{}
	// cycleKick coalesces TriggerCycle calls the same way: one extra
	// scheduling pass, run by the Start loop so it never overlaps a tick.
	cycleKick chan struct{}
	// sendMu serialises scheduleSMTPSend between the periodic tick and the
	// outbox loop. Not a correctness guard — the atomic claim in the send
	// tasks is — just no reason to read the same pending rows twice at once.
	sendMu sync.Mutex
}

// NewScheduler creates a new task scheduler with all dependencies wired up.
// Pool, Database and IntervalSeconds are required; the remaining fields are
// optional but several features are no-ops when their dependency is nil.
func NewScheduler(deps SchedulerDeps) *Scheduler {
	if deps.Pool == nil || deps.Database == nil {
		panic("worker.NewScheduler: Pool and Database are required")
	}
	if deps.IntervalSeconds <= 0 {
		panic("worker.NewScheduler: IntervalSeconds must be > 0")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		pool:           deps.Pool,
		database:       deps.Database,
		interval:       time.Duration(deps.IntervalSeconds) * time.Second,
		ctx:            ctx,
		cancel:         cancel,
		googleOAuth:    deps.GoogleOAuth,
		microsoftOAuth: deps.MicrosoftOAuth,
		notifyHub:      deps.NotifyHub,
		hostname:       deps.Hostname,
		analyzer:       deps.Analyzer,
		dkimSigner:     deps.DKIMSigner,
		outboxKick:     make(chan struct{}, 1),
		cycleKick:      make(chan struct{}, 1),
	}
}

// TriggerCycle runs one scheduling pass now instead of at the next tick.
//
// Called after the user edits an account or source: the edit lifts the login
// pause (auth_backoff), and new credentials should be tried right away, not
// up to a full interval later. Never blocks; calls coalesce.
func (s *Scheduler) TriggerCycle() {
	select {
	case s.cycleKick <- struct{}{}:
	default:
	}
}

// TriggerOutbox picks up queued outbound mail right now instead of waiting for
// the next scheduler tick.
//
// Sending is asynchronous: the send handler only inserts an outbox row. Without
// this, that row waited for the periodic cycle — up to a full interval (60s in
// the shipped config) before the message even reached SMTP. Everything
// downstream inherited the delay, most visibly the copy in Sent, which only
// exists once the send succeeds: the desktop refetches conversations right after
// «Отправить» and found nothing there, so a reply sent from a different address
// had no conversation to jump to (contract §1).
//
// Safe to call from a request handler: it never blocks. Calls coalesce — a
// burst of sends triggers one scan of the outbox (see outboxLoop), not a
// goroutine per call. Double delivery is prevented further down: a send task
// atomically claims its row ('pending' → 'sending') before sending, so a row
// picked up by two scans is sent once.
func (s *Scheduler) TriggerOutbox() {
	select {
	case s.outboxKick <- struct{}{}:
	default: // a scan is already pending; it will see this message too
	}
}

// outboxLoop runs one outbox scan per (coalesced) TriggerOutbox call.
func (s *Scheduler) outboxLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.outboxKick:
			s.scheduleSMTPSend()
		}
	}
}

// TriggerSyncForAccount submits an immediate sync task for a single account.
// This is called by the IDLE manager when new mail is detected.
func (s *Scheduler) TriggerSyncForAccount(account *models.Account) {
	// Refresh OAuth token if needed
	if account.IsOAuth() {
		if err := s.refreshAccountOAuthToken(account); err != nil {
			log.Printf("IDLE trigger: failed to refresh OAuth token for %s: %v", account.Email, err)
		}
	}

	task := imapclient.NewSyncTask(account, s.database)
	if s.notifyHub != nil {
		userID := account.UserID
		task.SetNotifyFunc(func(n imapclient.NewMailNotice) {
			s.notifyHub.Publish(notify.Event{
				UserID:    userID,
				Type:      notify.EventNewMessage,
				Username:  n.Username,
				Mailbox:   n.Mailbox,
				Count:     n.Count,
				From:      n.From,
				Subject:   n.Subject,
				MessageID: n.MessageID,
				NewCount:  n.NewCount,
			})
		})
		task.SetFlagsNotifyFunc(func(int) {
			// Read/starred in a client of the source account — tell the
			// desktop to refresh unread state (no toast, just a delta pull).
			s.notifyHub.Publish(notify.Event{
				UserID:  userID,
				Type:    notify.EventFlagsChanged,
				Mailbox: "INBOX",
			})
		})
		task.SetExpungeNotifyFunc(expungePublisher(s.notifyHub, userID))
	}
	if s.analyzer != nil {
		task.SetAnalyzer(s.analyzer)
	}
	task.SetOAuthRefresher(func(a *models.Account) error {
		return s.refreshAccountOAuthTokenForce(a, true)
	})

	s.submit(task, fmt.Sprintf("IDLE-triggered sync for %s", account.Email))
}

// expungePublisher turns a sync's "vanished upstream" notice into an expunge
// event: untagged EXPUNGE for IMAP sessions with that folder selected, and an
// `expunge` push that makes the desktop pull the change journal.
func expungePublisher(hub *notify.Hub, userID int64) func(imapclient.ExpungeNotice) {
	return func(n imapclient.ExpungeNotice) {
		hub.Publish(notify.Event{
			UserID:   userID,
			Type:     notify.EventExpunge,
			Username: n.Username,
			FolderID: n.FolderID,
			Mailbox:  n.Mailbox,
			SeqNums:  n.SeqNums,
		})
	}
}

// submit puts a task in the pool and logs the outcome uniformly.
// `what` names the work in log lines ("sync task for user@host").
//
// A duplicate — the same logical task already waiting for a worker — is
// deliberately silent: the scheduler re-offers every account and every source
// on every tick, so this is the normal steady state whenever work outlives one
// interval, not a failure worth a log line. Returns true only when the task
// was actually queued.
func (s *Scheduler) submit(t Task, what string) bool {
	err := s.pool.Submit(t)
	switch {
	case err == nil:
		log.Printf("Submitted %s", what)
		return true
	case errors.Is(err, ErrDuplicateTask), errors.Is(err, ErrPoolStopped):
		// Stopped: shutting down, a cycle still in progress — not news.
		return false
	default:
		log.Printf("Failed to submit %s: %v", what, err)
		return false
	}
}

func (s *Scheduler) getHostname() string {
	if s.hostname != "" {
		return s.hostname
	}
	return "localhost"
}

// Start starts the scheduler
func (s *Scheduler) Start() {
	log.Printf("Scheduler started with interval %v", s.interval)

	go s.outboxLoop()

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

		case <-s.cycleKick:
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

	// Credentials the providers currently reject: nothing below logs in
	// with them until their pause is over (package authfail).
	paused := s.loadAuthPaused()

	// Schedule IMAP sync tasks
	s.scheduleIMAPSync(paused)

	// Schedule SMTP send tasks
	s.scheduleSMTPSend()

	// Schedule CalDAV sync tasks
	s.scheduleCalendarSync(paused)

	// Schedule CardDAV contact sync tasks
	s.scheduleContactSync(paused)

	// Schedule contact sync push tasks (reverse sync to remote CardDAV)
	s.scheduleContactSyncPush(paused)

	// Schedule flag sync tasks (reverse proxy for external accounts)
	s.scheduleFlagSync(paused)

	// Schedule calendar event reverse sync tasks
	s.scheduleCalendarEventSync(paused)

	// Tell users about credentials that just started being rejected (once
	// per transition).
	s.publishAuthFailures()

	// Schedule calendar sync warning emails (one per source per day when
	// retries pile up — see CalendarSyncWarningTask for the threshold).
	s.scheduleCalendarSyncWarning()

	// Run spam cleanup (periodically delete old spam)
	s.runSpamCleanup()

	// Run account log cleanup (delete logs older than 5 days)
	s.runAccountLogCleanup()

	// Run vault cleanup (permanently delete soft-deleted messages older than 30 days)
	s.runVaultCleanup()

	// Prune the change journal (keep 30 days; advance low-watermark)
	s.runJournalCompaction()

	imapQueueLen, smtpQueueLen := s.pool.QueueLength()
	log.Printf("Current queue lengths - IMAP: %d, SMTP: %d", imapQueueLen, smtpQueueLen)
}

// refreshAccountOAuthToken refreshes the OAuth token for an account if it's
// near expiry. Delegates to the shared oauth.AccountTokenRefresher.
func (s *Scheduler) refreshAccountOAuthToken(account *models.Account) error {
	return s.refreshAccountOAuthTokenForce(account, false)
}

// refreshAccountOAuthTokenForce refreshes the OAuth token. If force is true,
// skips the expiry check (used when auth failed despite the stored expiry
// being in the future — providers can revoke tokens early).
func (s *Scheduler) refreshAccountOAuthTokenForce(account *models.Account, force bool) error {
	refresher := oauth.NewAccountTokenRefresher(s.googleOAuth, s.microsoftOAuth, s.database)
	refreshed, err := refresher.Refresh(account, force)
	if err != nil {
		return err
	}
	if refreshed {
		log.Printf("OAuth token refreshed for IMAP account %s, new expiry: %v", account.Email, account.OAuthTokenExpiry)
	}
	return nil
}

// scheduleIMAPSync schedules IMAP synchronization tasks.
// For poll-mode accounts: syncs when the account's poll_interval has elapsed.
// For idle-mode accounts: syncs as a safety-net fallback every 300s.
func (s *Scheduler) scheduleIMAPSync(paused authPaused) {
	accounts, err := s.getAllEnabledAccounts()
	if err != nil {
		log.Printf("Failed to get enabled accounts: %v", err)
		return
	}

	synced := 0
	for _, account := range accounts {
		// Login paused after rejected credentials. LastSync does not move
		// on a failed sync, so without this the poll retried every tick.
		if paused.has(models.AuthSubjectIMAP, account.ID) {
			continue
		}

		interval := 300 // default fallback for idle-mode accounts
		if account.SyncMode == "poll" && account.PollInterval >= 120 {
			interval = account.PollInterval
		}

		// Skip if not enough time since last sync
		if account.LastSync != 0 && timeutil.Now()-account.LastSync < int64(interval)*1000 {
			continue
		}

		// Refresh OAuth token if needed
		if account.IsOAuth() {
			if err := s.refreshAccountOAuthToken(account); err != nil {
				log.Printf("Failed to refresh OAuth token for %s: %v", account.Email, err)
				// invalid_grant: the grant is gone, a pause like a
				// rejected password.
				authGuard(s.database).Report(authfail.AccountIMAP(account), err)
				continue
			}
		}

		task := imapclient.NewSyncTask(account, s.database)
		if s.notifyHub != nil {
			uid := account.UserID
			task.SetNotifyFunc(func(n imapclient.NewMailNotice) {
				s.notifyHub.Publish(notify.Event{
					UserID:    uid,
					Type:      notify.EventNewMessage,
					Username:  n.Username,
					Mailbox:   n.Mailbox,
					Count:     n.Count,
					From:      n.From,
					Subject:   n.Subject,
					MessageID: n.MessageID,
					NewCount:  n.NewCount,
				})
			})
			task.SetFlagsNotifyFunc(func(int) {
				// See TriggerSyncForAccount — same push, poll path.
				s.notifyHub.Publish(notify.Event{
					UserID:  uid,
					Type:    notify.EventFlagsChanged,
					Mailbox: "INBOX",
				})
			})
			task.SetExpungeNotifyFunc(expungePublisher(s.notifyHub, uid))
		}
		if s.analyzer != nil {
			task.SetAnalyzer(s.analyzer)
		}
		task.SetOAuthRefresher(func(a *models.Account) error {
			return s.refreshAccountOAuthTokenForce(a, true)
		})

		if s.submit(task, fmt.Sprintf("sync task for %s", account.Email)) {
			synced++
		}
	}

	if synced > 0 {
		log.Printf("Submitted %d IMAP sync tasks", synced)
	}
}

// scheduleSMTPSend schedules SMTP send tasks for pending outbox messages
func (s *Scheduler) scheduleSMTPSend() {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.ctx.Err() != nil {
		return
	}

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

	smtpPaused, err := s.database.GetAuthPausedIDs(models.AuthSubjectSMTP)
	if err != nil {
		log.Printf("Failed to load SMTP auth pauses: %v", err)
	}
	deferred := 0

	for _, msg := range messages {
		// Relay account whose SMTP password the provider rejects: the
		// message stays pending (nothing is lost, retries are not spent)
		// and goes out once the pause is over or the password is edited.
		if msg.AccountID != 0 && smtpPaused[msg.AccountID] {
			deferred++
			continue
		}

		var sendTask taskpkg.Task

		// Tell the user's clients the moment the copy lands in Sent. Queueing
		// is not sending, so «отправлено» reaches the client long before the
		// conversation the message belongs to exists — a quiet refresh signal
		// is what closes that gap (see notify.EventMessageSent).
		notifySent := func(userID int64) func() {
			if s.notifyHub == nil {
				return nil
			}
			return func() {
				s.notifyHub.Publish(notify.Event{
					UserID:  userID,
					Type:    notify.EventMessageSent,
					Mailbox: "Sent",
				})
			}
		}(msg.UserID)

		if msg.AccountID == 0 {
			// Direct delivery for local domain senders
			direct := smtpclient.NewDirectSendTask(msg, s.database, s.getHostname(), s.dkimSigner)
			direct.SetSentNotifyFunc(notifySent)
			sendTask = direct
		} else {
			// Relay through external SMTP account
			account, err := s.database.GetAccountByID(msg.AccountID)
			if err != nil {
				log.Printf("Failed to get account %d for message %d: %v", msg.AccountID, msg.ID, err)
				continue
			}
			relay := smtpclient.NewSendTask(msg, account, s.database)
			relay.SetSentNotifyFunc(notifySent)
			sendTask = relay
		}

		task := sendTask

		s.submit(task, fmt.Sprintf("send task for message %d", msg.ID))
	}
	if deferred > 0 {
		log.Printf("%d outbox message(s) held: SMTP login of their account is paused after rejected credentials", deferred)
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

	paused := s.loadAuthPaused()
	for _, source := range sources {
		if !source.SyncEnabled || paused.has(models.AuthSubjectCalDAV, source.ID) {
			continue
		}

		var task Task
		switch source.SourceType {
		case "caldav":
			task = NewCalendarSyncTask(source, s.database, s.googleOAuth, s.microsoftOAuth, s.notifyHub)
		case "ics_url":
			task = NewICSSyncTask(source, s.database, s.notifyHub)
		default:
			continue
		}

		s.submit(task, fmt.Sprintf("immediate calendar sync for %s", source.Name))
	}
}

// scheduleCalendarSync schedules CalDAV and ICS URL synchronization tasks
func (s *Scheduler) scheduleCalendarSync(paused authPaused) {
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
		if !source.NeedsSync() || paused.has(models.AuthSubjectCalDAV, source.ID) {
			continue
		}

		var task Task
		switch source.SourceType {
		case "caldav":
			task = NewCalendarSyncTask(source, s.database, s.googleOAuth, s.microsoftOAuth, s.notifyHub)
		case "ics_url":
			task = NewICSSyncTask(source, s.database, s.notifyHub)
		default:
			log.Printf("Unknown source type: %s", source.SourceType)
			continue
		}

		s.submit(task, fmt.Sprintf("calendar sync task for %s (%s)", source.Name, source.SourceType))
	}
}

// scheduleContactSync schedules CardDAV contact synchronization tasks
func (s *Scheduler) scheduleContactSync(paused authPaused) {
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
		if !source.NeedsSync() || paused.has(models.AuthSubjectCardDAV, source.ID) {
			continue
		}

		task := NewContactSyncTask(source, s.database, s.googleOAuth, s.microsoftOAuth)

		s.submit(task, fmt.Sprintf("contact sync task for %s (%s)", source.Name, source.SourceType))
	}
}

// scheduleContactSyncPush schedules contact sync push tasks
// Pushes local contact changes back to external CardDAV servers
func (s *Scheduler) scheduleContactSyncPush(paused authPaused) {
	sourceIDs, err := s.database.GetSourcesWithPendingContactSync()
	if err != nil {
		log.Printf("Failed to get sources with pending contact sync push: %v", err)
		return
	}

	if len(sourceIDs) == 0 {
		return
	}

	log.Printf("Found %d contact sources with pending sync push", len(sourceIDs))

	for _, sourceID := range sourceIDs {
		if paused.has(models.AuthSubjectCardDAV, sourceID) {
			continue
		}
		source, err := s.database.GetContactSourceByID(sourceID)
		if err != nil {
			log.Printf("Failed to get contact source %d for sync push: %v", sourceID, err)
			continue
		}

		if !source.SyncEnabled {
			continue
		}

		task := NewContactSyncPushTask(source, s.database)

		s.submit(task, fmt.Sprintf("contact sync push task for %s", source.Name))
	}
}

// scheduleCalendarEventSync schedules calendar event reverse sync tasks
// Pushes local event changes back to external CalDAV servers
func (s *Scheduler) scheduleCalendarEventSync(paused authPaused) {
	sourceIDs, err := s.database.GetSourcesWithPendingCalendarEventSync()
	if err != nil {
		log.Printf("Failed to get sources with pending calendar event sync: %v", err)
		return
	}

	if len(sourceIDs) == 0 {
		return
	}

	log.Printf("Found %d calendar sources with pending event sync", len(sourceIDs))

	for _, sourceID := range sourceIDs {
		if paused.has(models.AuthSubjectCalDAV, sourceID) {
			continue
		}
		source, err := s.database.GetCalendarSourceByID(sourceID)
		if err != nil {
			log.Printf("Failed to get calendar source %d for event sync: %v", sourceID, err)
			continue
		}

		if !source.SyncEnabled {
			continue
		}

		task := NewCalendarEventSyncTask(source, s.database)

		s.submit(task, fmt.Sprintf("calendar event sync task for %s", source.Name))
	}
}

// scheduleCalendarSyncWarning submits a single task that scans every
// CalDAV source for stuck reverse-sync entries and sends a system email
// when the threshold + cooldown say it's time. The task itself rate-limits
// per source via `calendar_source_warnings`, so submitting on every
// scheduler tick is cheap and idempotent.
func (s *Scheduler) scheduleCalendarSyncWarning() {
	task := NewCalendarSyncWarningTask(s.database, s.notifyHub, s.getHostname())
	s.submit(task, "calendar sync warning task")
}

// scheduleFlagSync schedules flag synchronization tasks for external IMAP accounts
// This implements "reverse proxy" mode - pushing local flag changes back to source servers
func (s *Scheduler) scheduleFlagSync(paused authPaused) {
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
		if paused.has(models.AuthSubjectIMAP, accountID) {
			continue
		}
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

		s.submit(task, fmt.Sprintf("flag sync task for %s", account.Email))
	}
}

// runSpamCleanup deletes spam messages older than 1 year
// Only runs once per day to avoid excessive database operations
func (s *Scheduler) runSpamCleanup() {
	// Only run once per day
	if time.Since(s.spamCleanupLastRun) < 24*time.Hour {
		return
	}

	// Delete spam older than 365 days
	deleted, err := s.database.DeleteOldSpamMessages(365)
	if err != nil {
		log.Printf("Failed to cleanup old spam: %v", err)
		return
	}

	if deleted > 0 {
		log.Printf("Spam cleanup: deleted %d messages older than 1 year", deleted)
	}

	s.spamCleanupLastRun = time.Now()
}

// runAccountLogCleanup deletes account log entries older than 5 days.
// Only runs once per day.
func (s *Scheduler) runAccountLogCleanup() {
	if time.Since(s.accountLogCleanupLastRun) < 24*time.Hour {
		return
	}

	deleted, err := s.database.CleanupAccountLogs(5)
	if err != nil {
		log.Printf("Failed to cleanup account logs: %v", err)
		return
	}

	if deleted > 0 {
		log.Printf("Account log cleanup: deleted %d entries older than 5 days", deleted)
	}

	// Login pauses of deleted accounts and sources (no foreign key to them).
	if n, err := s.database.CleanupOrphanAuthBackoffs(); err != nil {
		log.Printf("Failed to cleanup auth backoff: %v", err)
	} else if n > 0 {
		log.Printf("Auth backoff cleanup: removed %d states of deleted accounts/sources", n)
	}

	s.accountLogCleanupLastRun = time.Now()
}

// runVaultCleanup permanently deletes soft-deleted ("vault") messages older
// than the retention window. The vault is the recoverable store for messages
// expunged from non-Trash folders; without a purge it grows unbounded (it was
// the bulk of the rows bloating per-folder scans). Only runs once per day.
const vaultRetention = 30 * 24 * time.Hour

func (s *Scheduler) runVaultCleanup() {
	if time.Since(s.vaultCleanupLastRun) < 24*time.Hour {
		return
	}

	deleted, err := s.database.PurgeVaultMessages(vaultRetention)
	if err != nil {
		log.Printf("Failed to cleanup vault messages: %v", err)
		return
	}

	if deleted > 0 {
		log.Printf("Vault cleanup: purged %d soft-deleted messages older than 30 days", deleted)
	}

	s.vaultCleanupLastRun = time.Now()
}

// journalRetention bounds how far back the change journal is kept. A client
// offline longer than this loses its incremental cursor and full-resyncs
// (the low-watermark, advanced on prune, signals that). Matches the vault
// window so a delete tombstone outlives its purgeable message.
const journalRetention = 30 * 24 * time.Hour

// runJournalCompaction prunes change-journal entries older than the retention
// window and advances the low-watermark. Only runs once per day.
func (s *Scheduler) runJournalCompaction() {
	if time.Since(s.journalCompactLastRun) < 24*time.Hour {
		return
	}

	cutoffMs := time.Now().Add(-journalRetention).UnixMilli()
	if _, err := s.database.CompactMessageChanges(cutoffMs); err != nil {
		log.Printf("Failed to compact change journal: %v", err)
		return
	}

	s.journalCompactLastRun = time.Now()
}
