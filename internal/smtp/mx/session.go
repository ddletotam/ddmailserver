package mx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/mail"
	"strings"

	"github.com/emersion/go-smtp"
	"github.com/yourusername/mailserver/internal/calendar"
	"github.com/yourusername/mailserver/internal/db"
	"github.com/yourusername/mailserver/internal/models"
	"github.com/yourusername/mailserver/internal/notify"
	"github.com/yourusername/mailserver/internal/parser"
	"github.com/yourusername/mailserver/internal/timeutil"
)

// Recipient holds info about a validated recipient
type Recipient struct {
	Email     string
	Mailbox   *models.Mailbox
	Domain    *models.Domain
	LocalPart string
}

// Session represents an MX SMTP session
type Session struct {
	database            *db.DB
	hub                 *notify.Hub
	conn                *smtp.Conn
	analyzer            *parser.Analyzer
	calendarSyncTrigger func(userID int64)
	from                string
	fromDomain          string
	senderIP            string
	recipients          []*Recipient
}

// AuthPlain - MX server does not require authentication
func (s *Session) AuthPlain(username, password string) error {
	// MX server accepts mail from anyone, no auth needed
	return nil
}

// Mail is called to set the sender (MAIL FROM)
func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	log.Printf("MX: MAIL FROM: %s", from)
	s.from = from

	// Extract domain for SPF check
	email := s.extractEmail(from)
	_, domain := s.splitEmail(email)
	s.fromDomain = domain

	return nil
}

// Rcpt is called to set a recipient (RCPT TO)
// This is where we validate if we accept mail for this address
func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	log.Printf("MX: RCPT TO: %s", to)

	// Extract email address
	email := s.extractEmail(to)
	if email == "" {
		return errors.New("550 Invalid recipient address")
	}

	// Split into local part and domain
	localPart, domainName := s.splitEmail(email)
	if localPart == "" || domainName == "" {
		return errors.New("550 Invalid recipient address format")
	}

	// Check if we handle this domain
	domain, err := s.database.GetDomainByName(domainName)
	if err != nil {
		log.Printf("MX: Domain %s not found: %v", domainName, err)
		return errors.New("550 Relay access denied - domain not handled")
	}

	if !domain.Enabled {
		log.Printf("MX: Domain %s is disabled", domainName)
		return errors.New("550 Relay access denied - domain disabled")
	}

	// Check if mailbox exists
	mailbox, err := s.database.GetMailbox(domain.ID, localPart)
	if err != nil {
		log.Printf("MX: Mailbox %s@%s not found: %v", localPart, domainName, err)
		return errors.New("550 No such user here")
	}

	if !mailbox.Enabled {
		log.Printf("MX: Mailbox %s@%s is disabled", localPart, domainName)
		return errors.New("550 Mailbox disabled")
	}

	// Add to recipients list
	s.recipients = append(s.recipients, &Recipient{
		Email:     email,
		Mailbox:   mailbox,
		Domain:    domain,
		LocalPart: localPart,
	})

	log.Printf("MX: Accepted recipient %s (user_id=%d)", email, mailbox.UserID)
	return nil
}

// Data is called when the client sends the message body
func (s *Session) Data(r io.Reader) error {
	log.Printf("MX: Receiving message from %s to %d recipients", s.from, len(s.recipients))

	if s.from == "" {
		return errors.New("503 No sender specified")
	}

	if len(s.recipients) == 0 {
		return errors.New("503 No valid recipients")
	}

	// Read the entire message
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return fmt.Errorf("failed to read message: %w", err)
	}

	messageData := buf.Bytes()
	messageSize := int64(len(messageData))

	// Parse the message using the new parser
	p := parser.New()
	parsed, err := p.ParseBytes(messageData)
	if err != nil {
		log.Printf("MX: Failed to parse message: %v", err)
		// Still try to save with minimal info
		parsed = &parser.ParsedMessage{
			RawData: messageData,
			RawSize: messageSize,
		}
	}

	// Every message's identity is (user_id, Message-ID). We never invent one
	// server-side — a message without a client-assigned Message-ID is rejected
	// with a hard 5xx so the sender sees an explicit error.
	if parsed.MessageID == "" {
		log.Printf("MX: REJECT message from %s — no Message-ID header", s.from)
		return errors.New("550 Message rejected: missing Message-ID header")
	}

	// Extract values from parsed message
	subject := parsed.Subject
	messageID := parsed.MessageID
	rawDate := parsed.GetDate()
	messageDate := timeutil.ToMs(rawDate)
	messageDateTZ := timeutil.TZOffsetMinutes(rawDate)
	inReplyTo := parsed.InReplyTo
	references := strings.Join(parsed.References, " ")

	fromAddr := parser.FormatAddress(parsed.From)
	toAddr := parser.FormatAddressList(parsed.To)
	ccAddr := parser.FormatAddressList(parsed.Cc)
	replyTo := parser.FormatAddress(parsed.ReplyTo)

	// Use envelope from if header from is empty
	if fromAddr == "" {
		fromAddr = s.from
	}

	// Reject messages with empty To header or where no To address matches our mailboxes
	if len(parsed.To) == 0 {
		log.Printf("MX: Rejecting message from %s - empty To header", fromAddr)
		return errors.New("550 Message rejected - no recipients in To header")
	}
	if !s.hasLocalRecipientInHeaders(parsed.To, parsed.Cc) {
		log.Printf("MX: Rejecting message from %s - no valid local address in To/Cc headers (To: %s)", fromAddr, toAddr)
		return errors.New("550 Message rejected - recipient not in To/Cc headers")
	}

	body := parsed.Body
	bodyHTML := parsed.BodyHTML

	// Log if we found embedded messages or attachments
	if len(parsed.EmbeddedMessages) > 0 {
		log.Printf("MX: Message contains %d embedded message(s)", len(parsed.EmbeddedMessages))
	}
	if len(parsed.Attachments) > 0 {
		log.Printf("MX: Message contains %d attachment(s)", len(parsed.Attachments))
		for _, att := range parsed.Attachments {
			if att.IsDangerous {
				log.Printf("MX: WARNING - Dangerous attachment detected: %s (%s)", att.Filename, att.ContentType)
			}
		}
	}

	// Save message for each recipient
	savedCount := 0
	for _, recipient := range s.recipients {
		// Check user spam rules (whitelist/blacklist)
		isSpam := false
		var spamRuleID *int64

		action, matchedRule, err := s.database.CheckSpamRules(recipient.Mailbox.UserID, fromAddr)
		if err != nil {
			log.Printf("MX: Failed to check spam rules: %v", err)
		}

		fullAllow := action == "allow" && len(matchedRule.ExcludedChecks) == 0
		if fullAllow {
			// Whitelist with no exclusions — never spam, skip analyzer.
			isSpam = false
			log.Printf("MX: Message whitelisted by rule %d for user %d", matchedRule.ID, recipient.Mailbox.UserID)
		} else if action == "spam" {
			// Blacklist - always spam
			isSpam = true
			spamRuleID = &matchedRule.ID
			log.Printf("MX: Message blacklisted by rule %d for user %d", matchedRule.ID, recipient.Mailbox.UserID)
		} else if s.analyzer != nil {
			// Either no rule, or partial-allow with excluded_checks: run the
			// analyzer but layer in the rule's exclusions and per-user
			// weights. Partial-allow never marks the message as spam.
			disabledChecks, err := s.database.GetDisabledSpamChecksMap(recipient.Mailbox.UserID)
			if err != nil {
				log.Printf("MX: Failed to get disabled spam checks: %v", err)
				disabledChecks = nil
			}
			if action == "allow" && len(matchedRule.ExcludedChecks) > 0 {
				if disabledChecks == nil {
					disabledChecks = map[string]bool{}
				}
				for _, c := range matchedRule.ExcludedChecks {
					disabledChecks[c] = true
				}
				spamRuleID = &matchedRule.ID
			}
			weights, wErr := s.database.GetSpamCheckWeights(recipient.Mailbox.UserID)
			if wErr != nil {
				log.Printf("MX: Failed to get spam weights: %v", wErr)
				weights = nil
			}

			s.analyzer.AnalyzeWithUserConfig(parsed, s.senderIP, s.fromDomain, disabledChecks, weights)
			if parsed.SpamScore > 0 {
				log.Printf("MX: Spam analysis for user %d - score=%.1f status=%s reasons=%v",
					recipient.Mailbox.UserID, parsed.SpamScore, parsed.SpamStatus, parsed.SpamReasons)
			}
			if parsed.AuthResults != nil {
				log.Printf("MX: Auth results - SPF=%s DKIM=%s",
					parsed.AuthResults.SPF, parsed.AuthResults.DKIM)
			}

			if parsed.SpamStatus == parser.SpamStatusSpam && action != "allow" {
				isSpam = true
				log.Printf("MX: Message marked as spam by system (score=%.1f) for user %d",
					parsed.SpamScore, recipient.Mailbox.UserID)
			}
		}

		// Find or create INBOX folder for the user (spam still needs a folder reference)
		folderID, err := s.getOrCreateInbox(recipient.Mailbox.UserID)
		if err != nil {
			log.Printf("MX: Failed to get inbox for user %d: %v", recipient.Mailbox.UserID, err)
			continue
		}

		// Get next UID atomically for this folder
		nextUID, err := s.database.GetNextUIDForFolder(folderID)
		if err != nil {
			log.Printf("MX: Failed to get next UID for folder %d: %v", folderID, err)
			continue
		}

		// Create message
		msg := &models.Message{
			AccountID:         0, // No external account - local delivery
			UserID:            recipient.Mailbox.UserID,
			FolderID:          folderID,
			MessageID:         messageID,
			Subject:           subject,
			From:              fromAddr,
			To:                toAddr,
			Cc:                ccAddr,
			ReplyTo:           replyTo,
			Date:              messageDate,
			DateTZ:            messageDateTZ,
			Body:              body,
			BodyHTML:          bodyHTML,
			RawEmail:          messageData,
			Size:              messageSize,
			UID:               nextUID,
			Seen:              false,
			Flagged:           false,
			Answered:          false,
			Draft:             false,
			Deleted:           false,
			InReplyTo:         inReplyTo,
			MessageReferences: references,
			SpamScore:         parsed.SpamScore,
			SpamStatus:        string(parsed.SpamStatus),
			SpamReasons:       parser.GetSpamReasonsJSON(parsed.SpamReasons),
			IsSpam:            isSpam,
			SpamRuleID:        spamRuleID,
		}

		// Save to database
		if err := s.database.CreateMessage(msg); err != nil {
			if errors.Is(err, db.ErrDuplicateMessage) {
				log.Printf("MX: duplicate (user_id, Message-ID) for %s — skip", recipient.Email)
				continue
			}
			log.Printf("MX: Failed to save message for %s: %v", recipient.Email, err)
			continue
		}

		// Save attachments
		for _, att := range parsed.Attachments {
			attachment := &models.Attachment{
				MessageID:   msg.ID,
				ContentID:   att.ContentID,
				Filename:    att.Filename,
				ContentType: att.ContentType,
				Size:        int(att.Size),
				IsInline:    att.IsInline,
				Data:        att.Data,
			}
			if err := s.database.CreateAttachment(attachment); err != nil {
				log.Printf("MX: Failed to save attachment %s: %v", att.Filename, err)
			}
		}

		savedCount++
		if isSpam {
			log.Printf("MX: Message %d saved as SPAM for %s (user_id=%d)",
				msg.ID, recipient.Email, recipient.Mailbox.UserID)
		} else {
			log.Printf("MX: Message %d saved for %s (user_id=%d, folder_id=%d)",
				msg.ID, recipient.Email, recipient.Mailbox.UserID, folderID)

			// Publish notification for IMAP IDLE clients (NOT for spam)
			if s.hub != nil {
				// Get message count and username for IMAP update
				count, _ := s.database.GetMessageCountByFolder(folderID)
				user, _ := s.database.GetUserByID(recipient.Mailbox.UserID)
				username := ""
				if user != nil {
					username = user.Username
				}

				s.hub.Publish(notify.Event{
					UserID:    recipient.Mailbox.UserID,
					FolderID:  folderID,
					Type:      notify.EventNewMessage,
					Count:     count,
					Username:  username,
					Mailbox:   "INBOX",
					From:      msg.From,
					Subject:   msg.Subject,
					MessageID: msg.ID,
					NewCount:  1,
				})
			}

			// Trigger calendar sync if message contains calendar invite (.ics)
			if s.calendarSyncTrigger != nil && s.hasCalendarInvite(parsed) {
				log.Printf("MX: Calendar invite detected, triggering sync for user %d", recipient.Mailbox.UserID)
				go s.calendarSyncTrigger(recipient.Mailbox.UserID)
			}

			// iTIP processing: REQUEST/CANCEL/REPLY/COUNTER attached as .ics
			// shouldn't surface in the conversation list. If the dispatch
			// returns processed=true the message + its attachments go away.
			// Local MX delivery has no account_id (=0) — FindUserCalendarForInvites
			// falls back to "any enabled calendar" for the user, which is fine.
			handler := calendar.NewIncomingHandler(s.database)
			if processed, perr := handler.ProcessAndDispatch(parsed, recipient.Mailbox.UserID, 0, []string{recipient.Email}); perr != nil {
				log.Printf("MX: ICS dispatch error for msg %d: %v", msg.ID, perr)
			} else if processed {
				if delErr := s.database.HardDeleteMessage(msg.ID); delErr != nil {
					log.Printf("MX: failed to drop iTIP-consumed msg %d: %v", msg.ID, delErr)
				} else {
					log.Printf("MX: msg %d consumed by iTIP handler, deleted", msg.ID)
				}
			}
		}
	}

	if savedCount == 0 {
		return errors.New("451 Failed to deliver message to any recipient")
	}

	log.Printf("MX: Message delivered to %d/%d recipients", savedCount, len(s.recipients))
	return nil
}

// Reset resets the session state
func (s *Session) Reset() {
	log.Printf("MX: Session reset")
	s.from = ""
	s.recipients = nil
}

// Logout is called when the client logs out
func (s *Session) Logout() error {
	log.Printf("MX: Session logout")
	return nil
}

// getOrCreateInbox finds or creates an INBOX folder for the user
func (s *Session) getOrCreateInbox(userID int64) (int64, error) {
	folder, err := s.database.GetOrCreateLocalInbox(userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get/create local inbox: %w", err)
	}
	return folder.ID, nil
}

// extractEmail extracts email address from various formats
func (s *Session) extractEmail(addr string) string {
	addr = strings.TrimSpace(addr)

	// Handle formats like:
	// - "user@example.com"
	// - "Name <user@example.com>"
	// - "<user@example.com>"

	start := strings.Index(addr, "<")
	end := strings.Index(addr, ">")

	if start >= 0 && end > start {
		return strings.TrimSpace(addr[start+1 : end])
	}

	return strings.ToLower(addr)
}

// splitEmail splits email into local part and domain
func (s *Session) splitEmail(email string) (string, string) {
	parts := strings.SplitN(strings.ToLower(email), "@", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// hasLocalRecipientInHeaders checks if any envelope recipient appears in the To or Cc headers
func (s *Session) hasLocalRecipientInHeaders(to []*mail.Address, cc []*mail.Address) bool {
	headerAddrs := make(map[string]bool)
	for _, addr := range to {
		if addr != nil {
			headerAddrs[strings.ToLower(addr.Address)] = true
		}
	}
	for _, addr := range cc {
		if addr != nil {
			headerAddrs[strings.ToLower(addr.Address)] = true
		}
	}
	for _, rcpt := range s.recipients {
		if headerAddrs[strings.ToLower(rcpt.Email)] {
			return true
		}
	}
	return false
}

// hasCalendarInvite checks if the message contains a calendar invite (.ics attachment)
func (s *Session) hasCalendarInvite(parsed *parser.ParsedMessage) bool {
	if parsed == nil {
		return false
	}

	for _, att := range parsed.Attachments {
		// Check content type
		contentType := strings.ToLower(att.ContentType)
		if strings.Contains(contentType, "text/calendar") ||
			strings.Contains(contentType, "application/ics") {
			return true
		}

		// Check filename extension
		filename := strings.ToLower(att.Filename)
		if strings.HasSuffix(filename, ".ics") {
			return true
		}
	}

	return false
}
