package db

import (
	"fmt"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/emersion/go-message/mail"
)

// SenderStore is the part of the database sender ownership is decided from.
// *DB satisfies it; tests substitute a fake.
type SenderStore interface {
	GetAccountsByUserID(userID int64) ([]*models.Account, error)
	GetMailboxesWithDomainByUserID(userID int64) ([]*MailboxWithDomain, error)
}

// SenderIdentities returns every address the user may send as (lowercased),
// mapped to the account it is sent through: >0 relays through that external
// account, 0 is direct delivery with our DKIM signature. It is the same set
// the clients are offered as identities (desktop /identities, IMAP METADATA):
//   - enabled local mailboxes owned by the user → direct delivery;
//   - the address of each enabled external account → relay through it;
//   - each alias of an enabled external account → relay through it.
//
// When one address qualifies twice, an account's own address wins over a
// local mailbox, which wins over an alias. Owning a domain does not by itself
// make every address on it sendable: a mailbox must exist.
//
// Every path that queues outgoing mail (SMTP submission, desktop API) must
// check the sender against this set; otherwise a user could send as any
// address, DKIM-signed by us.
func SenderIdentities(store SenderStore, userID int64) (map[string]int64, error) {
	owned := make(map[string]int64)

	accounts, err := store.GetAccountsByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("loading accounts: %w", err)
	}
	mailboxes, err := store.GetMailboxesWithDomainByUserID(userID)
	if err != nil {
		return nil, fmt.Errorf("loading mailboxes: %w", err)
	}

	for _, acc := range accounts {
		if !acc.Enabled || acc.UserID != userID {
			continue
		}
		for _, alias := range acc.GetAliases() {
			owned[alias] = acc.ID
		}
	}
	for _, mb := range mailboxes {
		if !mb.Enabled || mb.UserID != userID || mb.LocalPart == "" || mb.DomainName == "" {
			continue
		}
		owned[strings.ToLower(mb.LocalPart+"@"+mb.DomainName)] = 0
	}
	for _, acc := range accounts {
		if !acc.Enabled || acc.UserID != userID {
			continue
		}
		if email := strings.ToLower(strings.TrimSpace(acc.Email)); email != "" {
			owned[email] = acc.ID
		}
	}
	return owned, nil
}

// ParseSingleSender parses a From value that must name exactly one mailbox
// ("addr", "<addr>" or "Name <addr>", RFC 2047 / UTF-8 names allowed) and
// returns its lowercased address. CR/LF are refused outright: the value ends
// up in a header, where they would let a caller inject a second From.
func ParseSingleSender(from string) (string, error) {
	if strings.ContainsAny(from, "\r\n") {
		return "", fmt.Errorf("sender contains a line break")
	}
	list, err := mail.ParseAddressList(from)
	if err != nil {
		return "", fmt.Errorf("invalid sender %q: %w", from, err)
	}
	if len(list) != 1 {
		return "", fmt.Errorf("sender must be exactly one address, got %d", len(list))
	}
	return strings.ToLower(list[0].Address), nil
}
