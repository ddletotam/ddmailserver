package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/oauth"
	"github.com/ddletotam/ddmailserver/internal/tlsverify"
	"github.com/emersion/go-imap"
	idle "github.com/emersion/go-imap-idle"
	imapClient "github.com/emersion/go-imap/client"
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
	// auth gates logins on the account's IMAP credentials (auth_backoff):
	// a rejected password must not be retried by the reconnect loop.
	auth *authfail.Guard
	// authPollInterval: how often a watcher sitting out an auth pause checks
	// whether the pause was lifted (password edited, a sync logged in).
	authPollInterval time.Duration
}

// NewIdleManager creates a new IDLE manager.
func NewIdleManager(database *db.DB) *IdleManager {
	ctx, cancel := context.WithCancel(context.Background())
	var store authfail.Store
	if database != nil {
		store = database
	}
	return &IdleManager{
		database:         database,
		watchers:         make(map[int64]context.CancelFunc),
		ctx:              ctx,
		cancel:           cancel,
		auth:             authfail.NewGuard(store),
		authPollInterval: 30 * time.Second,
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
	// Переживает переподключения: по UIDNEXT прошлой сессии видно, пришло ли
	// письмо, пока соединения не было (см. idleSession).
	sess := &idleSession{}

	for {
		if ctx.Err() != nil {
			return
		}

		// The provider rejected the password: no session until the pause
		// is over. Before this the loop reconnected every 10s…5m forever
		// and each reconnect was one more failed LOGIN on the provider's
		// side — the pattern that gets accounts locked.
		if !m.auth.Allow(authfail.AccountIMAP(account)) {
			if !m.waitAuthPause(ctx, account.ID) {
				return
			}
			fresh, ok := m.reloadAccount(account)
			if !ok {
				return
			}
			account = fresh
			continue
		}

		sess.idleSince = time.Time{}
		err := m.runIdleSession(ctx, account, sess)
		if ctx.Err() != nil {
			return
		}
		if authfail.Is(err) {
			m.auth.Rejected(authfail.AccountIMAP(account), err)
			m.accountLog(account.ID, "error", "%v — login paused, see the account page", err)
			backoff = baseBackoff
			continue
		}

		var idleLen time.Duration
		if !sess.idleSince.IsZero() {
			idleLen = time.Since(sess.idleSince)
		}
		wait, next := reconnectDelay(err, idleLen, backoff, baseBackoff, maxBackoff)
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
		fresh, ok := m.reloadAccount(account)
		if !ok {
			return
		}
		account = fresh
	}
}

// reloadAccount re-reads the account (credentials may have changed). ok is
// false when the account was disabled and the watcher must stop; a read error
// keeps the old copy.
func (m *IdleManager) reloadAccount(account *models.Account) (*models.Account, bool) {
	fresh, err := m.database.GetAccountByID(account.ID)
	if err != nil {
		log.Printf("IDLE watcher [%s]: failed to reload account: %v", account.Email, err)
		return account, true
	}
	if !fresh.Enabled {
		log.Printf("IDLE watcher [%s]: account disabled, stopping", account.Email)
		return nil, false
	}
	return fresh, true
}

// waitAuthPause sleeps while the account's IMAP login is paused, waking every
// authPollInterval to notice a lifted pause early (password edited, a sync
// logged in). Returns false when ctx is done.
func (m *IdleManager) waitAuthPause(ctx context.Context, accountID int64) bool {
	for {
		wait := m.authPollInterval
		st, err := m.database.GetAuthBackoff(models.AuthSubjectIMAP, accountID)
		if err == nil {
			now := time.Now().UnixMilli()
			if !st.Paused(now) {
				return true
			}
			if left := time.Duration(st.NextAttemptAt-now) * time.Millisecond; left < wait {
				wait = left
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

// idleSession — состояние IDLE-наблюдателя одного аккаунта между сессиями.
type idleSession struct {
	// uidNext — UIDNEXT INBOX при последнем SELECT; 0 — сессий ещё не было.
	uidNext uint32
	// idleSince — когда текущая сессия вошла в IDLE; нулевое — не дошла.
	idleSince time.Time
}

// reconnectDelay решает, сколько ждать перед новой IDLE-сессией и каким станет
// backoff. idleLen — сколько сессия пробыла в IDLE (0 — не дошла до него).
// Возвращает (пауза, следующий backoff).
//
// Правила:
//   - плановое обновление по TTL — не сбой: подключаемся немедленно, backoff
//     возвращается к базовому;
//   - сессия, проработавшая в IDLE хотя бы healthyIdle, здорова, и её обрыв —
//     решение сервера, а не наш сбой: переподключаемся через quickReconnect,
//     backoff к базовому. Замерено на проде 2026-11-10: Яндекс закрывает
//     IDLE-соединение (FIN без BYE, через 0,5 с RST) через 25 с … 7 мин,
//     в среднем через 2,5 мин, у разных аккаунтов почти одновременно — у него
//     ротируются узлы. Прежнее правило («здорова, если прожила 2 мин»)
//     удваивало паузу до 5 минут, и ~16% суток ящик был без IDLE;
//   - сбой до IDLE (connect, login, select) — обычное удвоение до потолка.
func reconnectDelay(err error, idleLen, current, base, max time.Duration) (time.Duration, time.Duration) {
	const healthyIdle = 10 * time.Second
	const quickReconnect = 2 * time.Second

	if errors.Is(err, errIdleRefresh) {
		return 0, base
	}
	if idleLen >= healthyIdle {
		return quickReconnect, base
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
func (m *IdleManager) runIdleSession(ctx context.Context, account *models.Account, sess *idleSession) error {
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
				return fmt.Errorf("auth after refresh: %w", authfail.MarkLoginRefusal(authErr2))
			}
		}
	} else {
		if err := conn.Login(account.IMAPUsername, account.IMAPPassword); err != nil {
			return fmt.Errorf("login: %w", authfail.MarkLoginRefusal(err))
		}
	}
	m.auth.Accepted(authfail.AccountIMAP(account))

	// Select INBOX
	mbox, err := conn.Select("INBOX", false)
	if err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	// IDLE сообщает только о том, что случилось после его начала. Письмо,
	// пришедшее, пока соединения не было, видно по сдвигу UIDNEXT. Если в
	// прошлой сессии письмо уже пришло через IDLE, сдвиг даст один лишний
	// инкрементальный синк — дёшево, а пропуск письма до резервного цикла
	// (5 минут) — нет.
	missed := sess.uidNext != 0 && mbox.UidNext != 0 && mbox.UidNext != sess.uidNext
	if mbox.UidNext != 0 {
		sess.uidNext = mbox.UidNext
	}
	if missed && m.syncCallback != nil {
		m.accountLog(account.ID, "info", "INBOX changed while reconnecting (UIDNEXT moved), triggering sync")
		m.syncCallback(account)
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

	sess.idleSince = time.Now()

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
		bye := ""
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
				switch u := upd.(type) {
				case *imapClient.MailboxUpdate:
					triggered = true
					break wait
				case *imapClient.StatusUpdate:
					if u.Status != nil && u.Status.Type == imap.StatusRespBye {
						// Соединение библиотека уже закрыла; ошибку вернёт
						// idleDone следующим, причину добавим к ней.
						bye = statusText(u.Status)
					}
				}
				// Ignore other update types, keep waiting

			case err := <-idleDone:
				// IDLE ended unexpectedly (connection lost)
				if err != nil {
					// Причину сервер называет в «* BYE …», но библиотека
					// отдаёт его в Updates уже после закрытия соединения,
					// а ошибка Idle — просто «connection closed». Без текста
					// BYE не отличить решение сервера от обрыва TCP.
					if bye == "" {
						bye = byeReason(updates, 500*time.Millisecond)
					}
					if bye != "" {
						return fmt.Errorf("idle: %w (server BYE: %s)", err, bye)
					}
					return fmt.Errorf("idle: %w (no BYE from server)", err)
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

// byeReason ждёт до wait «* BYE» в уже прочитанных обновлениях и возвращает
// его текст; прочие обновления выбрасывает — сессия всё равно закончена.
func byeReason(ch <-chan imapClient.Update, wait time.Duration) string {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		select {
		case upd := <-ch:
			if su, ok := upd.(*imapClient.StatusUpdate); ok && su.Status != nil && su.Status.Type == imap.StatusRespBye {
				return statusText(su.Status)
			}
		case <-deadline.C:
			return ""
		}
	}
}

// statusText — текст статусного ответа вместе с кодом, если он есть.
func statusText(st *imap.StatusResp) string {
	if st.Code != "" {
		return fmt.Sprintf("[%s] %s", st.Code, st.Info)
	}
	return st.Info
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
