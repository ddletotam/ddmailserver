package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/yourusername/mailserver/internal/calendar"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/task"
	"github.com/yourusername/mailserver/internal/timeutil"
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
	// Called to force-refresh the OAuth token when auth fails. The callback
	// is expected to update the account in place (access token, expiry).
	refreshOAuth func(account *models.Account) error
}

func (t *SyncTask) SetNotifyFunc(fn func(NewMailNotice)) { t.notifyFunc = fn }
func (t *SyncTask) SetAnalyzer(analyzer *parser.Analyzer)                         { t.analyzer = analyzer }
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
func (t *SyncTask) syncAllRemoteFolders(ctx context.Context, client *Client, localInbox *models.Folder) error {
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

	totalNew, totalSkipped, totalSpam := 0, 0, 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		newN, skipN, spamN, err := t.syncOneFolder(ctx, client, localInbox, j.name, j.class)
		if err != nil {
			log.Printf("Sync [%s]: folder %q failed: %v", t.account.Email, j.name, err)
			continue
		}
		totalNew += newN
		totalSkipped += skipN
		totalSpam += spamN
	}
	t.accountLog("info", "synced %d new messages (skipped %d duplicates, %d classified spam) across %d folders",
		totalNew, totalSkipped, totalSpam, len(jobs))
	if totalNew > totalSpam && t.notifyFunc != nil {
		totalMessages, _ := t.database.GetMessageCountByFolder(localInbox.ID)
		if user, err := t.database.GetUserByID(t.account.UserID); err == nil {
			notice := NewMailNotice{
				Username: user.Username,
				Mailbox:  "INBOX",
				Count:    totalMessages,
				NewCount: totalNew - totalSpam,
			}
			if t.lastNew != nil {
				notice.From = t.lastNew.From
				notice.Subject = t.lastNew.Subject
				notice.MessageID = t.lastNew.ID
			}
			t.notifyFunc(notice)
		}
	}
	return nil
}

// syncOneFolder pulls all messages from a single remote mailbox and
// dispatches them through saveMessageToInbox with the appropriate
// folder role. Returns (newCount, skippedCount, spamCount).
func (t *SyncTask) syncOneFolder(ctx context.Context, client *Client, localInbox *models.Folder, name string, class folderClass) (int, int, int, error) {
	mbox, err := client.SelectFolder(name)
	if err != nil {
		return 0, 0, 0, err
	}
	if mbox.Messages == 0 {
		return 0, 0, 0, nil
	}
	uidSet := new(imap.SeqSet)
	uidSet.AddRange(1, 0)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem()}
	messages, fetchDone := client.FetchMessagesByUID(uidSet, items)
	newCount, skippedCount, spamCount := 0, 0, 0
	for msg := range messages {
		if ctx.Err() != nil {
			return newCount, skippedCount, spamCount, ctx.Err()
		}
		saved, isSpam, err := t.saveMessageToInbox(msg, localInbox, name, class)
		if err != nil {
			log.Printf("Sync [%s] %s: save failed: %v", t.account.Email, name, err)
			continue
		}
		if saved {
			newCount++
			if isSpam {
				spamCount++
			}
		} else {
			skippedCount++
		}
	}
	if err := <-fetchDone; err != nil {
		return newCount, skippedCount, spamCount, fmt.Errorf("IMAP fetch on %q failed: %w", name, err)
	}
	return newCount, skippedCount, spamCount, nil
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
	messageID := imapMsg.Envelope.MessageId
	if messageID == "" {
		h := sha256.New()
		h.Write([]byte(imapMsg.Envelope.Subject))
		h.Write([]byte(formatAddressList(imapMsg.Envelope.From)))
		h.Write([]byte(imapMsg.Envelope.Date.Format(time.RFC3339)))
		h.Write([]byte(fmt.Sprintf("%d", imapMsg.Uid)))
		messageID = fmt.Sprintf("<%s@generated.local>", hex.EncodeToString(h.Sum(nil))[:32])
	}
	exists, err := t.database.MessageExistsByMessageID(t.account.UserID, messageID)
	if err != nil {
		return false, false, err
	}
	if exists {
		// Inbox path just refreshes the remote pointer if we never
		// recorded one. Spam path is more interesting: the upstream
		// provider has just (re-)classified an already-known message
		// as junk. Flip is_spam on the local row, unless the user has
		// a whitelist rule for that sender — that's the rescue path.
		if treatAsSpam {
			fromAddr := parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.From))
			action, matchedRule, ruleErr := t.database.CheckSpamRules(t.account.UserID, fromAddr)
			if ruleErr != nil {
				log.Printf("IMAP sync: check spam rules during reclassify: %v", ruleErr)
			}
			rescue := action == "allow"
			if rescue {
				log.Printf("IMAP sync: remote-spam %s rescued by whitelist rule %d", messageID, matchedRule.ID)
			} else {
				log.Printf("IMAP sync: remote-spam %s reclassified as spam (folder=%s)", messageID, remoteFolderName)
			}
			if err := t.database.ReclassifyMessageFromRemoteSpam(
				t.account.UserID, messageID, imapMsg.Uid, remoteFolderName, !rescue,
			); err != nil {
				log.Printf("IMAP sync: reclassify failed for %s: %v", messageID, err)
			}
			return false, !rescue, nil
		}
		// Inbox path on an already-known row: sync flags from the
		// remote side AND decide is_spam based on the user's current
		// spam rules. Earlier this branch unconditionally downgraded
		// is_spam=false (to handle "user moved out of upstream Junk
		// back into INBOX") — which silently killed blacklisting:
		// the next sync after a Spam-button click would resurrect
		// every previously-blocked message because it sat in upstream
		// INBOX. We now re-evaluate the rule on every dedup hit so
		// blacklist verdicts stick across re-syncs and whitelist /
		// no-rule cases still rescue.
		fromAddrCheck := parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.From))
		ruleAction, ruleMatched, ruleErr := t.database.CheckSpamRules(t.account.UserID, fromAddrCheck)
		if ruleErr != nil {
			log.Printf("IMAP sync: check spam rules on existing %s: %v", messageID, ruleErr)
		}
		downgrade := ruleAction != "spam" // spam-rule keeps is_spam=true; allow / no-rule lets remote INBOX rescue
		if err := t.database.RefreshExistingFromRemote(
			t.account.UserID, messageID, imapMsg.Uid, remoteFolderName,
			hasFlag(imapMsg.Flags, imap.SeenFlag),
			hasFlag(imapMsg.Flags, imap.FlaggedFlag),
			hasFlag(imapMsg.Flags, imap.AnsweredFlag),
			downgrade,
		); err != nil {
			log.Printf("IMAP sync: refresh existing %s failed: %v", messageID, err)
		}
		if !downgrade && ruleMatched != nil {
			// Make sure the row is actually flagged spam and that
			// spam_rule_id points at the rule. Cheap UPDATE; if the
			// row is already in this state, RowsAffected is 0.
			if err := t.database.ReclassifyMessageFromRemoteSpam(
				t.account.UserID, messageID, imapMsg.Uid, remoteFolderName, true,
			); err != nil {
				log.Printf("IMAP sync: re-flag spam on existing %s: %v", messageID, err)
			}
		}
		return false, ruleAction == "spam", nil
	}
	var body, bodyHTML string
	var attachments []parser.ParsedAttachment
	var parsed *parser.ParsedMessage
	var rawData []byte
	var rfc822Body io.Reader
	for _, literal := range imapMsg.Body {
		rfc822Body = literal
		break
	}
	if rfc822Body != nil {
		var buf bytes.Buffer
		teeReader := io.TeeReader(rfc822Body, &buf)
		p := parser.New()
		var parseErr error
		parsed, parseErr = p.Parse(teeReader)
		rawData = buf.Bytes()
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
		return false, false, err
	}
	if !isSpam {
		// Toast content for the post-sync notification (last new wins).
		t.lastNew = msg
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
			if delErr := t.database.HardDeleteMessage(msg.ID); delErr != nil {
				log.Printf("IMAP sync: failed to drop iTIP-consumed msg %d: %v", msg.ID, delErr)
			} else {
				log.Printf("IMAP sync: msg %d consumed by iTIP handler, deleted", msg.ID)
				return false, isSpam, nil
			}
		}
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
