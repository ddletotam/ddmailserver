package worker

import (
	"testing"

	"github.com/yourusername/mailserver/internal/models"
)

// TestComponentOf reads the component out of a queued body.
//
// It reads the body rather than the event row deliberately: by retry time the
// row may be gone. That is not hypothetical — an inbound sync that could not
// recognise a VTODO deleted three of them while their queue entries kept
// retrying, so the body was the only remaining evidence of what would be sent.
func TestComponentOf(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"iOS reminder": {
			body: "BEGIN:VCALENDAR\r\nBEGIN:VTODO\r\nUID:x\r\nEND:VTODO\r\nEND:VCALENDAR\r\n",
			want: models.ComponentTodo,
		},
		"ordinary event": {
			body: "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
			want: models.ComponentEvent,
		},
		"both present reads as an event": {
			body: "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nEND:VEVENT\r\nBEGIN:VTODO\r\nUID:b\r\nEND:VTODO\r\nEND:VCALENDAR\r\n",
			want: models.ComponentEvent,
		},
		"empty body defaults to event": {
			body: "",
			want: models.ComponentEvent,
		},
	}

	for name, c := range cases {
		if got := componentOf(c.body); got != c.want {
			t.Errorf("%s: componentOf = %q, want %q", name, got, c.want)
		}
	}
}

// TestPermanentSyncErrorClassification pins which statuses are worth retrying.
// 403 has to be permanent: iCloud answers it to a task in an event collection,
// and no amount of backoff turns that into an accepted PUT.
func TestPermanentSyncErrorClassification(t *testing.T) {
	permanent := []string{
		"PUT https://x/y.ics failed with status 403: ",
		"PUT failed with status 400: bad request",
		"PUT failed with status 404: ",
		"PUT failed with status 412: ",
	}
	for _, msg := range permanent {
		if !isPermanentSyncError(errString(msg)) {
			t.Errorf("%q should be permanent", msg)
		}
	}

	// These heal on their own: re-auth, backoff, or the remote recovering.
	transient := []string{
		"PUT failed with status 401: ",
		"PUT failed with status 429: ",
		"PUT failed with status 500: ",
		"PUT failed with status 503: ",
		"dial tcp: connection timed out",
	}
	for _, msg := range transient {
		if isPermanentSyncError(errString(msg)) {
			t.Errorf("%q should be retried, not retired", msg)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
