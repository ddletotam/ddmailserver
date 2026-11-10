package client

import (
	"testing"
	"time"

	"github.com/emersion/go-imap"
	imapClient "github.com/emersion/go-imap/client"
)

func TestByeReason(t *testing.T) {
	ch := make(chan imapClient.Update, 4)
	ch <- &imapClient.MailboxUpdate{}
	ch <- &imapClient.StatusUpdate{Status: &imap.StatusResp{Tag: "*", Type: imap.StatusRespBye, Code: "UNAVAILABLE", Info: "Too many connections"}}
	if got := byeReason(ch, 100*time.Millisecond); got != "[UNAVAILABLE] Too many connections" {
		t.Fatalf("got %q", got)
	}
	if got := byeReason(ch, 10*time.Millisecond); got != "" {
		t.Fatalf("empty channel: got %q", got)
	}
}
