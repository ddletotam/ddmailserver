package parser

import "testing"

func TestDecodeMIMEHeader(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Malformed: 3 trailing '=' on base64 — stdlib rejects, lenient
		// path strips & re-pads.
		{
			in:   "=?windows-1251?B?yuLo8uDt9uj/IOfgIDA0LjIwMjbjLiDv7iDL0SAxMDAxMTkzNA===?=",
			want: "Квитанция за 04.2026г. по ЛС 10011934",
		},
		// Standard happy-path: well-formed UTF-8 base64.
		{
			in:   "=?UTF-8?B?0J/RgNC40LLQtdGC?=",
			want: "Привет",
		},
		// Plain text passes through.
		{in: "Just a plain subject", want: "Just a plain subject"},
		{in: "", want: ""},
	}
	for _, c := range cases {
		got := DecodeMIMEHeader(c.in)
		if got != c.want {
			t.Errorf("DecodeMIMEHeader(%q):\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}
