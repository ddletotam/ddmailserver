package parser

import (
	"net/mail"
	"testing"

	"github.com/emersion/go-msgauth/dmarc"
)

func dmarcResolver() *fakeResolver {
	r := newFakeResolver()
	r.txt["_dmarc.bank.example"] = []string{"v=DMARC1; p=reject"}
	r.txt["_dmarc.strict.example"] = []string{"v=DMARC1; p=quarantine; adkim=s; aspf=s"}
	r.txt["_dmarc.parent.example"] = []string{"v=DMARC1; p=none; sp=reject"}
	r.txt["_dmarc.flaky.example"] = nil
	r.fail["_dmarc.flaky.example"] = true
	return r
}

func TestDMARCEvaluate(t *testing.T) {
	c := NewDMARCCheckerWithResolver(dmarcResolver())
	cases := []struct {
		name       string
		from       string
		spf        AuthResult
		spfDomain  string
		dkim       []string
		want       AuthResult
		wantPolicy dmarc.Policy
	}{
		{"aligned DKIM", "bank.example", AuthResultNone, "", []string{"bank.example"}, AuthResultPass, "reject"},
		{"relaxed DKIM subdomain", "bank.example", AuthResultNone, "", []string{"mail.bank.example"}, AuthResultPass, "reject"},
		{"aligned SPF", "bank.example", AuthResultPass, "bounce.bank.example", nil, AuthResultPass, "reject"},
		{"forged: foreign DKIM and SPF", "bank.example", AuthResultPass, "evil.example", []string{"evil.example"}, AuthResultFail, "reject"},
		{"SPF fail own domain", "bank.example", AuthResultFail, "bank.example", nil, AuthResultFail, "reject"},
		{"strict rejects subdomain DKIM", "strict.example", AuthResultNone, "", []string{"mail.strict.example"}, AuthResultFail, "quarantine"},
		{"strict exact DKIM", "strict.example", AuthResultNone, "", []string{"strict.example"}, AuthResultPass, "quarantine"},
		{"subdomain uses sp", "news.parent.example", AuthResultNone, "", nil, AuthResultFail, "reject"},
		{"no policy", "nopolicy.example", AuthResultFail, "nopolicy.example", nil, AuthResultNone, ""},
		{"DNS trouble", "flaky.example", AuthResultNone, "", nil, AuthResultTempError, ""},
		{"multi-label suffix", "shop.co.uk", AuthResultNone, "", nil, AuthResultNone, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Evaluate(tc.from, tc.spf, tc.spfDomain, tc.dkim)
			if got.Result != tc.want || got.Policy != tc.wantPolicy {
				t.Fatalf("got %s p=%s (%s), want %s p=%s", got.Result, got.Policy, got.Detail, tc.want, tc.wantPolicy)
			}
		})
	}
}

func TestOrgDomain(t *testing.T) {
	for in, want := range map[string]string{
		"mail.bank.example": "bank.example",
		"a.b.shop.co.uk":    "shop.co.uk",
		"e.mail.ru":         "mail.ru",
		"MAIL.RU.":          "mail.ru",
	} {
		if got := OrgDomain(in); got != want {
			t.Errorf("OrgDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// Cached: one DNS query per domain, not per message.
func TestDMARCCachesRecords(t *testing.T) {
	r := dmarcResolver()
	c := NewDMARCCheckerWithResolver(r)
	for i := 0; i < 3; i++ {
		c.Evaluate("bank.example", AuthResultNone, "", nil)
	}
	if r.queries != 1 {
		t.Fatalf("queries = %d, want 1", r.queries)
	}
}

// The analyzer scores by policy and only when SPF was actually evaluated.
func TestAnalyzeDMARCScoring(t *testing.T) {
	a := &Analyzer{config: DefaultAnalyzerConfig(), dmarcChecker: NewDMARCCheckerWithResolver(dmarcResolver())}
	msg := func(from, spfDomain string, spf AuthResult) *ParsedMessage {
		return &ParsedMessage{
			From:        &mail.Address{Address: "info@" + from},
			AuthResults: &AuthResults{SPF: spf, SPFDomain: spfDomain, DKIMChecked: true},
		}
	}
	m := msg("bank.example", "evil.example", AuthResultPass)
	if got := a.analyzeDMARC(m).total(); got != 6.0 || m.AuthResults.DMARC != AuthResultFail {
		t.Fatalf("p=reject forgery: score %v, DMARC %s", got, m.AuthResults.DMARC)
	}
	m = msg("strict.example", "evil.example", AuthResultPass)
	if got := a.analyzeDMARC(m).total(); got != 4.0 {
		t.Fatalf("p=quarantine forgery: score %v", got)
	}
	// SPF not evaluated (no sender IP): absence of aligned DKIM proves nothing.
	m = msg("bank.example", "", AuthResultNone)
	if got := a.analyzeDMARC(m).total(); got != 0 || m.AuthResults.DMARC != AuthResultNone {
		t.Fatalf("SPF not evaluated: score %v, DMARC %s", got, m.AuthResults.DMARC)
	}
}
