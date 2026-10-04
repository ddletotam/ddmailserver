package spam

import (
	"errors"
	"strings"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
)

const userID = int64(5)

var promoRaw = strings.ReplaceAll(`From: Sberbank Bonus <noreply@promo-sberr.example>
To: bob@example.net
Reply-To: claims@other.example
Subject: WINNER act now
X-Mailer: PHPMailer 6.0
Content-Type: text/html; charset=utf-8

<p>Free money! Click here, act now. Short link http://bit.ly/x1 https://vk.cc/abc</p>
`, "\n", "\r\n")

type fakeStore struct {
	msg      *models.Message
	raw      []byte
	state    db.MessageSpamState
	rules    map[int64]*db.SpamRule
	action   string
	matched  *db.SpamRule
	disabled map[string]bool
	weights  map[string]float64
}

func (f *fakeStore) GetMessageByMessageID(uid int64, mid string) (*models.Message, error) {
	if f.msg == nil || f.msg.UserID != uid || f.msg.MessageID != mid {
		return nil, nil
	}
	return f.msg, nil
}
func (f *fakeStore) GetMessageRawEmail(int64) ([]byte, error) { return f.raw, nil }
func (f *fakeStore) GetMessageSpamState(int64) (*db.MessageSpamState, error) {
	st := f.state
	return &st, nil
}
func (f *fakeStore) GetSpamRuleByID(id int64) (*db.SpamRule, error) {
	if r, ok := f.rules[id]; ok {
		return r, nil
	}
	return nil, db.ErrNotFound
}
func (f *fakeStore) CheckSpamRules(int64, string) (string, *db.SpamRule, error) {
	return f.action, f.matched, nil
}
func (f *fakeStore) GetDisabledSpamChecksMap(int64) (map[string]bool, error) { return f.disabled, nil }
func (f *fakeStore) GetSpamCheckWeights(int64) (map[string]float64, error)   { return f.weights, nil }
func (f *fakeStore) GetSenderStats(int64, string, string) (*db.SenderStats, error) {
	return &db.SenderStats{TotalMessages: 3, SpamMessages: 2}, nil
}
func (f *fakeStore) GetDomainStats(int64, string) (int, int, error) { return 7, 6, nil }

func offline() *parser.Analyzer {
	cfg := parser.DefaultAnalyzerConfig()
	cfg.CheckSPF, cfg.CheckDKIM, cfg.CheckRBL = false, false, false
	return parser.NewAnalyzer(cfg)
}

// deliver stores the message the way the MX path does: the analyzer's
// score/status/reasons as computed with the user's configuration.
func deliver(t *testing.T, raw string, disabled map[string]bool, weights map[string]float64) (*fakeStore, *parser.ParsedMessage) {
	t.Helper()
	parsed, err := parser.New().ParseBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	offline().AnalyzeWithUserConfig(parsed, "", "", disabled, weights)
	msg := &models.Message{
		ID: 11, UserID: userID, MessageID: "<m1@example>", From: "Sberbank Bonus <noreply@promo-sberr.example>",
		Subject: parsed.Subject, SpamScore: parsed.SpamScore, SpamStatus: string(parsed.SpamStatus),
		SpamReasons: parser.GetSpamReasonsJSON(parsed.SpamReasons),
	}
	store := &fakeStore{
		msg: msg, raw: []byte(raw), state: db.MessageSpamState{IsSpam: parsed.SpamStatus == parser.SpamStatusSpam},
		disabled: disabled, weights: weights,
	}
	return store, parsed
}

// TestExplanationMatchesDeliveryVerdict is the point of the package: the
// explanation shows the verdict the message was filtered by, and the rules
// that produced it — with the very scores the delivery analyzer gave them.
func TestExplanationMatchesDeliveryVerdict(t *testing.T) {
	configs := []struct {
		name     string
		disabled map[string]bool
		weights  map[string]float64
	}{
		{"stock", nil, nil},
		{"user weights", map[string]bool{"emojis": true}, map[string]float64{"content": 2, "headers": 0.5}},
		{"opt-in heuristics on", nil, map[string]float64{"link_heuristics": 1, "subject_heuristics": 1, "sender_heuristics": 1}},
	}
	for _, c := range configs {
		t.Run(c.name, func(t *testing.T) {
			store, delivered := deliver(t, promoRaw, c.disabled, c.weights)
			if len(delivered.SpamFindings) < 5 || delivered.SpamStatus != parser.SpamStatusSpam {
				t.Fatalf("fixture too weak to prove anything: %+v", delivered.SpamFindings)
			}
			e, err := NewExplainer(store, offline()).ExplainByMessageID(userID, "<m1@example>")
			if err != nil {
				t.Fatal(err)
			}
			if e.Score != delivered.SpamScore || e.Status != string(delivered.SpamStatus) {
				t.Fatalf("explained %v/%s, delivered %v/%s", e.Score, e.Status, delivered.SpamScore, delivered.SpamStatus)
			}
			if e.IsSpam != (delivered.SpamStatus == parser.SpamStatusSpam) {
				t.Fatal("is_spam not taken from the stored verdict")
			}
			if len(e.Findings) != len(delivered.SpamFindings) {
				t.Fatalf("%d findings explained, %d fired at delivery", len(e.Findings), len(delivered.SpamFindings))
			}
			var sum float64
			for i, f := range e.Findings {
				want := delivered.SpamFindings[i]
				if !f.Scored || f.Reason != want.Reason || f.Check != want.Check || f.Score != want.Score {
					t.Errorf("finding %d: explained %+v, delivered %+v", i, f, want)
				}
				sum += f.Score
			}
			if sum != e.Score {
				t.Errorf("explained findings sum to %v, verdict score %v", sum, e.Score)
			}
			if e.Recheck.Differs || e.Recheck.Score != e.Score || e.Recheck.Status != delivered.SpamStatus {
				t.Errorf("re-run disagrees with an unchanged configuration: %+v", e.Recheck)
			}
		})
	}
}

func TestExplanationKeepsStoredVerdictWhenSettingsChanged(t *testing.T) {
	store, delivered := deliver(t, promoRaw, nil, nil)
	store.weights = map[string]float64{"content": 0} // user changed settings after delivery
	e, err := NewExplainer(store, offline()).ExplainByMessageID(userID, "<m1@example>")
	if err != nil {
		t.Fatal(err)
	}
	if e.Score != delivered.SpamScore || len(e.Findings) != len(delivered.SpamReasons) {
		t.Fatal("explanation followed today's settings instead of the stored verdict")
	}
	if !e.Recheck.Differs {
		t.Fatal("changed re-run not flagged")
	}
	for _, f := range e.Findings {
		if f.Check == "content" && f.Scored {
			t.Errorf("content finding %q scored by a re-run that no longer fires it", f.Reason)
		}
	}
}

func TestExplanationNetworkAndForeignReasons(t *testing.T) {
	store, _ := deliver(t, promoRaw, nil, nil)
	store.msg.SpamReasons = `["SPF fail: not permitted","RBL listed: zen","recipient mismatch: account address not in To/Cc","spam word: act now"]`
	e, err := NewExplainer(store, offline()).ExplainByMessageID(userID, "<m1@example>")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		check  string
		scored bool
	}{{"spf", false}, {"rbl", false}, {"recipient", false}, {"content", true}}
	for i, w := range want {
		if e.Findings[i].Check != w.check || e.Findings[i].Scored != w.scored {
			t.Errorf("finding %d = %+v, want check %s scored %v", i, e.Findings[i], w.check, w.scored)
		}
	}
}

func TestExplanationBlacklistRule(t *testing.T) {
	store, _ := deliver(t, promoRaw, nil, nil)
	ruleID := int64(9)
	store.msg.SpamScore, store.msg.SpamReasons, store.msg.SpamStatus = 0, "", "clean"
	store.state = db.MessageSpamState{IsSpam: true, RuleID: &ruleID}
	store.rules = map[int64]*db.SpamRule{ruleID: {ID: ruleID, RuleType: "domain", RuleValue: "promo-sberr.example", Action: "spam"}}
	e, err := NewExplainer(store, offline()).ExplainByMessageID(userID, "<m1@example>")
	if err != nil {
		t.Fatal(err)
	}
	if !e.IsSpam || !e.DecidedByRule() || e.Rule.RuleValue != "promo-sberr.example" {
		t.Fatalf("blacklist verdict not explained: %+v", e)
	}
	if e.Recheck.Differs {
		t.Error("re-run compared against a verdict the analyzer never made")
	}
}

func TestExplanationPartialWhitelistAppliesExclusions(t *testing.T) {
	store, _ := deliver(t, promoRaw, nil, nil)
	store.action = "allow"
	store.matched = &db.SpamRule{ID: 3, Action: "allow", ExcludedChecks: []string{"content", "headers"}}
	e, err := NewExplainer(store, offline()).ExplainByMessageID(userID, "<m1@example>")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range e.Recheck.Findings {
		if f.Check == "content" || f.Check == "headers" {
			t.Errorf("excluded check %s ran: %q", f.Check, f.Reason)
		}
	}
}

func TestExplanationLegacyMessageWithoutRaw(t *testing.T) {
	store, _ := deliver(t, promoRaw, nil, nil)
	store.raw = nil
	store.msg.Body = "click here"
	e, err := NewExplainer(store, offline()).ExplainByMessageID(userID, "<m1@example>")
	if err != nil {
		t.Fatal(err)
	}
	if e.FromEmail != "noreply@promo-sberr.example" || e.Domain != "promo-sberr.example" || e.FromName != "Sberbank Bonus" {
		t.Fatalf("sender not rebuilt from stored fields: %q %q %q", e.FromEmail, e.Domain, e.FromName)
	}
	if e.DomainTotal != 7 || e.DomainSpam != 6 || e.SenderStats.TotalMessages != 3 {
		t.Fatal("history not attached")
	}
}

func TestExplanationLinks(t *testing.T) {
	store, _ := deliver(t, promoRaw, nil, nil)
	e, err := NewExplainer(store, nil).ExplainByMessageID(userID, "<m1@example>")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Links.URLs) != 2 || len(e.Links.Shorteners) != 2 {
		t.Fatalf("links = %+v", e.Links)
	}
}

func TestExplanationNotFound(t *testing.T) {
	store, _ := deliver(t, promoRaw, nil, nil)
	_, err := NewExplainer(store, offline()).ExplainByMessageID(userID+1, "<m1@example>")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoredReasonsNonJSONKept(t *testing.T) {
	if got := storedReasons("legacy text"); len(got) != 1 || got[0] != "legacy text" {
		t.Fatalf("got %q", got)
	}
	if got := storedReasons(""); got != nil {
		t.Fatalf("got %q", got)
	}
}
