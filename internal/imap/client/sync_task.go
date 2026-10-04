package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/calendar"
	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/task"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
	"github.com/emersion/go-imap"
)

// NewMailNotice is what a completed sync reports to the notification hub:
// folder totals for IMAP EXISTS plus toast content — the LAST new message
// of the batch (the desktop shows «N новых» when NewCount > 1).
type NewMailNotice struct {
	Username  string
	Mailbox   string
	Count     uint32
	NewCount  int
	From      string
	Subject   string
	MessageID int64
}

type SyncTask struct {
	account    *models.Account
	database   *db.DB
	analyzer   *parser.Analyzer
	notifyFunc func(NewMailNotice)
	priority   int
	// Last non-spam message saved by the current run — toast content.
	lastNew *models.Message
	// How many already-known messages had their flags refreshed from the
	// remote this run (read in another client → seen pulled in). Drives a
	// single flags_changed push at the end of the run so connected desktop
	// clients update unread state without waiting for new mail.
	flagsChanged    int
	flagsNotifyFunc func(changed int)
	// Per-run memo of CheckSpamRules for already-known messages: the flag
	// pass re-evaluates every known message each cycle, and senders repeat.
	spamRuleMemo map[string]spamRuleVerdict
	// Message-IDs present upstream in any synced folder this run — a message
	// seen anywhere has not vanished (sync_vanished.go).
	seenIDs           map[string]bool
	expungeNotifyFunc func(ExpungeNotice)
	// Called to force-refresh the OAuth token when auth fails. The callback
	// is expected to update the account in place (access token, expiry).
	refreshOAuth func(account *models.Account) error
}

func (t *SyncTask) SetNotifyFunc(fn func(NewMailNotice))    { t.notifyFunc = fn }
func (t *SyncTask) SetFlagsNotifyFunc(fn func(changed int)) { t.flagsNotifyFunc = fn }

// SetExpungeNotifyFunc sets the callback for messages removed locally because
// they vanished from the source server (one call per local folder).
func (t *SyncTask) SetExpungeNotifyFunc(fn func(ExpungeNotice)) { t.expungeNotifyFunc = fn }
func (t *SyncTask) SetAnalyzer(analyzer *parser.Analyzer)   { t.analyzer = analyzer }
func (t *SyncTask) SetOAuthRefresher(fn func(account *models.Account) error) {
	t.refreshOAuth = fn
}

func NewSyncTask(account *models.Account, database *db.DB) *SyncTask {
	return &SyncTask{account: account, database: database, priority: 1}
}

func (t *SyncTask) Type() task.Type { return task.TypeIMAP }
func (t *SyncTask) Priority() int   { return t.priority }
func (t *SyncTask) String() string {
	return fmt.Sprintf("IMAP sync for %s (account %d)", t.account.Email, t.account.ID)
}
func (t *SyncTask) accountLog(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if level == "error" {
		log.Printf("Sync [%s]: ERROR: %s", t.account.Email, msg)
	} else {
		log.Printf("Sync [%s]: %s", t.account.Email, msg)
	}
	if err := t.database.AddAccountLog(t.account.ID, level, msg); err != nil {
		log.Printf("Sync [%s]: failed to write log to DB: %v", t.account.Email, err)
	}
}

// Execute runs the synchronization and records the result in the DB
func (t *SyncTask) Execute(ctx context.Context) error {
	err := t.doExecute(ctx)
	if err != nil {
		if dbErr := t.database.SetAccountSyncError(t.account.ID, err.Error()); dbErr != nil {
			log.Printf("Failed to record sync error for %s: %v", t.account.Email, dbErr)
		}
	} else {
		if dbErr := t.database.ClearAccountSyncError(t.account.ID); dbErr != nil {
			log.Printf("Failed to clear sync error for %s: %v", t.account.Email, dbErr)
		}
	}
	return err
}

// doExecute performs the actual sync work
func (t *SyncTask) doExecute(ctx context.Context) error {
	t.accountLog("info", "starting sync")
	client := &Client{account: t.account}
	connectErr := client.Connect()
	// On OAuth auth failure, force-refresh token and retry once
	if connectErr != nil && t.account.IsOAuth() && t.refreshOAuth != nil && isAuthError(connectErr) {
		t.accountLog("info", "OAuth auth failed (%v), forcing token refresh and retrying", connectErr)
		if rerr := t.refreshOAuth(t.account); rerr != nil {
			t.accountLog("error", "failed to refresh OAuth token: %v", rerr)
			return fmt.Errorf("failed to connect: %w", connectErr)
		}
		client = &Client{account: t.account}
		connectErr = client.Connect()
	}
	if connectErr != nil {
		t.accountLog("error", "failed to connect: %v", connectErr)
		return fmt.Errorf("failed to connect: %w", connectErr)
	}
	defer client.Disconnect()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	localInbox, err := t.database.GetOrCreateLocalInbox(t.account.UserID)
	if err != nil {
		t.accountLog("error", "failed to get local inbox: %v", err)
		return fmt.Errorf("failed to get local inbox: %w", err)
	}
	if err := t.syncAllRemoteFolders(ctx, client, localInbox); err != nil {
		t.accountLog("error", "sync folders failed: %v", err)
	}
	if err := t.database.UpdateAccountLastSync(t.account.ID, timeutil.Now()); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}
	t.accountLog("info", "sync completed")
	return nil
}

// syncAllRemoteFolders is the unified pull path. It lists every
// mailbox the remote IMAP server exposes, filters out Trash and
// All-Mail-style duplicates, and syncs each one into our local inbox
// folder. Junk-class folders flow through saveMessageToInbox with
// treatAsSpam=true; everything else (INBOX, Sent, Drafts, Archive,
// user-named folders) goes through the regular inbox path.
//
// All synced messages share the same local folder (`localInbox`).
// remote_folder on each row records the upstream mailbox, so the
// flag-sync / delete-sync workers know where to push back.
//
// After every folder is synced, messages that vanished upstream are mirrored
// as vault deletes (reconcileVanished, sync_vanished.go).
func (t *SyncTask) syncAllRemoteFolders(ctx context.Context, client remoteAccount, localInbox *models.Folder) error {
	mailboxes, err := client.ListFolders()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}

	type folderJob struct {
		name  string
		class folderClass
	}
	var jobs []folderJob
	for _, mb := range mailboxes {
		c := classifyMailbox(mb)
		if c == folderSkip {
			continue
		}
		jobs = append(jobs, folderJob{name: mb.Name, class: c})
	}
	if len(jobs) == 0 {
		log.Printf("Sync [%s]: no syncable folders found", t.account.Email)
		return nil
	}

	totalNew, totalSkipped, totalSpam, totalBodies := 0, 0, 0, 0
	var fullPasses []string
	var presences []folderPresence
	synced := make([]string, 0, len(jobs))
	allComplete := true
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		synced = append(synced, j.name)
		res, err := t.syncOneFolder(ctx, client, localInbox, j.name, j.class)
		if err != nil {
			allComplete = false
		} else if res.presence.mode != presenceNone {
			presences = append(presences, res.presence)
		}
		// Partial results count even on error: whatever was saved is saved.
		totalNew += res.newCount
		totalSkipped += res.skipped
		totalSpam += res.spam
		totalBodies += res.bodies
		if res.fullReason != "" {
			fullPasses = append(fullPasses, fmt.Sprintf("%s (%s)", j.name, res.fullReason))
		}
		if err != nil {
			log.Printf("Sync [%s]: folder %q failed: %v", t.account.Email, j.name, err)
		}
	}
	var vs vanishStats
	if ctx.Err() == nil {
		vs = t.reconcileVanished(ctx, client, mailboxes, synced, presences, allComplete)
	}
	full := "none"
	if len(fullPasses) > 0 {
		full = strings.Join(fullPasses, ", ")
	}
	t.accountLog("info", "synced %d new messages (skipped %d duplicates, %d classified spam) across %d folders; "+
		"downloaded %d bodies, flags updated on %d messages; full pass: %s; "+
		"upstream deletions: %d removed, %d recognised as moved, %d held by mass-delete guard, "+
		"%d left alone (pending local change or local delete in flight), %d deferred",
		totalNew, totalSkipped, totalSpam, len(jobs), totalBodies, t.flagsChanged, full,
		vs.removed, vs.moved, vs.held, vs.pending, vs.deferred)
	// Push gate: t.lastNew is set by every non-spam save of this run, so its
	// presence is direct evidence the user got something worth announcing.
	// The old arithmetic gate (totalNew > totalSpam) silently swallowed the
	// notification whenever one batch carried both a clean message and at
	// least as much spam — one junk mail in the same sync cycle was enough
	// to make a fresh inbox message arrive with no push at all.
	if t.lastNew != nil && t.notifyFunc != nil {
		totalMessages, _ := t.database.GetMessageCountByFolder(localInbox.ID)
		if user, err := t.database.GetUserByID(t.account.UserID); err == nil {
			newCount := totalNew - totalSpam
			if newCount < 1 {
				newCount = 1
			}
			t.notifyFunc(NewMailNotice{
				Username:  user.Username,
				Mailbox:   "INBOX",
				Count:     totalMessages,
				NewCount:  newCount,
				From:      t.lastNew.From,
				Subject:   t.lastNew.Subject,
				MessageID: t.lastNew.ID,
			})
		}
	}
	// Flags pulled from the remote for already-known messages (read/starred
	// in another client of the SOURCE account). One push per run, and only
	// when something actually changed — RefreshExistingFromRemote's no-op
	// guard is the filter.
	if t.flagsChanged > 0 && t.flagsNotifyFunc != nil {
		t.accountLog("info", "flags refreshed on %d existing messages — pushing flags_changed", t.flagsChanged)
		t.flagsNotifyFunc(t.flagsChanged)
	}
	return nil
}

// folderClass classifies a remote IMAP mailbox for our sync purposes.
type folderClass int

const (
	folderSkip   folderClass = iota // Trash, All-Mail, Noselect containers
	folderInbox                     // INBOX + user-named (custom) folders
	folderJunk                      // Spam / Junk
	folderSent                      // Sent / Отправленные — outgoing
	folderDrafts                    // Drafts / Черновики
)

// SyncableMailbox reports whether the sync pulls this mailbox at all — the
// same rule as syncAllRemoteFolders, for tools that must walk exactly the
// folders the sync walks (cmd/msgid-audit).
func SyncableMailbox(mb *imap.MailboxInfo) bool { return classifyMailbox(mb) != folderSkip }

// classifyMailbox decides whether and how to sync a given mailbox.
// Trash-class folders and Gmail-style "All Mail" duplicates are
// dropped. Spam-class folders flow through the spam branch in
// saveMessageToInbox so the user's whitelist can still rescue. Every
// other selectable mailbox is treated as inbox content — pulling Sent,
// Drafts, Archive, etc. is part of the "give me everything that isn't
// in the trash" mandate.
func classifyMailbox(mb *imap.MailboxInfo) folderClass {
	if mb == nil || mb.Name == "" {
		return folderSkip
	}
	for _, a := range mb.Attributes {
		switch {
		case strings.EqualFold(a, "\\Noselect"):
			return folderSkip
		case strings.EqualFold(a, "\\Trash"):
			return folderSkip
		case strings.EqualFold(a, "\\All"):
			// Gmail's "All Mail" is a virtual union of every other
			// folder — pulling it on top of the rest just doubles
			// the work and creates duplicate dedup hits.
			return folderSkip
		case strings.EqualFold(a, "\\Junk"):
			return folderJunk
		case strings.EqualFold(a, "\\Sent"):
			return folderSent
		case strings.EqualFold(a, "\\Drafts"):
			return folderDrafts
		}
	}
	lower := strings.ToLower(mb.Name)
	if isTrashName(lower) {
		return folderSkip
	}
	if isAllMailName(lower) {
		return folderSkip
	}
	if isJunkName(lower) {
		return folderJunk
	}
	if isSentName(lower) {
		return folderSent
	}
	if isDraftsName(lower) {
		return folderDrafts
	}
	return folderInbox
}

func isSentName(lower string) bool {
	switch lower {
	case "sent", "sent items", "sent messages", "отправленные":
		return true
	}
	if strings.HasSuffix(lower, "/sent") || strings.HasSuffix(lower, "/sent items") {
		return true
	}
	if strings.Contains(lower, "отправленн") {
		return true
	}
	return false
}

func isDraftsName(lower string) bool {
	switch lower {
	case "drafts", "draft", "черновики":
		return true
	}
	if strings.HasSuffix(lower, "/drafts") {
		return true
	}
	if strings.Contains(lower, "черновик") {
		return true
	}
	return false
}

func isTrashName(lower string) bool {
	switch lower {
	case "trash", "deleted messages", "deleted items", "корзина",
		"удаленные элементы", "удалённые элементы":
		return true
	}
	if strings.HasSuffix(lower, "/trash") || strings.HasSuffix(lower, "/корзина") {
		return true
	}
	return false
}

func isAllMailName(lower string) bool {
	return lower == "[gmail]/all mail" || strings.HasSuffix(lower, "/all mail")
}

func isJunkName(lower string) bool {
	switch lower {
	case "spam", "junk", "junk e-mail", "junk email", "bulk mail":
		return true
	}
	if strings.HasSuffix(lower, "/spam") || strings.HasSuffix(lower, "/junk") {
		return true
	}
	if strings.Contains(lower, "нежелательная") || strings.Contains(lower, "спам") {
		return true
	}
	return false
}

// saveMessageToInbox persists a fetched IMAP message into our local
// inbox folder, returning `(saved, isSpam, err)`.
//
//   - remoteFolderName names the upstream IMAP mailbox the message came
//     from (e.g. "INBOX" or "[Gmail]/Spam"). Stored on the row so
//     flag-sync / delete-sync know where to push back.
//   - class drives spam-pipeline semantics:
//   - folderInbox: full pipeline (whitelist → analyzer →
//     recipient-mismatch check).
//   - folderJunk: skip analyzer + recipient check, default
//     is_spam=true; whitelist still rescues.
//   - folderSent / folderDrafts: bypass the spam pipeline entirely.
//     These are the user's own outgoing / draft content; running
//     them through the recipient-mismatch check would (correctly)
//     trip — Sent has the user as the FROM, not the TO — and
//     misclassify them as spam.
func (t *SyncTask) saveMessageToInbox(imapMsg *imap.Message, inbox *models.Folder, remoteFolderName string, class folderClass) (bool, bool, error) {
	treatAsSpam := class == folderJunk
	bypassSpam := class == folderSent || class == folderDrafts
	if imapMsg.Envelope == nil {
		log.Printf("IMAP sync: Skipping message UID %d - no envelope data", imapMsg.Uid)
		return false, false, nil
	}
	if len(imapMsg.Envelope.From) == 0 && imapMsg.Envelope.Subject == "" {
		log.Printf("IMAP sync: Skipping message UID %d - empty envelope", imapMsg.Uid)
		return false, false, nil
	}
	if hasFlag(imapMsg.Flags, imap.DeletedFlag) {
		return false, false, nil
	}
	// A message's identity is (user_id, Message-ID). Upstream senders that
	// omit the header (tutu, ELMA, …) can't be 5xx'd from here, and dropping
	// the message loses real mail — so the id is derived deterministically
	// from the content instead (see parser.DeriveMessageID).
	var rawData []byte
	messageID := imapMsg.Envelope.MessageId
	if messageID == "" {
		rawData = firstLiteral(imapMsg)
		messageID = parser.DeriveMessageID(rawData)
		if messageID == "" {
			log.Printf("IMAP sync: skip message (uid %d, subj %q) — no Message-ID and no body to derive one",
				imapMsg.Uid, imapMsg.Envelope.Subject)
			return false, false, nil
		}
	}
	exists, err := t.database.MessageExistsByMessageID(t.account.UserID, messageID)
	if err != nil {
		return false, false, err
	}
	if exists {
		fromAddr := parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.From))
		return false, t.refreshKnownMessage(messageID, fromAddr, imapMsg.Uid, imapMsg.Flags, remoteFolderName, class), nil
	}
	var body, bodyHTML string
	var attachments []parser.ParsedAttachment
	var parsed *parser.ParsedMessage
	if rawData == nil {
		rawData = firstLiteral(imapMsg)
	}
	if rawData != nil {
		var parseErr error
		parsed, parseErr = parser.New().ParseBytes(rawData)
		if parseErr == nil {
			body = parsed.Body
			bodyHTML = parsed.BodyHTML
			attachments = parsed.Attachments
		} else {
			log.Printf("IMAP sync: Failed to parse message body: %v", parseErr)
		}
	}
	localUID, err := t.database.GetNextUIDForFolder(inbox.ID)
	if err != nil {
		return false, false, fmt.Errorf("failed to get next UID: %w", err)
	}
	msgDateMs := timeutil.ToMs(imapMsg.Envelope.Date.UTC())
	if msgDateMs <= 0 {
		msgDateMs = timeutil.Now()
	}
	fromAddr := parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.From))
	isSpam := treatAsSpam // default: trust the remote's spam-folder verdict
	var spamScore float64
	var spamStatus, spamReasons string
	var spamRuleID *int64
	action, matchedRule, ruleErr := t.database.CheckSpamRules(t.account.UserID, fromAddr)
	if ruleErr != nil {
		log.Printf("IMAP sync: Failed to check spam rules: %v", ruleErr)
	}
	// Sent / Drafts bypass: the user's own outgoing content has no
	// business being put through the spam analyzer (and would trip
	// recipient-mismatch — the user is in From, not To/Cc). Store
	// it cleanly with is_spam=false and move on.
	if bypassSpam {
		goto buildMessage
	}

	// Remote spam-folder path: short-circuit our analyzer + recipient
	// check (the upstream already classified, we just record the
	// verdict). A whitelist rule still rescues — that's the entire
	// point of pulling Spam: catch upstream false positives.
	if treatAsSpam {
		if action == "allow" {
			isSpam = false
			log.Printf("IMAP sync: Remote-spam message whitelisted by rule %d for user %d", matchedRule.ID, t.account.UserID)
		} else {
			spamStatus = string(parser.SpamStatusSpam)
			spamReasons = parser.GetSpamReasonsJSON([]string{
				fmt.Sprintf("classified spam by upstream (%s)", remoteFolderName),
			})
		}
		// Skip the analyzer + recipient check — fall through to the
		// row-build below, leaving everything else at zero / default.
		goto buildMessage
	}
	// Whitelist rule with NO per-check exclusions = full bypass.
	if action == "allow" && len(matchedRule.ExcludedChecks) == 0 {
		isSpam = false
		log.Printf("IMAP sync: Message whitelisted by rule %d for user %d", matchedRule.ID, t.account.UserID)
	} else if action == "spam" {
		isSpam = true
		spamRuleID = &matchedRule.ID
		log.Printf("IMAP sync: Message blacklisted by rule %d for user %d", matchedRule.ID, t.account.UserID)
	} else if t.analyzer != nil && parsed != nil {
		disabledChecks, err := t.database.GetDisabledSpamChecksMap(t.account.UserID)
		if err != nil {
			log.Printf("IMAP sync: Failed to get disabled spam checks: %v", err)
			disabledChecks = nil
		}
		// Partial-allow rule: merge its excluded checks into disabledChecks
		// so the analyzer still runs the rest. We also record the rule id so
		// the UI can trace the score-reduction back to the rule.
		if action == "allow" && len(matchedRule.ExcludedChecks) > 0 {
			if disabledChecks == nil {
				disabledChecks = map[string]bool{}
			}
			for _, c := range matchedRule.ExcludedChecks {
				disabledChecks[c] = true
			}
			spamRuleID = &matchedRule.ID
		}
		weights, wErr := t.database.GetSpamCheckWeights(t.account.UserID)
		if wErr != nil {
			log.Printf("IMAP sync: Failed to get spam weights: %v", wErr)
			weights = nil
		}
		if len(rawData) > 0 {
			p := parser.New()
			parsed, _ = p.ParseBytes(rawData)
		}
		t.analyzer.AnalyzeWithUserConfig(parsed, "", "", disabledChecks, weights)
		spamScore = parsed.SpamScore
		spamStatus = string(parsed.SpamStatus)
		spamReasons = parser.GetSpamReasonsJSON(parsed.SpamReasons)
		// Partial-allow rules never mark spam — that's the point of the
		// exception: trust this sender even if a subset of checks fired.
		if parsed.SpamStatus == parser.SpamStatusSpam && action != "allow" {
			isSpam = true
			log.Printf("IMAP sync: Message marked as spam (score=%.1f, reasons=%v) for user %d", parsed.SpamScore, parsed.SpamReasons, t.account.UserID)
		}
	}

	// Recipient validation: if To/Cc don't include the account's email or any alias,
	// mark as spam (unless whitelisted by a user rule).
	if action != "allow" && !recipientIncludesAccount(t.account, imapMsg.Envelope.To, imapMsg.Envelope.Cc) {
		isSpam = true
		log.Printf("IMAP sync: Message marked as spam — recipient mismatch (account=%s, To=%s, Cc=%s)",
			t.account.Email,
			formatAddressList(imapMsg.Envelope.To),
			formatAddressList(imapMsg.Envelope.Cc))
		// Add a reason if there isn't already a spam_reasons string
		mismatchReason := "recipient mismatch: account address not in To/Cc"
		if spamReasons == "" {
			spamReasons = parser.GetSpamReasonsJSON([]string{mismatchReason})
		} else {
			// Try to merge into existing JSON array; if parsing fails just keep both
			var existing []string
			if err := json.Unmarshal([]byte(spamReasons), &existing); err == nil {
				existing = append(existing, mismatchReason)
				spamReasons = parser.GetSpamReasonsJSON(existing)
			}
		}
		if spamStatus == "" {
			spamStatus = string(parser.SpamStatusSpam)
		}
	}

buildMessage:
	msg := &models.Message{
		AccountID: t.account.ID, UserID: t.account.UserID, FolderID: inbox.ID, MessageID: messageID,
		Subject: parser.DecodeMIMEHeader(imapMsg.Envelope.Subject),
		From:    parser.DecodeMIMEHeader(fromAddr),
		To:      parser.DecodeMIMEHeader(formatAddressList(imapMsg.Envelope.To)),
		Cc:      parser.DecodeMIMEHeader(formatAddressList(imapMsg.Envelope.Cc)),
		Bcc:     parser.DecodeMIMEHeader(formatAddressList(imapMsg.Envelope.Bcc)),
		ReplyTo: parser.DecodeMIMEHeader(formatAddressList(imapMsg.Envelope.ReplyTo)),
		Date:    msgDateMs, Body: parser.SanitizeUTF8(body), BodyHTML: parser.SanitizeUTF8(bodyHTML),
		RawEmail: rawData,
		UID:      localUID, Seen: hasFlag(imapMsg.Flags, imap.SeenFlag),
		Flagged:   hasFlag(imapMsg.Flags, imap.FlaggedFlag),
		Answered:  hasFlag(imapMsg.Flags, imap.AnsweredFlag),
		Draft:     class == folderDrafts || hasFlag(imapMsg.Flags, imap.DraftFlag),
		Deleted:   hasFlag(imapMsg.Flags, imap.DeletedFlag),
		InReplyTo: parser.SanitizeUTF8(imapMsg.Envelope.InReplyTo),
		RemoteUID: imapMsg.Uid, RemoteFolder: remoteFolderName,
		SpamScore: spamScore, SpamStatus: spamStatus, SpamReasons: spamReasons,
		IsSpam: isSpam, SpamRuleID: spamRuleID,
	}
	if err := t.database.CreateMessage(msg); err != nil {
		if errors.Is(err, db.ErrDuplicateMessage) {
			// Another sync/MX already stored this (user_id, Message-ID). The
			// DB constraint won the race; treat as a benign skip.
			return false, false, nil
		}
		return false, false, err
	}
	attachmentCount := 0
	for _, att := range attachments {
		attachment := &models.Attachment{
			MessageID: msg.ID, ContentID: att.ContentID, Filename: att.Filename,
			ContentType: att.ContentType, Size: int(att.Size), IsInline: att.IsInline, Data: att.Data,
		}
		if err := t.database.CreateAttachment(attachment); err != nil {
			log.Printf("IMAP sync: Failed to save attachment %s: %v", att.Filename, err)
		} else {
			attachmentCount++
		}
	}
	if attachmentCount > 0 {
		msg.Attachments = attachmentCount
		t.database.UpdateMessageAttachmentCount(msg.ID, attachmentCount)
	}

	// iTIP: if this message is purely a calendar invite (REQUEST/CANCEL/REPLY/
	// COUNTER), route it into the calendar pipeline and hard-delete the row.
	// We use the account's primary email as the "recipient identity" — the
	// per-folder sync doesn't track which alias the original recipient used,
	// and external Sent/Drafts pulls would never arrive at us anyway.
	if parsed != nil && t.account.Email != "" {
		identities := append([]string{t.account.Email}, t.account.GetAliases()...)
		handler := calendar.NewIncomingHandler(t.database)
		if processed, perr := handler.ProcessAndDispatch(parsed, t.account.UserID, t.account.ID, identities); perr != nil {
			log.Printf("IMAP sync: ICS dispatch on msg %d: %v", msg.ID, perr)
		} else if processed {
			// SOFT delete, not hard: the remote copy stays on the source
			// server, so a hard-deleted row is "new" again on the very next
			// sync — the same invite got re-fetched, re-dispatched and
			// re-announced every cycle, forever (the "ancient event toast
			// every 6 minutes" bug). A soft-deleted row keeps its
			// (user_id, message_id) for dedup and stays invisible.
			if delErr := t.database.SoftDeleteMessage(msg.ID); delErr != nil {
				log.Printf("IMAP sync: failed to drop iTIP-consumed msg %d: %v", msg.ID, delErr)
			} else {
				log.Printf("IMAP sync: msg %d consumed by iTIP handler, hidden", msg.ID)
				return false, isSpam, nil
			}
		}
	}

	if !isSpam {
		// Toast content for the post-sync notification (last new wins).
		// Assigned only AFTER the iTIP branch: a consumed invite never
		// surfaces as a message, so it must not become the toast either —
		// that's the other half of the ancient-toast bug.
		t.lastNew = msg
	}
	return true, isSpam, nil
}

// isAuthError returns true if the error looks like an OAuth authentication failure
// that may be fixed by refreshing the token.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "auth") ||
		strings.Contains(msg, "invalid_request") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "unauthorized")
}

// recipientIncludesAccount returns true if any address in the To/Cc lists
// matches the account's email or one of its aliases.
func recipientIncludesAccount(account *models.Account, to, cc []*imap.Address) bool {
	check := func(list []*imap.Address) bool {
		for _, a := range list {
			if a == nil {
				continue
			}
			addr := fmt.Sprintf("%s@%s", a.MailboxName, a.HostName)
			if account.IsKnownRecipient(addr) {
				return true
			}
		}
		return false
	}
	return check(to) || check(cc)
}

func formatAddressList(addresses []*imap.Address) string {
	if len(addresses) == 0 {
		return ""
	}
	result := ""
	for i, addr := range addresses {
		if i > 0 {
			result += ", "
		}
		if addr.PersonalName != "" {
			result += addr.PersonalName + " "
		}
		result += fmt.Sprintf("<%s@%s>", addr.MailboxName, addr.HostName)
	}
	return result
}

func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// firstLiteral reads the fetched RFC822 body section. A literal is a one-shot
// reader, so callers keep the returned bytes. Returns nil when the fetch
// carried no body section.
func firstLiteral(imapMsg *imap.Message) []byte {
	for _, literal := range imapMsg.Body {
		if literal == nil {
			return nil
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(literal); err != nil {
			log.Printf("IMAP sync: read body of uid %d: %v", imapMsg.Uid, err)
		}
		return buf.Bytes()
	}
	return nil
}
