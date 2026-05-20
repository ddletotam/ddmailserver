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
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/task"
	"github.com/yourusername/mailserver/internal/timeutil"
)

type SyncTask struct {
	account    *models.Account
	database   *db.DB
	analyzer   *parser.Analyzer
	notifyFunc func(username, mailbox string, count uint32)
	priority   int
	// Called to force-refresh the OAuth token when auth fails. The callback
	// is expected to update the account in place (access token, expiry).
	refreshOAuth func(account *models.Account) error
}

func (t *SyncTask) SetNotifyFunc(fn func(username, mailbox string, count uint32)) { t.notifyFunc = fn }
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
	if err := t.syncRemoteInbox(ctx, client, localInbox); err != nil {
		t.accountLog("error", "sync INBOX failed: %v", err)
	}
	if err := t.syncRemoteSpamFolders(ctx, client, localInbox); err != nil {
		t.accountLog("error", "sync spam folders failed: %v", err)
	}
	if err := t.database.UpdateAccountLastSync(t.account.ID, timeutil.Now()); err != nil {
		log.Printf("Failed to update last sync time: %v", err)
	}
	t.accountLog("info", "sync completed")
	return nil
}

func (t *SyncTask) syncRemoteInbox(ctx context.Context, client *Client, localInbox *models.Folder) error {
	log.Printf("Syncing remote INBOX for %s to local inbox (folder %d)", t.account.Email, localInbox.ID)
	mbox, err := client.SelectFolder("INBOX")
	if err != nil {
		return err
	}
	if mbox.Messages == 0 {
		log.Printf("Remote INBOX is empty for %s", t.account.Email)
		return nil
	}
	uidSet := new(imap.SeqSet)
	uidSet.AddRange(1, 0)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem()}
	messages, fetchDone := client.FetchMessagesByUID(uidSet, items)
	messageCount, skippedCount, spamCount := 0, 0, 0
	for msg := range messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		saved, isSpam, err := t.saveMessageToInbox(msg, localInbox, "INBOX", false)
		if err != nil {
			log.Printf("Failed to save message: %v", err)
			continue
		}
		if saved {
			messageCount++
			if isSpam {
				spamCount++
			}
		} else {
			skippedCount++
		}
	}
	if err := <-fetchDone; err != nil {
		return fmt.Errorf("IMAP fetch failed: %w", err)
	}
	t.accountLog("info", "synced %d new messages (skipped %d duplicates, %d spam)", messageCount, skippedCount, spamCount)
	if messageCount > spamCount && t.notifyFunc != nil {
		totalMessages, _ := t.database.GetMessageCountByFolder(localInbox.ID)
		if user, err := t.database.GetUserByID(t.account.UserID); err == nil {
			t.notifyFunc(user.Username, "INBOX", totalMessages)
		}
	}
	return nil
}

// saveMessageToInbox persists a fetched IMAP message into our local
// inbox folder, returning `(saved, isSpam, err)`.
//
//   - remoteFolderName names the upstream IMAP mailbox the message came
//     from (e.g. "INBOX" or "[Gmail]/Spam"). Stored on the row so
//     flag-sync / delete-sync know where to push back.
//   - treatAsSpam=true is the path for remote spam/junk folders: the
//     upstream provider has already classified the message as spam, so
//     we default isSpam=true and skip our own analyzer. A user whitelist
//     rule (`action="allow"`) still rescues the message — that's the
//     "false positive on Gmail's side" escape hatch.
func (t *SyncTask) saveMessageToInbox(imapMsg *imap.Message, inbox *models.Folder, remoteFolderName string, treatAsSpam bool) (bool, bool, error) {
	if imapMsg.Envelope == nil {
		log.Printf("IMAP sync: Skipping message UID %d - no envelope data", imapMsg.Uid)
		return false, false, nil
	}
	if len(imapMsg.Envelope.From) == 0 && imapMsg.Envelope.Subject == "" {
		log.Printf("IMAP sync: Skipping message UID %d - empty envelope", imapMsg.Uid)
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
		if imapMsg.Uid > 0 {
			t.database.UpdateMessageRemoteUID(t.account.UserID, messageID, imapMsg.Uid, remoteFolderName)
		}
		return false, false, nil
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
		Subject: parser.SanitizeUTF8(imapMsg.Envelope.Subject), From: fromAddr,
		To:      parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.To)),
		Cc:      parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.Cc)),
		Bcc:     parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.Bcc)),
		ReplyTo: parser.SanitizeUTF8(formatAddressList(imapMsg.Envelope.ReplyTo)),
		Date:    msgDateMs, Body: parser.SanitizeUTF8(body), BodyHTML: parser.SanitizeUTF8(bodyHTML),
		RawEmail: rawData,
		UID:      localUID, Seen: hasFlag(imapMsg.Flags, imap.SeenFlag),
		Flagged:   hasFlag(imapMsg.Flags, imap.FlaggedFlag),
		Answered:  hasFlag(imapMsg.Flags, imap.AnsweredFlag),
		Draft:     hasFlag(imapMsg.Flags, imap.DraftFlag),
		Deleted:   hasFlag(imapMsg.Flags, imap.DeletedFlag),
		InReplyTo: parser.SanitizeUTF8(imapMsg.Envelope.InReplyTo),
		RemoteUID: imapMsg.Uid, RemoteFolder: remoteFolderName,
		SpamScore: spamScore, SpamStatus: spamStatus, SpamReasons: spamReasons,
		IsSpam: isSpam, SpamRuleID: spamRuleID,
	}
	if err := t.database.CreateMessage(msg); err != nil {
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
	return true, isSpam, nil
}

// syncRemoteSpamFolders pulls messages from upstream Spam/Junk
// folders into our local inbox, tagged `is_spam=true` by default. A
// user whitelist rule (`action="allow"` for that sender or domain) can
// still flip it to non-spam — that's the entire reason we bother
// pulling: catch upstream false positives where the remote provider
// classified something as spam that the user actually wants.
//
// Folder discovery: prefer the IMAP SPECIAL-USE `\Junk` attribute
// (modern servers — Gmail, Yandex, Fastmail, Dovecot — all advertise
// it). Fall back to common folder names for older servers / providers
// that don't expose SPECIAL-USE.
func (t *SyncTask) syncRemoteSpamFolders(ctx context.Context, client *Client, localInbox *models.Folder) error {
	mailboxes, err := client.ListFolders()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}

	var spamFolders []string
	for _, mb := range mailboxes {
		if mb == nil || mb.Name == "" {
			continue
		}
		if isJunkMailbox(mb) {
			spamFolders = append(spamFolders, mb.Name)
		}
	}
	if len(spamFolders) == 0 {
		return nil
	}
	log.Printf("Sync [%s]: discovered %d spam folder(s): %v", t.account.Email, len(spamFolders), spamFolders)

	totalSaved, totalSkipped, totalRescued := 0, 0, 0
	for _, name := range spamFolders {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mbox, err := client.SelectFolder(name)
		if err != nil {
			log.Printf("Sync [%s]: failed to select %q: %v", t.account.Email, name, err)
			continue
		}
		if mbox.Messages == 0 {
			continue
		}
		uidSet := new(imap.SeqSet)
		uidSet.AddRange(1, 0)
		section := &imap.BodySectionName{Peek: true}
		items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem()}
		messages, fetchDone := client.FetchMessagesByUID(uidSet, items)
		for msg := range messages {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			saved, isSpam, err := t.saveMessageToInbox(msg, localInbox, name, true)
			if err != nil {
				log.Printf("Sync [%s]: failed to save spam msg: %v", t.account.Email, err)
				continue
			}
			if saved {
				totalSaved++
				if !isSpam {
					// Whitelist rescue — we pulled from remote spam but
					// our rule said this sender is fine.
					totalRescued++
				}
			} else {
				totalSkipped++
			}
		}
		if err := <-fetchDone; err != nil {
			log.Printf("Sync [%s]: spam fetch on %q failed: %v", t.account.Email, name, err)
		}
	}
	if totalSaved > 0 || totalSkipped > 0 {
		t.accountLog("info", "spam folders: saved %d (rescued %d), skipped %d duplicates", totalSaved, totalRescued, totalSkipped)
	}
	return nil
}

// isJunkMailbox returns true for mailboxes that the user's upstream
// provider treats as the spam/junk bin — either via the standardised
// IMAP SPECIAL-USE `\Junk` attribute (RFC 6154) or a name match against
// the de-facto conventions of major providers.
func isJunkMailbox(mb *imap.MailboxInfo) bool {
	for _, a := range mb.Attributes {
		if strings.EqualFold(a, "\\Junk") {
			return true
		}
	}
	lower := strings.ToLower(mb.Name)
	switch lower {
	case "spam", "junk", "junk e-mail", "junk email", "bulk mail":
		return true
	}
	// Path-style folder names that Gmail / Outlook hand out, plus the
	// Russian-language defaults of Yandex / Mail.ru.
	if strings.HasSuffix(lower, "/spam") || strings.HasSuffix(lower, "/junk") {
		return true
	}
	if strings.Contains(lower, "нежелательная") || strings.Contains(lower, "спам") {
		return true
	}
	return false
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
