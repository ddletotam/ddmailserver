package parser

import (
	"reflect"
	"testing"
)

type goldenVerdict struct {
	score   float64
	status  SpamStatus
	reasons []string
}

// TestAnalyzerGolden pins the delivery verdict for the corpus. It was recorded
// before the analyzer started emitting structured findings and before the
// opt-in heuristics were ported from the web "why spam" engine: neither change
// may move a single score, status or reason of an existing user (opt-in
// categories have default weight 0).
func TestAnalyzerGolden(t *testing.T) {
	weights := map[string]float64{"content": 2, "links": 0, "chain": 0.5}
	disabled := map[string]bool{"spam_word:скидка": true}
	phishStock := []string{"only one Received hop", "HELO doesn't match reverse DNS", "no reverse DNS for sending IP",
		"suspicious sending MTA name: auth-xkfg-7.relay.example", "dangerous attachment: invoice.pdf.exe",
		"double extension: invoice.pdf.exe", "suspicious domain: paypa1-secure.example", "excessive links (>10)",
		"contains URL shortener(s)", "brand impersonation: \"PayPal Support\" from gmail.com"}
	phishWeighted := []string{"only one Received hop", "HELO doesn't match reverse DNS", "no reverse DNS for sending IP",
		"suspicious sending MTA name: auth-xkfg-7.relay.example", "dangerous attachment: invoice.pdf.exe",
		"double extension: invoice.pdf.exe", "brand impersonation: \"PayPal Support\" from gmail.com"}
	promo := []string{"no Received headers", "From domain differs from Reply-To domain", "missing Message-ID header",
		"missing Date header", "suspicious mail client: phpmailer", "spam word: winner", "spam word: free money",
		"spam word: act now", "spam word: click here", "spam word: получи", "spam word: заработ",
		"HTML-only message (no plain text)", "brand impersonation: \"Сбербанк\" from promo-sberr.example", "emojis in subject"}
	scam := []string{"Received chain timestamps inconsistent", "spam word: только сегодня", "spam word: последний шанс",
		"scam-like sender: \"Лаборатория дохода\""}

	golden := map[string][2]goldenVerdict{
		"clean":       {{0, SpamStatusClean, nil}, {0, SpamStatusClean, nil}},
		"embedded":    {{1.5, SpamStatusClean, []string{"only one Received hop"}}, {0.75, SpamStatusClean, []string{"only one Received hop"}}},
		"phish":       {{24, SpamStatusSpam, phishStock}, {16.5, SpamStatusSpam, phishWeighted}},
		"promo":       {{16.5, SpamStatusSpam, promo}, {18, SpamStatusSpam, promo}},
		"scam-sender": {{5.5, SpamStatusSuspicious, scam}, {5.75, SpamStatusSuspicious, scam}},
	}

	corpus := analyzerCorpus()
	for name, want := range golden {
		for i, w := range []map[string]float64{nil, weights} {
			msg := parseCorpus(t, corpus[name])
			offlineAnalyzer().AnalyzeWithUserConfig(msg, "", "", disabled, w)
			got := goldenVerdict{msg.SpamScore, msg.SpamStatus, msg.SpamReasons}
			if !reflect.DeepEqual(got, want[i]) {
				t.Errorf("%s (weighted=%v):\n got  %+v\n want %+v", name, i == 1, got, want[i])
			}
		}
	}
}
