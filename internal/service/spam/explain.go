// Package spam explains spam verdicts. The explanation is built from the
// verdict stored at delivery (is_spam, spam_rule_id, spam_score/status/
// reasons written by parser.Analyzer in the MX and IMAP-sync paths) plus a
// re-run of that same analyzer — never from a second scoring engine. The web
// page "why is this spam" used to run its own ~1000-line engine with its own
// rules and weights and showed a verdict the message was not filtered by.
package spam

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
)

// ErrNotFound: no such message for this user.
var ErrNotFound = db.ErrNotFound

// Store is the slice of internal/db the explainer reads. *db.DB implements it.
type Store interface {
	GetMessageByMessageID(userID int64, messageID string) (*models.Message, error)
	GetMessageRawEmail(messageID int64) ([]byte, error)
	GetMessageSpamState(messageID int64) (*db.MessageSpamState, error)
	GetSpamRuleByID(id int64) (*db.SpamRule, error)
	CheckSpamRules(userID int64, fromEmail string) (string, *db.SpamRule, error)
	GetDisabledSpamChecksMap(userID int64) (map[string]bool, error)
	GetSpamCheckWeights(userID int64) (map[string]float64, error)
	GetSenderStats(userID int64, senderEmail, senderDomain string) (*db.SenderStats, error)
	GetDomainStats(userID int64, domain string) (int, int, error)
}

// Explainer builds explanations. Safe for concurrent use.
type Explainer struct {
	store    Store
	analyzer *parser.Analyzer
}

// NewExplainer returns an explainer re-running analyzer — it must be
// configured like the delivery analyzer. Pass nil for the stock configuration
// with the network checks (SPF, DKIM, RBL) off: their answers depend on the
// moment of delivery, so the stored reasons stay authoritative for them.
func NewExplainer(store Store, analyzer *parser.Analyzer) *Explainer {
	if analyzer == nil {
		cfg := parser.DefaultAnalyzerConfig()
		cfg.CheckSPF, cfg.CheckDKIM, cfg.CheckRBL = false, false, false
		analyzer = parser.NewAnalyzer(cfg)
	}
	return &Explainer{store: store, analyzer: analyzer}
}

// Finding is one rule that fired at delivery.
type Finding struct {
	// Check is the analyzer category, or "upstream" / "recipient" for the
	// verdicts recorded outside the analyzer.
	Check string
	// Score is the rule's weighted contribution; valid only when Scored.
	// Network checks (not re-run) and non-analyzer reasons have no score.
	Score  float64
	Scored bool
	Reason string
}

// Recheck is the same analysis run again now on the stored message, with the
// user's current settings.
type Recheck struct {
	Score    float64
	Status   parser.SpamStatus
	Findings []parser.SpamFinding
	// Differs reports that re-running gives other rules than at delivery
	// (ignoring the network checks, which are not re-run): settings or the
	// analyzer changed since.
	Differs bool
}

// Explanation of why a message is (or is not) in spam.
type Explanation struct {
	Message   *models.Message
	FromEmail string
	FromName  string
	Domain    string

	// The verdict the message is actually filtered by, as stored.
	IsSpam bool
	Status string
	Score  float64
	// Rule is the user rule recorded on the message: a blacklist hit (the
	// analyzer was not consulted) or a partial whitelist. Nil when none.
	Rule *db.SpamRule
	// Whitelisted: a whitelist rule without exclusions covers the sender
	// now — such mail skips the analyzer entirely.
	Whitelisted bool
	// Findings are the rules that fired at delivery, in order.
	Findings []Finding

	Recheck Recheck

	Links parser.LinkReport

	// Sender history (informational — it does not score).
	SenderStats  *db.SenderStats
	DomainTotal  int
	DomainSpam   int
	HasWhitelist bool
	HasBlacklist bool
}

// DecidedByRule reports a blacklist verdict: the analyzer was not consulted.
func (e *Explanation) DecidedByRule() bool {
	return e.Rule != nil && e.Rule.Action == "spam"
}

// ExplainByMessageID explains the user's message with the given RFC 5322
// Message-ID.
func (x *Explainer) ExplainByMessageID(userID int64, messageID string) (*Explanation, error) {
	msg, err := x.store.GetMessageByMessageID(userID, messageID)
	if err != nil {
		return nil, fmt.Errorf("find message: %w", err)
	}
	if msg == nil || msg.UserID != userID {
		return nil, ErrNotFound
	}
	return x.Explain(userID, msg)
}

// Explain explains msg, which the caller has already authorized for userID.
func (x *Explainer) Explain(userID int64, msg *models.Message) (*Explanation, error) {
	state, err := x.store.GetMessageSpamState(msg.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e := &Explanation{
		Message: msg,
		IsSpam:  state.IsSpam,
		Status:  msg.SpamStatus,
		Score:   msg.SpamScore,
	}
	if state.RuleID != nil {
		rule, err := x.store.GetSpamRuleByID(*state.RuleID)
		if err == nil {
			e.Rule = rule
		} // a deleted rule leaves a dangling id: explain without it
	}

	parsed, err := x.parsedMessage(msg)
	if err != nil {
		return nil, err
	}
	if parsed.From != nil {
		e.FromEmail = strings.ToLower(parsed.From.Address)
		e.FromName = parsed.From.Name
		if at := strings.LastIndex(e.FromEmail, "@"); at >= 0 {
			e.Domain = e.FromEmail[at+1:]
		}
	}

	if err := x.recheck(userID, msg, parsed, e); err != nil {
		return nil, err
	}
	e.Findings = annotate(storedReasons(msg.SpamReasons), e.Recheck.Findings)
	// Comparing makes sense only when the analyzer decided at delivery: a
	// blacklist hit, a full whitelist or the upstream's spam folder skip it.
	analyzerDecided := !e.DecidedByRule() && !e.Whitelisted && !hasCheck(e.Findings, "upstream")
	e.Recheck.Differs = analyzerDecided && differs(e.Findings, e.Recheck.Findings)
	e.Links = x.analyzer.Links(parsed)

	if e.FromEmail != "" {
		stats, err := x.store.GetSenderStats(userID, e.FromEmail, e.Domain)
		if err != nil {
			return nil, fmt.Errorf("sender stats: %w", err)
		}
		e.SenderStats = stats
		e.HasWhitelist, e.HasBlacklist = stats.HasWhitelist, stats.HasBlacklist
		total, spam, err := x.store.GetDomainStats(userID, e.Domain)
		if err != nil {
			return nil, fmt.Errorf("domain stats: %w", err)
		}
		e.DomainTotal, e.DomainSpam = total, spam
	}
	return e, nil
}

// recheck re-runs the analyzer with the user's configuration assembled the
// way the delivery paths (MX session, IMAP sync) assemble it: disabled
// checks, per-category weights, and a partial whitelist rule's exclusions.
func (x *Explainer) recheck(userID int64, msg *models.Message, parsed *parser.ParsedMessage, e *Explanation) error {
	disabled, err := x.store.GetDisabledSpamChecksMap(userID)
	if err != nil {
		return fmt.Errorf("disabled checks: %w", err)
	}
	weights, err := x.store.GetSpamCheckWeights(userID)
	if err != nil {
		return fmt.Errorf("check weights: %w", err)
	}
	action, rule, err := x.store.CheckSpamRules(userID, msg.From)
	if err != nil {
		return fmt.Errorf("spam rules: %w", err)
	}
	e.Whitelisted = action == "allow" && rule != nil && len(rule.ExcludedChecks) == 0
	if action == "allow" && rule != nil && len(rule.ExcludedChecks) > 0 {
		merged := make(map[string]bool, len(disabled)+len(rule.ExcludedChecks))
		for k, v := range disabled {
			merged[k] = v
		}
		for _, c := range rule.ExcludedChecks {
			merged[c] = true
		}
		disabled = merged
	}
	x.analyzer.AnalyzeWithUserConfig(parsed, "", "", disabled, weights)
	e.Recheck = Recheck{Score: parsed.SpamScore, Status: parsed.SpamStatus, Findings: parsed.SpamFindings}
	return nil
}

// parsedMessage re-parses the stored original; legacy rows without one are
// rebuilt from the stored fields (no headers beyond those, so the Received
// chain checks see nothing).
func (x *Explainer) parsedMessage(msg *models.Message) (*parser.ParsedMessage, error) {
	raw, err := x.store.GetMessageRawEmail(msg.ID)
	if err != nil {
		return nil, fmt.Errorf("raw message: %w", err)
	}
	if len(raw) > 0 {
		if parsed, err := parser.New().ParseBytes(raw); err == nil {
			return parsed, nil
		}
	}
	p := &parser.ParsedMessage{
		MessageID: msg.MessageID,
		Subject:   msg.Subject,
		Body:      msg.Body,
		BodyHTML:  msg.BodyHTML,
		InReplyTo: msg.InReplyTo,
	}
	if a, err := mail.ParseAddress(msg.From); err == nil {
		p.From = a
	} else if strings.Contains(msg.From, "@") {
		p.From = &mail.Address{Address: strings.Trim(strings.TrimSpace(msg.From), "<>")}
	}
	if a, err := mail.ParseAddress(msg.ReplyTo); err == nil {
		p.ReplyTo = a
	}
	return p, nil
}

func storedReasons(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var reasons []string
	if err := json.Unmarshal([]byte(raw), &reasons); err != nil {
		return []string{raw} // not JSON: show it as is rather than drop it
	}
	return reasons
}

// annotate pairs each stored reason with the re-run finding of the same text
// (category + score). Reasons the re-run cannot reproduce keep their text and
// get the category from their shape.
func annotate(reasons []string, recheck []parser.SpamFinding) []Finding {
	used := make([]bool, len(recheck))
	out := make([]Finding, 0, len(reasons))
	for _, r := range reasons {
		f := Finding{Reason: r, Check: reasonCheck(r)}
		for i, rf := range recheck {
			if !used[i] && rf.Reason == r {
				used[i] = true
				f.Check, f.Score, f.Scored = rf.Check, rf.Score, true
				break
			}
		}
		out = append(out, f)
	}
	return out
}

// reasonCheck categorizes reasons not reproduced by the offline re-run: the
// network checks and the verdicts recorded outside the analyzer.
func reasonCheck(reason string) string {
	switch {
	case strings.HasPrefix(reason, "SPF "):
		return "spf"
	case strings.HasPrefix(reason, "DKIM "):
		return "dkim"
	case strings.HasPrefix(reason, "RBL listed"):
		return "rbl"
	case strings.HasPrefix(reason, "classified spam by upstream"):
		return "upstream"
	case strings.HasPrefix(reason, "recipient mismatch"):
		return "recipient"
	}
	return ""
}

func hasCheck(findings []Finding, check string) bool {
	for _, f := range findings {
		if f.Check == check {
			return true
		}
	}
	return false
}

func isRecheckable(check string) bool {
	switch check {
	case "spf", "dkim", "rbl", "upstream", "recipient":
		return false
	}
	return true
}

// differs compares delivery and re-run on the rules the re-run can reproduce.
func differs(stored []Finding, recheck []parser.SpamFinding) bool {
	count := map[string]int{}
	for _, f := range stored {
		if isRecheckable(f.Check) {
			count[f.Reason]++
		}
	}
	for _, f := range recheck {
		if isRecheckable(f.Check) {
			count[f.Reason]--
		}
	}
	for _, n := range count {
		if n != 0 {
			return true
		}
	}
	return false
}
