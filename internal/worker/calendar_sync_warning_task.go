package worker

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/task"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// calendarSyncWarningMinRetries is the failure threshold below which we keep
// quiet. A single 5xx blip shouldn't pollute the inbox; we wait until the
// entry has bounced at least this many times before counting it as "stuck".
const calendarSyncWarningMinRetries = 3

// calendarSyncWarningMinIntervalMs is the minimum gap between two consecutive
// "calendar sync failed" emails for the same source. 24h matches the user's
// "one per day, until fixed" requirement.
const calendarSyncWarningMinIntervalMs int64 = 24 * 60 * 60 * 1000

// CalendarSyncWarningTask scans every enabled CalDAV source and, for each
// one that has accumulated stuck reverse-sync entries, drops a synthetic
// system email into the user's local inbox so the failure can't be ignored.
type CalendarSyncWarningTask struct {
	database  *db.DB
	notifyHub *notify.Hub
	hostname  string
}

func NewCalendarSyncWarningTask(database *db.DB, notifyHub *notify.Hub, hostname string) *CalendarSyncWarningTask {
	return &CalendarSyncWarningTask{database: database, notifyHub: notifyHub, hostname: hostname}
}

func (t *CalendarSyncWarningTask) Type() task.Type { return task.TypeIMAP }
func (t *CalendarSyncWarningTask) Priority() int   { return 3 }
func (t *CalendarSyncWarningTask) String() string  { return "CalendarSyncWarning" }

func (t *CalendarSyncWarningTask) Execute(ctx context.Context) error {
	sources, err := t.database.GetAllEnabledCalendarSources()
	if err != nil {
		return fmt.Errorf("get sources: %w", err)
	}

	now := timeutil.Now()
	for _, src := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if src.SourceType != "caldav" {
			// Read-only sources (ics_url) don't have a reverse queue, so
			// nothing to warn about.
			continue
		}

		failureCount, err := t.database.CountPendingCalendarSyncFailures(src.ID, calendarSyncWarningMinRetries)
		if err != nil {
			log.Printf("warning: count failures for source %d: %v", src.ID, err)
			continue
		}

		lastSent, _, _ := t.database.GetCalendarSourceWarning(src.ID)
		if failureCount == 0 {
			// Clean slate — clear the counter so the next failure starts
			// the 24h cooldown from zero.
			if lastSent != 0 {
				_ = t.database.SetCalendarSourceWarning(src.ID, 0, 0)
			}
			continue
		}
		if now-lastSent < calendarSyncWarningMinIntervalMs {
			continue
		}

		if err := t.sendWarning(src, failureCount); err != nil {
			log.Printf("warning: send for source %d: %v", src.ID, err)
			continue
		}
		_ = t.database.SetCalendarSourceWarning(src.ID, now, failureCount)
	}
	return nil
}

// sendWarning composes the synthetic email and inserts it into the user's
// inbox. Plain text only — no HTML, per the user's spec. The "From" is the
// configured server hostname so address parsing doesn't blow up clients.
func (t *CalendarSyncWarningTask) sendWarning(src *models.CalendarSource, count int) error {
	user, err := t.database.GetUserByID(src.UserID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	inbox, err := t.database.GetOrCreateLocalInbox(user.ID)
	if err != nil {
		return fmt.Errorf("get inbox: %w", err)
	}

	samples, _ := t.database.SampleFailingSyncEntries(src.ID, calendarSyncWarningMinRetries, 10)

	var body strings.Builder
	body.WriteString(fmt.Sprintf("Не удалось синхронизировать %d событий с календарём «%s».\n\n", count, src.Name))
	body.WriteString("Сервер несколько раз пытался отправить изменения на удалённый CalDAV — \nи получил ошибку. Возможные причины:\n")
	body.WriteString("  • протух пароль/токен (особенно у iCloud — нужен app-specific password);\n")
	body.WriteString("  • удалённый сервер временно отказывает (5xx, 4xx);\n")
	body.WriteString("  • квота / права на запись поменялись на их стороне.\n\n")
	body.WriteString("Откройте веб-интерфейс, раздел «Календари» — переавторизуйте источник.\n")
	body.WriteString("Как только хотя бы одна запись из очереди пройдёт, эти письма перестанут приходить.\n\n")

	if len(samples) > 0 {
		body.WriteString("Примеры застрявших операций:\n")
		for _, e := range samples {
			body.WriteString(fmt.Sprintf("  • %s (%s, попыток: %d)", e.UID, e.Operation, e.RetryCount))
			if e.LastError != "" {
				snippet := strings.TrimSpace(e.LastError)
				if len(snippet) > 160 {
					snippet = snippet[:160] + "…"
				}
				body.WriteString(fmt.Sprintf("\n    → %s", snippet))
			}
			body.WriteString("\n")
		}
	}

	hostname := t.hostname
	if hostname == "" {
		hostname = "localhost"
	}
	fromAddr := fmt.Sprintf("SYSTEM <noreply@%s>", hostname)
	now := timeutil.Now()
	subject := fmt.Sprintf("⚠️ Не удалось синхронизировать %d событий — %s", count, src.Name)

	msg := &models.Message{
		UserID:    user.ID,
		FolderID:  inbox.ID,
		MessageID: fmt.Sprintf("calendar-sync-warning-%d-%d@%s", src.ID, now, hostname),
		Subject:   subject,
		From:      fromAddr,
		To:        user.Username,
		Date:      now,
		Body:      body.String(),
		Size:      int64(len(body.String())),
		UID:       0, // will be assigned in Inbox
		Seen:      false,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Use a fresh UID for this folder so IMAP clients pick it up as a new
	// message via UID-monotonicity.
	uid, err := t.database.GetNextUIDForFolder(inbox.ID)
	if err != nil {
		return fmt.Errorf("next uid: %w", err)
	}
	msg.UID = uid

	if err := t.database.CreateMessage(msg); err != nil {
		return fmt.Errorf("create message: %w", err)
	}

	if t.notifyHub != nil {
		t.notifyHub.Publish(notify.Event{
			UserID:    user.ID,
			Type:      notify.EventNewMessage,
			Username:  user.Username,
			Mailbox:   "INBOX",
			Count:     1,
			From:      fromAddr,
			Subject:   subject,
			MessageID: msg.ID,
			NewCount:  1,
		})
	}
	log.Printf("Calendar sync warning email queued for user %d source %d (%d failures)", user.ID, src.ID, count)
	return nil
}
