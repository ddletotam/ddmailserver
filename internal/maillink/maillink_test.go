package maillink

import (
	"testing"
	"time"
)

func TestTicket(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	tok := Sign(secret, 42, now)
	v := NewVerifier(secret)
	if u, err := v.Verify(tok, now); err != nil || u != 42 {
		t.Fatalf("Verify = %d, %v", u, err)
	}
	if _, err := v.Verify(tok, now); err == nil {
		t.Fatal("a ticket was accepted twice")
	}
	if _, err := NewVerifier(secret).Verify(Sign(secret, 1, now), now.Add(2*TicketTTL)); err == nil {
		t.Fatal("an expired ticket was accepted")
	}
	if _, err := NewVerifier([]byte("other-secret-other-secret-other!")).Verify(Sign(secret, 1, now), now); err == nil {
		t.Fatal("a ticket signed with another secret was accepted")
	}
	if _, err := v.Verify(tok[:len(tok)-2]+"xx", now); err == nil {
		t.Fatal("a forged signature was accepted")
	}
}
