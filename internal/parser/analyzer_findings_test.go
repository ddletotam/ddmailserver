package parser

import (
	"math"
	"strings"
	"testing"
)

// Findings are the explanation of the verdict, so they must add up to it
// exactly: same rules, same order, same total.
func TestFindingsAddUpToVerdict(t *testing.T) {
	valid := map[string]bool{}
	for _, c := range SpamCheckCategories {
		valid[c] = true
	}
	allOn := map[string]float64{"subject_heuristics": 1, "sender_heuristics": 1.5, "link_heuristics": 1, "content": 2}
	for name, raw := range analyzerCorpus() {
		for _, w := range []map[string]float64{nil, allOn} {
			msg := parseCorpus(t, raw)
			offlineAnalyzer().AnalyzeWithUserConfig(msg, "", "", nil, w)
			if len(msg.SpamFindings) != len(msg.SpamReasons) {
				t.Fatalf("%s: %d findings for %d reasons", name, len(msg.SpamFindings), len(msg.SpamReasons))
			}
			var sum float64
			for i, f := range msg.SpamFindings {
				if f.Reason != msg.SpamReasons[i] {
					t.Errorf("%s: finding %d reason %q != stored reason %q", name, i, f.Reason, msg.SpamReasons[i])
				}
				if !valid[f.Check] {
					t.Errorf("%s: finding %q has unknown category %q", name, f.Reason, f.Check)
				}
				sum += f.Score
			}
			if math.Abs(sum-msg.SpamScore) > 1e-9 {
				t.Errorf("%s: findings sum to %v, score is %v", name, sum, msg.SpamScore)
			}
		}
	}
}

func TestOptInHeuristicsAreOffByDefault(t *testing.T) {
	for _, c := range []string{"subject_heuristics", "sender_heuristics", "link_heuristics"} {
		if DefaultCategoryWeight(c) != 0 {
			t.Errorf("%s is on by default — it would re-classify existing mail", c)
		}
	}
	if DefaultCategoryWeight("content") != 1 {
		t.Error("stock category lost its default weight")
	}
	msg := parseCorpus(t, analyzerCorpus()["phish"])
	offlineAnalyzer().AnalyzeWithUserConfig(msg, "", "", nil, map[string]float64{"links": 1})
	for _, f := range msg.SpamFindings {
		if strings.HasSuffix(f.Check, "_heuristics") {
			t.Errorf("opt-in rule fired without a weight: %+v", f)
		}
	}
}

func checksFired(msg *ParsedMessage, check string) []string {
	var out []string
	for _, f := range msg.SpamFindings {
		if f.Check == check {
			out = append(out, f.Reason)
		}
	}
	return out
}

func TestOptInHeuristicsFireWhenWeighted(t *testing.T) {
	on := map[string]float64{"subject_heuristics": 1, "sender_heuristics": 1, "link_heuristics": 1}

	phish := parseCorpus(t, analyzerCorpus()["phish"])
	offlineAnalyzer().AnalyzeWithUserConfig(phish, "", "", nil, on)
	wantPhish := map[string][]string{
		"subject_heuristics": {"reply/forward subject without In-Reply-To"},
		"sender_heuristics":  {`brand-like name "PayPal Support" from free mail provider gmail.com`},
		"link_heuristics": {
			"regional URL shortener(s): 1",
			"login/verify links to foreign domains: 1",
			"tracking links with encoded parameters: 2",
			"links to random-looking domains: 2", // paypa1-secure + rusege-oleneva
		},
	}
	for check, want := range wantPhish {
		if got := checksFired(phish, check); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("phish %s: got %q, want %q", check, got, want)
		}
	}

	scam := parseCorpus(t, analyzerCorpus()["scam-sender"])
	offlineAnalyzer().AnalyzeWithUserConfig(scam, "", "", nil, on)
	wantScam := []string{"discount percentage in subject", "urgency: срочно"}
	if got := checksFired(scam, "subject_heuristics"); strings.Join(got, "|") != strings.Join(wantScam, "|") {
		t.Errorf("scam subject: got %q, want %q", got, wantScam)
	}
}

func TestSubjectHeuristicsDetails(t *testing.T) {
	a := offlineAnalyzer()
	cases := map[string][]string{
		"":                      {"empty subject"},
		"СРОЧНО ПРОЧТИТЕ ЭТО":   {"excessive caps in subject (Cyrillic)", "urgency: срочно"},
		"Fwd: hello":            {"reply/forward subject without In-Reply-To"},
		"Обычная тема письма":   nil,
		"Weekly digest, 5 news": nil,
	}
	for subject, want := range cases {
		got := a.analyzeSubjectHeuristics(&ParsedMessage{Subject: subject})
		var reasons []string
		for _, h := range got {
			reasons = append(reasons, h.reason)
		}
		if strings.Join(reasons, "|") != strings.Join(want, "|") {
			t.Errorf("%q: got %q, want %q", subject, reasons, want)
		}
	}
}

func TestSenderHeuristicsDisplayNameAddress(t *testing.T) {
	msg := parseCorpus(t, crlf(`
From: "support@bank.example" <x@random.example>
Subject: hi

body
`))
	hits := offlineAnalyzer().analyzeSenderHeuristics(msg)
	if len(hits) != 1 || hits[0].reason != "display name contains another address: support@bank.example" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestLinkHeuristicsBrandMismatch(t *testing.T) {
	msg := parseCorpus(t, crlf(`
From: Ozon <news@ozon-promo.example>
Subject: hi

Visit https://ozon.ru/a and https://elsewhere.example/b
`))
	hits := offlineAnalyzer().analyzeLinkHeuristics(msg)
	if len(hits) != 1 || hits[0].reason != `links not matching the claimed brand "ozon": 1` || hits[0].score != 2.0 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestLinksReport(t *testing.T) {
	msg := parseCorpus(t, analyzerCorpus()["phish"])
	rep := offlineAnalyzer().Links(msg)
	if len(rep.URLs) != 13 {
		t.Errorf("urls = %d, want 13", len(rep.URLs))
	}
	if len(rep.Shorteners) != 3 {
		t.Errorf("shorteners = %q, want bit.ly ×2 + vk.cc", rep.Shorteners)
	}
	if len(rep.Suspicious) != 1 || !strings.Contains(rep.Suspicious[0], "/login") {
		t.Errorf("suspicious = %q", rep.Suspicious)
	}
}

func TestVerdictThresholds(t *testing.T) {
	a := offlineAnalyzer()
	for score, want := range map[float64]SpamStatus{0: SpamStatusClean, 2.99: SpamStatusClean, 3: SpamStatusSuspicious, 6: SpamStatusSpam} {
		if got := a.Verdict(score); got != want {
			t.Errorf("Verdict(%v) = %s, want %s", score, got, want)
		}
	}
}

func TestIsRandomDomain(t *testing.T) {
	for d, want := range map[string]bool{
		"rusege-oleneva.ru":      true,
		"example.org":            false,
		"web-shop.example":       false,
		"xkcdqwrtzplmnabvc.test": true,
		"localhost":              false,
	} {
		if got := isRandomDomain(d); got != want {
			t.Errorf("isRandomDomain(%q) = %v, want %v", d, got, want)
		}
	}
}
