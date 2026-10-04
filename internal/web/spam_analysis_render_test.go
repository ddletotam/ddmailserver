package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/ddletotam/ddmailserver/internal/db"
	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/parser"
	spamsvc "github.com/ddletotam/ddmailserver/internal/service/spam"
)

func renderSpamAnalysis(t *testing.T, view SpamAnalysisView) string {
	t.Helper()
	s := &Server{i18nManager: NewI18nManager()}
	tmpl, err := template.New("").Funcs(s.buildFuncMap(view)).ParseFS(templatesFS, "templates/spam_analysis.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "spam-analysis", view); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}

// The page shows the stored verdict and the delivery findings — the numbers
// the message was actually filtered by.
func TestSpamAnalysisRendersStoredVerdict(t *testing.T) {
	user := &models.User{ID: 1, Language: "ru"}
	e := &spamsvc.Explanation{
		Message:   &models.Message{Subject: "Тема", From: "X <x@spam.example>", Date: 1700000000000},
		FromEmail: "x@spam.example", Domain: "spam.example",
		IsSpam: true, Status: "spam", Score: 7.5,
		Findings: []spamsvc.Finding{
			{Check: "sender", Score: 5, Scored: true, Reason: `brand impersonation: "Sber" from spam.example`},
			{Check: "spf", Reason: "SPF fail: not permitted"},
			{Check: "content", Score: 2.5, Scored: true, Reason: "spam word: act now"},
		},
		Recheck: spamsvc.Recheck{Differs: true, Score: 2.5, Status: parser.SpamStatusClean,
			Findings: []parser.SpamFinding{{Check: "content", Score: 2.5, Reason: "spam word: act now"}}},
		Links:       parser.LinkReport{URLs: []string{"http://bit.ly/x"}, Shorteners: []string{"http://bit.ly/x"}},
		SenderStats: &db.SenderStats{TotalMessages: 2, SpamMessages: 2},
	}
	i18n := NewI18nManager().Get("ru")
	out := renderSpamAnalysis(t, SpamAnalysisView{User: user, E: e, MessageID: `<a"b@example>`, CheckLabels: spamCheckLabels(i18n)})

	for _, want := range []string{
		"7.5", "Отфильтровано как спам",
		"brand impersonation", "SPF fail: not permitted",
		i18n.T("spam.check.sender"), i18n.T("spam.check.spf"),
		i18n.T("spam.analyze.recheck_differs"),
		i18n.T("spam.analyze.shortener"),
	} {
		if !strings.Contains(out, template.HTMLEscapeString(want)) {
			t.Errorf("rendered page lacks %q", want)
		}
	}
	// html/template writes "+" as &#43;: the per-rule scores of the stored
	// verdict, signed; the unscored network rule shows a dash.
	for _, want := range []string{"&#43;5.0", "&#43;2.5", "—"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page lacks score %q", want)
		}
	}
	if strings.Contains(out, "ZgotmplZ") {
		t.Error("template refused to interpolate a value")
	}
	// The Message-ID with a quote must stay inside the JSON of hx-vals:
	// the quote is JSON-escaped (\") before html/template entity-encodes it.
	if !strings.Contains(out, "&#34;message_id&#34;:&#34;\\u003ca\\&#34;b@example\\u003e&#34;") {
		i := strings.Index(out, "message_id")
		t.Errorf("hx-vals not JSON-encoded: %q", out[i-10:i+60])
	}
}

func TestSpamAnalysisRendersRuleVerdict(t *testing.T) {
	e := &spamsvc.Explanation{
		Message: &models.Message{Subject: "s", From: "x@spam.example"},
		IsSpam:  true, Status: "clean",
		Rule: &db.SpamRule{RuleType: "domain", RuleValue: "spam.example", Action: "spam"},
	}
	i18n := NewI18nManager().Get("en")
	out := renderSpamAnalysis(t, SpamAnalysisView{User: &models.User{Language: "en"}, E: e, CheckLabels: spamCheckLabels(i18n)})
	for _, want := range []string{i18n.T("spam.analyze.decided_by_rule"), "domain: spam.example", i18n.T("spam.analyze.no_rules")} {
		if !strings.Contains(out, template.HTMLEscapeString(want)) {
			t.Errorf("rendered page lacks %q", want)
		}
	}
}

func TestSpamCheckLabelsCoverEveryCategory(t *testing.T) {
	en := NewI18nManager().Get("en")
	labels := spamCheckLabels(en)
	for _, c := range parser.SpamCheckCategories {
		key := "spam.check." + c
		if labels[c] == "" || labels[c] == key {
			t.Errorf("category %s has no translation", c)
		}
	}
}

func TestValidCheckNameFollowsAnalyzer(t *testing.T) {
	for _, c := range parser.SpamCheckCategories {
		if !validCheckName(c) {
			t.Errorf("analyzer category %s rejected by the settings endpoints", c)
		}
	}
	if !validCheckName("url_shortener") || validCheckName("drop table") {
		t.Error("validCheckName accepts/rejects the wrong names")
	}
}
