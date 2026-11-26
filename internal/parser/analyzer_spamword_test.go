package parser

import "testing"

// Spam words are stems matched from the start of a word: a hit inside another
// word was a false positive ("Проверка доставки" scored as "ставки", 2026-11-26).
func TestContainsAtWordStart(t *testing.T) {
	cases := []struct {
		content, word string
		want          bool
	}{
		{"проверка доставки lunlun.email", "ставки", false},
		{"лучшие ставки на спорт", "ставки", true},
		{"ставки", "ставки", true},
		{"аккредитация пройдена", "кредит", false},
		{"кредит без справок", "кредит", true},
		{"абсолютно бесплатно", "бесплатн", true},
		{"(бесплатно)", "бесплатн", true},
		{"доставки и ставки", "ставки", true}, // second occurrence is a word
		{"servip account", "vip", false},
		{"vip-доступ", "vip", true},
		{"click here now", "click here", true},
		{"doubleclick here", "click here", false},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := containsAtWordStart(c.content, c.word); got != c.want {
			t.Errorf("containsAtWordStart(%q, %q) = %v, want %v", c.content, c.word, got, c.want)
		}
	}
}
