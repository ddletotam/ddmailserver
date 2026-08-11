package caldav

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscapeText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{name: "nothing to do", in: "простая встреча", want: "простая встреча"},
		{name: "comma", in: "г. Ярославль, пр. Октября, 59", want: `г. Ярославль\, пр. Октября\, 59`},
		{name: "semicolon", in: "a;b", want: `a\;b`},
		{name: "backslash", in: `path\to`, want: `path\\to`},
		{name: "lf becomes an escape", in: "первая\nвторая", want: `первая\nвторая`},
		{name: "crlf is one break", in: "первая\r\nвторая", want: `первая\nвторая`},
		{name: "bare cr is one break", in: "первая\rвторая", want: `первая\nвторая`},
		{name: "backslash first", in: `a\,b`, want: `a\\\,b`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EscapeText(c.in); got != c.want {
				t.Errorf("EscapeText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestFoldLine_StaysWithinTheOctetLimit checks the property that matters: no
// fragment exceeds 75 octets. The limit is in octets, which is the whole point
// for Cyrillic — two bytes per letter means a line looks half as long as it is.
func TestFoldLine_StaysWithinTheOctetLimit(t *testing.T) {
	inputs := []string{
		"DESCRIPTION:" + strings.Repeat("я", 200),
		"DESCRIPTION:" + strings.Repeat("a", 200),
		"SUMMARY:короткая",
		"DESCRIPTION:Номер вашей записи: 8788086084\\nг. Ярославль\\, пр. Октября\\, 59 тел. 8 (4852) 66-41-14\\nРегистратура 3 (платный мед. осмотр)",
	}

	for _, in := range inputs {
		folded := FoldLine(in)

		for i, fragment := range strings.Split(folded, "\r\n") {
			if len(fragment) > maxOctets {
				t.Errorf("fragment %d is %d octets (limit %d):\n%q", i, len(fragment), maxOctets, fragment)
			}
			if i > 0 && !strings.HasPrefix(fragment, " ") {
				t.Errorf("continuation %d does not start with a space: %q", i, fragment)
			}
		}

		// Unfolding must give back exactly what went in.
		unfolded := strings.ReplaceAll(folded, "\r\n ", "")
		if unfolded != in {
			t.Errorf("unfolding did not round-trip:\nwant %q\ngot  %q", in, unfolded)
		}

		if !utf8.ValidString(folded) {
			t.Errorf("folding split a UTF-8 sequence:\n%q", folded)
		}
	}
}

func TestFoldLine_ShortLineUntouched(t *testing.T) {
	line := "SUMMARY:тест"
	if got := FoldLine(line); got != line {
		t.Errorf("FoldLine(%q) = %q, want it unchanged", line, got)
	}
}

// TestContentLine_RebuildsTheRejectedDescription puts the two halves together on
// the value that iCloud refused: a Gosuslugi appointment description, long and
// multi-line, with commas in the address.
func TestContentLine_RebuildsTheRejectedDescription(t *testing.T) {
	description := "Номер вашей записи: 8788086084\r\nг. Ярославль, пр. Октября, 59    тел. 8 (4852) 66-41-14\r\nРегистратура 3 (платный мед. осмотр)"

	line := ContentLine("DESCRIPTION", description)

	if !strings.HasSuffix(line, "\r\n") {
		t.Error("content line does not end with CRLF")
	}

	// No raw line break may survive inside the value: that is what terminated
	// the content line early and made the whole object invalid.
	body := strings.TrimSuffix(line, "\r\n")
	for i, fragment := range strings.Split(body, "\r\n") {
		if i > 0 && !strings.HasPrefix(fragment, " ") {
			t.Errorf("fragment %d is not a fold continuation, so a raw break leaked through: %q", i, fragment)
		}
		if len(fragment) > maxOctets {
			t.Errorf("fragment %d is %d octets, over the limit: %q", i, len(fragment), fragment)
		}
	}

	unfolded := strings.ReplaceAll(body, "\r\n ", "")
	if strings.Count(unfolded, `\n`) != 2 {
		t.Errorf("expected both line breaks as \\n escapes, got: %q", unfolded)
	}
	if !strings.Contains(unfolded, `Ярославль\, пр. Октября\, 59`) {
		t.Errorf("commas were not escaped: %q", unfolded)
	}
}

// TestRawContentLine_DoesNotTouchRRULE: a recurrence rule spells its syntax with
// semicolons and commas. Escaping those would corrupt the rule.
func TestRawContentLine_DoesNotTouchRRULE(t *testing.T) {
	rule := "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE,FR"

	got := RawContentLine("RRULE", rule)
	want := "RRULE:" + rule + "\r\n"

	if got != want {
		t.Errorf("RawContentLine mangled the rule:\ngot  %q\nwant %q", got, want)
	}
}
