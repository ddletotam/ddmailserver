package spam

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/service/messages"
)

// ErrNoSender: nothing identifies a sender to block.
var ErrNoSender = errors.New("no sender to block")

// RuleStore is what blocking needs from the database. *db.DB implements it.
type RuleStore interface {
	GetSenderAddrsByIDs(userID int64, ids []int64) ([]string, error)
	CreateSpamRule(rule *db.SpamRule) error
}

// Purger hard-deletes messages; *messages.Service implements it.
type Purger interface {
	Purge(ctx context.Context, userID int64, sel messages.PurgeSelector) (messages.PurgeResult, error)
}

// Blocker implements "this sender is spam": blacklist it and purge its mail.
type Blocker struct {
	rules  RuleStore
	purger Purger
}

// NewBlocker returns a blocker.
func NewBlocker(rules RuleStore, purger Purger) *Blocker {
	return &Blocker{rules: rules, purger: purger}
}

// BlockRule is one blacklist entry: Type is "address" or "domain".
type BlockRule struct {
	Type  string
	Value string
}

// BlockRequest selects what to block.
type BlockRequest struct {
	// MessageIDs are the rows the user is looking at; the REAL sender is read
	// from them, and they are purged by id too (covers outgoing threads).
	MessageIDs []int64
	// FallbackAddress / FallbackDomain are the client's guess, used only when
	// the rows resolve no sender (e.g. an IMAP-provider conversation).
	FallbackAddress string
	FallbackDomain  string
	// Scope "domain" blocks whole domains (spammers rotate local-parts);
	// anything else blocks exact addresses.
	Scope string
}

// BlockResult reports what was blocked and purged.
type BlockResult struct {
	// Rules are sorted by value; Rules[0] is the "primary" one the UI shows.
	Rules        []BlockRule
	Deleted      int64
	QueuedRemote int
}

// BlockAndPurge blacklists the sender(s) of the selected messages for the
// user and hard-deletes those messages plus all mail from the blocked
// senders/domains — no vault, by design: "I don't want it anywhere". External
// accounts' copies are deleted at the source too (messages.Service.Purge).
func (b *Blocker) BlockAndPurge(ctx context.Context, userID int64, req BlockRequest) (BlockResult, error) {
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if scope != "domain" {
		scope = "address"
	}
	fromAddrs, err := b.rules.GetSenderAddrsByIDs(userID, req.MessageIDs)
	if err != nil {
		// The fallback hint can still identify the sender.
		log.Printf("spam block: sender lookup for user %d: %v", userID, err)
	}
	rules := SenderRules(fromAddrs, req.FallbackAddress, req.FallbackDomain, scope)
	if len(rules) == 0 {
		return BlockResult{}, ErrNoSender
	}

	sel := messages.PurgeSelector{IDs: req.MessageIDs}
	for _, r := range rules {
		rule := &db.SpamRule{UserID: userID, RuleType: r.Type, RuleValue: r.Value, Action: "spam"}
		if err := b.rules.CreateSpamRule(rule); err != nil {
			// A duplicate rule (unique constraint) is fine — already blocked.
			log.Printf("spam block: create rule %s=%s for user %d: %v", r.Type, r.Value, userID, err)
		}
		if r.Type == "domain" {
			sel.SenderDomains = append(sel.SenderDomains, r.Value)
		} else {
			sel.SenderAddresses = append(sel.SenderAddresses, r.Value)
		}
	}

	purged, err := b.purger.Purge(ctx, userID, sel)
	if err != nil {
		return BlockResult{Rules: rules}, fmt.Errorf("purge: %w", err)
	}
	return BlockResult{Rules: rules, Deleted: purged.Deleted, QueuedRemote: purged.Queued}, nil
}

// SenderRules resolves the block set from the REAL senders (fromAddrs, raw
// "Name <addr>" headers of the selected messages) under scope. When no sender
// resolves it falls back to the client's hint. Sorted by value, deduplicated.
//
// Trusting the rows, not the client's counterpart guess, is what fixes the
// BCC-blast case: From=spammer, To=someone else lands both addresses in the
// conversation's participants, and blocking "the first one" often hit the
// innocent To address.
func SenderRules(fromAddrs []string, fallbackAddr, fallbackDomain, scope string) []BlockRule {
	senders := map[string]bool{}
	domains := map[string]bool{}
	for _, fa := range fromAddrs {
		email := strings.ToLower(headerAddress(fa))
		if email == "" {
			continue
		}
		senders[email] = true
		if at := strings.LastIndex(email, "@"); at >= 0 {
			domains[email[at+1:]] = true
		}
	}
	if len(senders) == 0 {
		addr := strings.ToLower(strings.TrimSpace(fallbackAddr))
		dom := strings.ToLower(strings.TrimSpace(fallbackDomain))
		if addr != "" {
			senders[addr] = true
			if at := strings.LastIndex(addr, "@"); at >= 0 {
				domains[addr[at+1:]] = true
			}
		} else if dom != "" {
			domains[dom] = true
		}
	}
	var rules []BlockRule
	if scope == "domain" {
		for d := range domains {
			rules = append(rules, BlockRule{Type: "domain", Value: d})
		}
	} else {
		for a := range senders {
			rules = append(rules, BlockRule{Type: "address", Value: a})
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Value < rules[j].Value })
	return rules
}

// headerAddress extracts the address of a "Name <addr>" header value.
func headerAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if start := strings.Index(addr, "<"); start >= 0 {
		if end := strings.Index(addr[start:], ">"); end > 0 {
			return strings.TrimSpace(addr[start+1 : start+end])
		}
	}
	return addr
}
