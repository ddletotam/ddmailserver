package client

import "testing"

// TestUsesImplicitTLS pins the port split that decides how the connection is
// opened. Getting this backwards does not produce an error message — it hangs
// until the dial times out, which is why it is worth a test of its own.
func TestUsesImplicitTLS(t *testing.T) {
	cases := map[int]bool{
		465:  true,  // RFC 8314 submissions, what device profiles ask for
		587:  false, // submission with STARTTLS
		25:   false, // relay
		2525: false, // common alternate relay port
		993:  false, // IMAP, never an SMTP target
		0:    false, // unset
	}

	for port, want := range cases {
		if got := usesImplicitTLS(port); got != want {
			t.Errorf("usesImplicitTLS(%d) = %v, want %v", port, got, want)
		}
	}
}
