package web

import (
	"testing"

	"github.com/ddletotam/ddmailserver/internal/parser"
)

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
