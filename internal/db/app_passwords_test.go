package db

import (
	"strings"
	"testing"
)

// TestGenerateAppPasswordShape pins the display form. Clients and humans copy
// these by hand, so the grouping is part of the contract, not decoration.
func TestGenerateAppPasswordShape(t *testing.T) {
	secret, err := GenerateAppPassword()
	if err != nil {
		t.Fatalf("GenerateAppPassword: %v", err)
	}

	if len(secret) != 19 { // 16 letters + 3 dashes
		t.Errorf("len(%q) = %d, want 19", secret, len(secret))
	}

	groups := strings.Split(secret, "-")
	if len(groups) != 4 {
		t.Fatalf("got %d groups in %q, want 4", len(groups), secret)
	}
	for i, g := range groups {
		if len(g) != 4 {
			t.Errorf("group %d = %q, want 4 chars", i, g)
		}
		for _, r := range g {
			if r < 'a' || r > 'z' {
				t.Errorf("group %d = %q contains non-lowercase-letter %q", i, g, r)
			}
		}
	}
}

// TestGenerateAppPasswordIsRandom is a smoke test against a generator that
// returns a constant or reuses a seed — the failure mode that would hand every
// device the same credential.
func TestGenerateAppPasswordIsRandom(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		secret, err := GenerateAppPassword()
		if err != nil {
			t.Fatalf("GenerateAppPassword: %v", err)
		}
		if seen[secret] {
			t.Fatalf("duplicate secret after %d draws: %q", i, secret)
		}
		seen[secret] = true
	}
}

// TestNormalizeAppPassword covers the forms a secret comes back in: as issued,
// with the dashes stripped, uppercased by a phone keyboard, or padded with
// spaces by a copy-paste. All must hash to the same credential.
func TestNormalizeAppPassword(t *testing.T) {
	const want = "abcdefghijklmnop"

	cases := map[string]string{
		"as issued":     "abcd-efgh-ijkl-mnop",
		"no dashes":     "abcdefghijklmnop",
		"uppercased":    "ABCD-EFGH-IJKL-MNOP",
		"mixed case":    "AbCd-EfGh-IjKl-MnOp",
		"padded":        "  abcd-efgh-ijkl-mnop  ",
		"spaced groups": "abcd efgh ijkl mnop",
	}

	for name, in := range cases {
		if got := NormalizeAppPassword(in); got != want {
			t.Errorf("%s: NormalizeAppPassword(%q) = %q, want %q", name, in, got, want)
		}
	}
}

// TestHashAppPasswordStableAcrossForms is the property that matters: the two
// forms a user is likely to present must reach the same stored hash.
func TestHashAppPasswordStableAcrossForms(t *testing.T) {
	grouped := hashAppPassword("abcd-efgh-ijkl-mnop")
	bare := hashAppPassword("abcdefghijklmnop")

	if grouped != bare {
		t.Errorf("grouped %s != bare %s", grouped, bare)
	}
	if len(grouped) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(grouped))
	}
	if grouped == hashAppPassword("abcd-efgh-ijkl-mnoq") {
		t.Error("different secrets hashed to the same value")
	}
}

// TestNormalizeAppPasswordRejectsLength guards the cheap pre-check in
// VerifyAppPassword: anything that is not exactly 16 letters after
// normalisation must not reach the database at all.
func TestNormalizeAppPasswordRejectsLength(t *testing.T) {
	tooShort := []string{"", "   ", "abc", "abcd-efgh-ijkl-mno", "1234-5678-9012-3456"}
	for _, in := range tooShort {
		if got := NormalizeAppPassword(in); len(got) == appPasswordLen {
			t.Errorf("NormalizeAppPassword(%q) = %q, unexpectedly %d chars", in, got, appPasswordLen)
		}
	}

	if got := NormalizeAppPassword("abcd-efgh-ijkl-mnop"); len(got) != appPasswordLen {
		t.Errorf("valid secret normalised to %d chars, want %d", len(got), appPasswordLen)
	}
}
