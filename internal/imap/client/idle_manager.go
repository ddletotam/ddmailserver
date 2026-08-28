package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	idle "github.com/emersion/go-imap-idle"
	imapClient "github.com/emersion/go-imap/client"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/oauth"
	"github.com/yourusername/mailserver/internal/tlsverify"
)

// idleSessionTTL — предельный возраст одной IDLE-сессии, после которого
// соединение пересоздаётся, даже если с ним всё «хорошо».
//
// Замерено на проде 2026-08-28: сессия ddanilin@appsec.global (Яндекс) висела
// 10 часов без единой ошибки — TCP живой, keepalive отвечает, библиотека честно
// перевыпускает IDLE каждые 25 минут (`idle.Client.LogoutTimeout`), — и при
// этом сервер перестал слать EXISTS: письмо, доставленное в 08:40:25, не
// подняло ни одного уведомления и приехало только плановым синком в 08:43:32.
// Соседний яндексовый аккаунт на той же машине уведомления получал. Отличить
// «тихо, потому что писем нет» от «тихо, потому что канал умер» изнутри
// сессии нечем, поэтому мы её просто не держим дольше TTL: цена — один
// реконнект в 15 минут на аккаунт, выигрыш — мгновенная доставка не пропадает
// до перезапуска сервиса.
const idleSessionTTL = 15 * time.Minute

// errIdleRefresh — плановое завершение сессии по TTL. Не ошибка: не логируется
// как сбой и не наращивает backoff переподключения.
var errIdleRefresh = errors.New("idle session refresh")

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

	const baseBackoff = 10 * time.Second
	const maxBackoff = 5 * time.Minute
	backoff := baseBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		err := m.runIdleSession(ctx, account)
		if ctx.Err() != nil {
			return
		}

		wait, next := reconnectDelay(err, time.Since(started), backoff, baseBackoff, maxBackoff)
		if err != nil && !errors.Is(err, errIdleRefresh) {
			m.accountLog(account.ID, "error", "%v, reconnecting in %v", err, wait)
		}
		backoff = next

		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
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

// reconnectDelay решает, сколько ждать перед новой IDLE-сессией и каким станет
// backoff. Возвращает (пауза, следующий backoff).
//
// Правила:
//   - плановое обновление по TTL — не сбой: подключаемся немедленно, backoff
//     возвращается к базовому;
//   - сессия, прожившая дольше healthySession, тоже считается здоровой: разрыв
//     после часа работы не имеет отношения к предыдущему разрыву, и наказывать
//     за него пятиминутной паузой не за что. Раньше backoff удваивался после
//     ЛЮБОГО завершения сессии и не сбрасывался никогда, так что долгоживущий
//     аккаунт со временем восстанавливался только через maxBackoff;
//   - быстрый повторный сбой — обычное удвоение до потолка.
func reconnectDelay(err error, sessionLen, current, base, max time.Duration) (time.Duration, time.Duration) {
	const healthySession = 2 * time.Minute

	if errors.Is(err, errIdleRefresh) {
		return 0, base
	}
	if sessionLen > healthySession {
		current = base
	}
	next := current * 2
	if next > max {
		next = max
	}
	return current, next
}

// refreshOAuthToken refreshes the OAuth token for an account if needed.
// If force is true, skips the expiry check and always refreshes. Delegates
// to the shared oauth.AccountTokenRefresher.
func (m *IdleManager) refreshOAuthToken(account *models.Account, force bool) error {
	refresher := oauth.NewAccountTokenRefresher(m.googleOAuth, m.microsoftOAuth, m.database)
	refreshed, err := refresher.Refresh(account, force)
	if err != nil {
		return err
	}
	if refreshed {
		m.accountLog(account.ID, "info", "OAuth token refreshed, new expiry: %v", account.OAuthTokenExpiry)
	}
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
		conn, err = imapClient.DialTLS(addr, tlsverify.Config(account.IMAPHost))
	} else {
		// Тот же обязательный STARTTLS, что и в клиенте синка: IDLE-сессия
		// логинится теми же учётными данными и не имеет права уронить их в
		// открытый текст.
		conn, err = imapClient.Dial(addr)
		if err == nil {
			if tlsErr := upgradeStartTLS(conn, account.IMAPHost); tlsErr != nil {
				conn.Logout()
				return tlsErr
			}
		}
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

	// Возраст сессии считаем от установленного соединения: по его истечении
	// выходим и подключаемся заново (см. idleSessionTTL).
	expiry := time.NewTimer(idleSessionTTL)
	defer expiry.Stop()

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

			case <-expiry.C:
				// Сессия отжила своё — закрываемся и переподключаемся, не
				// дожидаясь ошибки, которой может и не быть никогда.
				close(stop)
				<-idleDone
				m.accountLog(account.ID, "info", "плановое обновление IDLE-сессии (%v)", idleSessionTTL)
				return errIdleRefresh

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
