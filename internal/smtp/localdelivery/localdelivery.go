// Package localdelivery puts a message into the mailbox of a user of a domain
// this server hosts. It is the one implementation of "deliver locally", shared
// by the MX listener (mail from the outside) and the outbound queue (mail our
// own users send to addresses on our own domains).
//
// The outbound queue used to resolve the MX of such a recipient and dial it —
// that is, dial this very server on its public address. Behind NAT that
// connection is refused (the 25→2525 redirect applies to external traffic
// only), so mail between two of our own domains sat in the queue until it was
// given up on. Delivering it in-process skips the network entirely and runs
// the recipient side exactly as the MX does: spam rules, INBOX, IDLE/desktop
// notifications, calendar invites.
package localdelivery

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/logmask"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/notify"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/timeutil"
)

// ErrNotLocal means the recipient's domain is not hosted here: the message
// goes out through MX like any other.
var ErrNotLocal = errors.New("domain is not hosted on this server")

// RejectError is a permanent refusal of one local recipient, phrased the way
// SMTP phrases it. The MX answers RCPT with it, the submission port does the
// same, and the outbound queue puts it into the bounce (DSN) to the sender.
type RejectError struct {
	Code     int    // SMTP reply code, 5xx
	Enhanced [3]int // RFC 3463 status, e.g. 5.1.1
	Message  string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("%d %s %s", e.Code, e.Status(), e.Message)
}

// Status renders the enhanced status code ("5.1.1").
func (e *RejectError) Status() string {
	return fmt.Sprintf("%d.%d.%d", e.Enhanced[0], e.Enhanced[1], e.Enhanced[2])
}

// Directory is what recipient lookup needs from the database.
type Directory interface {
	GetDomainByName(domainName string) (*models.Domain, error)
	GetMailbox(domainID int64, localPart string) (*models.Mailbox, error)
}

// Recipient is a resolved local mailbox.
type Recipient struct {
	Email     string // lower-cased addr-spec
	LocalPart string
	Mailbox   *models.Mailbox
	Domain    *models.Domain
}

// SplitAddress lower-cases an addr-spec and splits it at the last "@".
// "" "" for anything that is not local@domain.
func SplitAddress(email string) (localPart, domain string) {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "", ""
	}
	return email[:at], strings.TrimSuffix(email[at+1:], ".")
}

// Lookup resolves an addr-spec against the hosted domains.
//
//   - ErrNotLocal: the domain is not ours, deliver through MX;
//   - *RejectError: the domain is ours but the address cannot take mail
//     (no such mailbox, mailbox or domain disabled) — a permanent failure;
//   - any other error: the database could not answer, try again later.
//
// There are no aliases or catch-all mailboxes in the model: an address is
// deliverable only if a mailbox with exactly that local part exists.
func Lookup(dir Directory, email string) (*Recipient, error) {
	localPart, domainName := SplitAddress(email)
	if localPart == "" || domainName == "" {
		return nil, &RejectError{Code: 550, Enhanced: [3]int{5, 1, 3}, Message: "Invalid recipient address"}
	}

	domain, err := dir.GetDomainByName(domainName)
	if errors.Is(err, db.ErrDomainNotFound) {
		return nil, ErrNotLocal
	}
	if err != nil {
		return nil, fmt.Errorf("look up domain %s: %w", domainName, err)
	}
	if !domain.Enabled {
		return nil, &RejectError{Code: 550, Enhanced: [3]int{5, 7, 1}, Message: "Relay access denied - domain disabled"}
	}

	mailbox, err := dir.GetMailbox(domain.ID, localPart)
	if errors.Is(err, db.ErrMailboxNotFound) {
		return nil, &RejectError{Code: 550, Enhanced: [3]int{5, 1, 1}, Message: "No such user here"}
	}
	if err != nil {
		return nil, fmt.Errorf("look up mailbox %s: %w", logmask.Addr(email), err)
	}
	if !mailbox.Enabled {
		return nil, &RejectError{Code: 550, Enhanced: [3]int{5, 2, 1}, Message: "Mailbox disabled"}
	}

	return &Recipient{
		Email:     localPart + "@" + domainName,
		LocalPart: localPart,
		Mailbox:   mailbox,
		Domain:    domain,
	}, nil
}

// Store is everything delivery needs from the database.
type Store interface {
	Directory
	CheckSpamRules(userID int64, fromEmail string) (string, *db.SpamRule, error)
	GetDisabledSpamChecksMap(userID int64) (map[string]bool, error)
	GetSpamCheckWeights(userID int64) (map[string]float64, error)
	GetOrCreateLocalInbox(userID int64) (*models.Folder, error)
	GetNextUIDForFolder(folderID int64) (uint32, error)
	CreateMessage(msg *models.Message) error
	CreateAttachment(att *models.Attachment) error
	GetMessageCountByFolder(folderID int64) (uint32, error)
	GetUserByID(id int64) (*models.User, error)
}

// ITIPFunc consumes a calendar message (REQUEST/CANCEL/REPLY…) delivered as
// msgID; true means it was consumed and the mail copy has been removed.
type ITIPFunc func(parsed *parser.ParsedMessage, msgID, userID int64, recipient string) (bool, error)

// Deliverer stores messages into local mailboxes.
type Deliverer struct {
	store        Store
	hub          *notify.Hub
	analyzer     *parser.Analyzer
	calendarSync func(userID int64)
	itip         ITIPFunc
}

// New returns a Deliverer backed by the database. hub, analyzer,
// calendarSync and itip may be nil: no notifications, no content analysis,
// no calendar sync trigger, no iTIP processing. (iTIP is injected rather than
// built here: the calendar package imports the SMTP client, which imports
// this package.)
func New(database *db.DB, hub *notify.Hub, analyzer *parser.Analyzer, calendarSync func(userID int64), itip ITIPFunc) *Deliverer {
	d := NewWithStore(database, hub, analyzer, calendarSync)
	d.itip = itip
	return d
}

// NewWithStore is New over any Store; iTIP processing is off until SetITIP.
func NewWithStore(store Store, hub *notify.Hub, analyzer *parser.Analyzer, calendarSync func(userID int64)) *Deliverer {
	return &Deliverer{store: store, hub: hub, analyzer: analyzer, calendarSync: calendarSync}
}

// SetITIP replaces the calendar-message hook (tests).
func (d *Deliverer) SetITIP(fn ITIPFunc) { d.itip = fn }

// Lookup is Lookup over the deliverer's store.
func (d *Deliverer) Lookup(email string) (*Recipient, error) {
	return Lookup(d.store, email)
}

// Envelope describes where a message came from.
type Envelope struct {
	From       string // envelope sender (MAIL FROM)
	FromDomain string // its domain, for SPF
	SenderIP   string // connecting client, for SPF/RBL
	// Trusted marks mail submitted by an authenticated user of this server.
	// SPF/RBL/content scoring of such mail says nothing (the "client" is the
	// server itself), so the analyzer is skipped; the recipient's own spam
	// rules still apply — a blacklisted sender stays blacklisted.
	Trusted bool
}

// Result is the outcome of delivering to one recipient.
type Result int

const (
	// Delivered: stored (or consumed by the calendar handler).
	Delivered Result = iota
	// Duplicate: the recipient already has a message with this Message-ID —
	// a redelivery (e.g. a retry of a partially delivered message). Nothing
	// was stored, and nothing needs to be.
	Duplicate
	// Failed: could not store; the error says why. Temporary by nature.
	Failed
)

// Deliver stores parsed (raw bytes in raw) into every recipient's INBOX.
// The returned slices are parallel to rcpts; errs[i] is set iff
// results[i] == Failed.
func (d *Deliverer) Deliver(env Envelope, raw []byte, parsed *parser.ParsedMessage, rcpts []*Recipient) ([]Result, []error) {
	results := make([]Result, len(rcpts))
	errs := make([]error, len(rcpts))
	for i, rcpt := range rcpts {
		results[i], errs[i] = d.deliverOne(env, raw, parsed, rcpt)
	}
	return results, errs
}

func (d *Deliverer) deliverOne(env Envelope, raw []byte, parsed *parser.ParsedMessage, rcpt *Recipient) (Result, error) {
	userID := rcpt.Mailbox.UserID

	fromAddr := parser.FormatAddress(parsed.From)
	if fromAddr == "" {
		fromAddr = env.From
	}

	isSpam, spamRuleID := d.classify(env, parsed, userID, fromAddr)

	inbox, err := d.store.GetOrCreateLocalInbox(userID)
	if err != nil {
		return Failed, fmt.Errorf("get inbox for user %d: %w", userID, err)
	}
	folderID := inbox.ID

	nextUID, err := d.store.GetNextUIDForFolder(folderID)
	if err != nil {
		return Failed, fmt.Errorf("next UID for folder %d: %w", folderID, err)
	}

	rawDate := parsed.GetDate()
	msg := &models.Message{
		AccountID:         0, // no external account — local delivery
		UserID:            userID,
		FolderID:          folderID,
		MessageID:         parsed.MessageID,
		Subject:           parsed.Subject,
		From:              fromAddr,
		To:                parser.FormatAddressList(parsed.To),
		Cc:                parser.FormatAddressList(parsed.Cc),
		ReplyTo:           parser.FormatAddress(parsed.ReplyTo),
		Date:              timeutil.ToMs(rawDate),
		DateTZ:            timeutil.TZOffsetMinutes(rawDate),
		Body:              parsed.Body,
		BodyHTML:          parsed.BodyHTML,
		RawEmail:          raw,
		Size:              int64(len(raw)),
		UID:               nextUID,
		InReplyTo:         parsed.InReplyTo,
		MessageReferences: strings.Join(parsed.References, " "),
		SpamScore:         parsed.SpamScore,
		SpamStatus:        string(parsed.SpamStatus),
		SpamReasons:       parser.GetSpamReasonsJSON(parsed.SpamReasons),
		IsSpam:            isSpam,
		SpamRuleID:        spamRuleID,
	}

	if err := d.store.CreateMessage(msg); err != nil {
		if errors.Is(err, db.ErrDuplicateMessage) {
			log.Printf("Local delivery: duplicate (user_id, Message-ID) for %s — skip", logmask.Addr(rcpt.Email))
			return Duplicate, nil
		}
		return Failed, fmt.Errorf("save message for %s: %w", logmask.Addr(rcpt.Email), err)
	}

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
		if err := d.store.CreateAttachment(attachment); err != nil {
			log.Printf("Local delivery: failed to save attachment %s: %v", att.Filename, err)
		}
	}

	if isSpam {
		log.Printf("Local delivery: message %d saved as SPAM for %s (user_id=%d)", msg.ID, logmask.Addr(rcpt.Email), userID)
		return Delivered, nil
	}
	log.Printf("Local delivery: message %d saved for %s (user_id=%d, folder_id=%d)", msg.ID, logmask.Addr(rcpt.Email), userID, folderID)

	// Notify IMAP IDLE and desktop clients (never for spam).
	if d.hub != nil {
		count, _ := d.store.GetMessageCountByFolder(folderID)
		username := ""
		if user, _ := d.store.GetUserByID(userID); user != nil {
			username = user.Username
		}
		d.hub.Publish(notify.Event{
			UserID:    userID,
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

	if d.calendarSync != nil && HasCalendarInvite(parsed) {
		log.Printf("Local delivery: calendar invite detected, triggering sync for user %d", userID)
		go d.calendarSync(userID)
	}

	// iTIP processing: REQUEST/CANCEL/REPLY/COUNTER attached as .ics
	// shouldn't surface in the conversation list.
	if d.itip != nil {
		if processed, err := d.itip(parsed, msg.ID, userID, rcpt.Email); err != nil {
			log.Printf("Local delivery: ICS dispatch error for msg %d: %v", msg.ID, err)
		} else if processed {
			log.Printf("Local delivery: msg %d consumed by iTIP handler, deleted", msg.ID)
		}
	}

	return Delivered, nil
}

// classify applies the recipient's spam rules and, for untrusted mail, the
// content analyzer. Returns the spam verdict and the rule that decided it.
func (d *Deliverer) classify(env Envelope, parsed *parser.ParsedMessage, userID int64, fromAddr string) (bool, *int64) {
	action, rule, err := d.store.CheckSpamRules(userID, fromAddr)
	if err != nil {
		log.Printf("Local delivery: failed to check spam rules: %v", err)
	}

	switch {
	case action == "allow" && (rule == nil || len(rule.ExcludedChecks) == 0):
		// Whitelist with no exclusions — never spam, skip the analyzer.
		return false, nil
	case action == "spam" && rule != nil:
		id := rule.ID
		log.Printf("Local delivery: message blacklisted by rule %d for user %d", id, userID)
		return true, &id
	case env.Trusted || d.analyzer == nil:
		return false, nil
	}

	// No rule, or partial-allow with excluded checks: run the analyzer with
	// the rule's exclusions and the user's weights. Partial-allow never marks
	// the message as spam.
	var spamRuleID *int64
	disabled, err := d.store.GetDisabledSpamChecksMap(userID)
	if err != nil {
		log.Printf("Local delivery: failed to get disabled spam checks: %v", err)
		disabled = nil
	}
	if action == "allow" && rule != nil {
		if disabled == nil {
			disabled = map[string]bool{}
		}
		for _, c := range rule.ExcludedChecks {
			disabled[c] = true
		}
		id := rule.ID
		spamRuleID = &id
	}
	weights, err := d.store.GetSpamCheckWeights(userID)
	if err != nil {
		log.Printf("Local delivery: failed to get spam weights: %v", err)
		weights = nil
	}

	d.analyzer.AnalyzeWithUserConfig(parsed, env.SenderIP, env.FromDomain, disabled, weights)
	if parsed.SpamScore > 0 {
		log.Printf("Local delivery: spam analysis for user %d - score=%.1f status=%s reasons=%v",
			userID, parsed.SpamScore, parsed.SpamStatus, parsed.SpamReasons)
	}
	if parsed.AuthResults != nil {
		log.Printf("Local delivery: auth results - SPF=%s DKIM=%s", parsed.AuthResults.SPF, parsed.AuthResults.DKIM)
	}
	if parsed.SpamStatus == parser.SpamStatusSpam && action != "allow" {
		log.Printf("Local delivery: message marked as spam by system (score=%.1f) for user %d", parsed.SpamScore, userID)
		return true, spamRuleID
	}
	return false, spamRuleID
}

// HasCalendarInvite reports whether the message carries a calendar object.
func HasCalendarInvite(parsed *parser.ParsedMessage) bool {
	if parsed == nil {
		return false
	}
	for _, att := range parsed.Attachments {
		contentType := strings.ToLower(att.ContentType)
		if strings.Contains(contentType, "text/calendar") || strings.Contains(contentType, "application/ics") {
			return true
		}
		if strings.HasSuffix(strings.ToLower(att.Filename), ".ics") {
			return true
		}
	}
	return false
}
