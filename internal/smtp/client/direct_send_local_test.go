package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/models"
	"github.com/ddletotam/ddmailserver/internal/outbox"
	"github.com/ddletotam/ddmailserver/internal/parser"
	"github.com/ddletotam/ddmailserver/internal/smtp/localdelivery"
)

// fakeOutbox records what the task does to its outbox row.
type fakeOutbox struct {
	retries   int
	status    string
	lastError string
	sent      bool
}

func (f *fakeOutbox) ClaimOutboxMessage(int64) (bool, error) { return true, nil }
func (f *fakeOutbox) UpdateOutboxMessageStatus(_ int64, status, lastError string) error {
	f.status, f.lastError = status, lastError
	return nil
}
func (f *fakeOutbox) IncrementOutboxMessageRetries(_ int64, lastError string) (int, error) {
	f.retries++
	f.lastError = lastError
	return f.retries, nil
}
func (f *fakeOutbox) MarkOutboxMessageSent(int64) error {
	f.sent, f.status = true, "sent"
	return nil
}

// fakeLocal hosts ours.test: bob (user 20), carol (user 21), alice (user 7,
// the sender). Everything it is asked to deliver is recorded.
type fakeLocal struct {
	lookupErr error
	failFor   string // Deliver fails for this address
	delivered []string
	dsns      []*parser.ParsedMessage
	dsnRaw    []string
}

func (f *fakeLocal) Lookup(email string) (*localdelivery.Recipient, error) {
	users := map[string]int64{"bob": 20, "carol": 21, "alice": 7}
	local, domain := localdelivery.SplitAddress(email)
	if domain != "ours.test" {
		return nil, localdelivery.ErrNotLocal
	}
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	uid, ok := users[local]
	if !ok {
		return nil, &localdelivery.RejectError{Code: 550, Enhanced: [3]int{5, 1, 1}, Message: "No such user here"}
	}
	return &localdelivery.Recipient{Email: local + "@" + domain, LocalPart: local, Mailbox: &models.Mailbox{UserID: uid}}, nil
}

func (f *fakeLocal) Deliver(env localdelivery.Envelope, raw []byte, parsed *parser.ParsedMessage, rcpts []*localdelivery.Recipient) ([]localdelivery.Result, []error) {
	res := make([]localdelivery.Result, len(rcpts))
	errs := make([]error, len(rcpts))
	for i, r := range rcpts {
		if strings.HasPrefix(parsed.MessageID, "dsn.") {
			f.dsns = append(f.dsns, parsed)
			f.dsnRaw = append(f.dsnRaw, string(raw))
			continue
		}
		if !env.Trusted {
			panic("submission mail must be delivered as trusted")
		}
		if r.Email == f.failFor {
			res[i], errs[i] = localdelivery.Failed, errors.New("db down")
			continue
		}
		f.delivered = append(f.delivered, r.Email)
	}
	return res, errs
}

const outMsg = "From: Alice <alice@ours.test>\r\n" +
	"To: %s\r\n" +
	"Subject: test\r\n" +
	"Message-ID: <out1@ours.test>\r\n" +
	"\r\n" +
	"hi\r\n"

// fakeTracker stands in for outbox.Service.
type fakeTracker struct {
	fresh   bool // the next Prepare reports a row accepted by an older version
	copies  int  // SaveSentCopy calls
	records []outbox.Delivery
}

func (f *fakeTracker) Prepare(msg *models.OutboxMessage) (bool, error) {
	if msg.MessageID == "" {
		msg.MessageID = "out1@ours.test"
	}
	fresh := f.fresh
	f.fresh = false
	return fresh, nil
}

func (f *fakeTracker) SaveSentCopy(*models.OutboxMessage) error {
	f.copies++
	return nil
}

func (f *fakeTracker) Record(_ *models.OutboxMessage, d outbox.Delivery) {
	f.records = append(f.records, d)
}

// last is the most recent recorded status.
func (f *fakeTracker) last() outbox.Delivery {
	if len(f.records) == 0 {
		return outbox.Delivery{}
	}
	return f.records[len(f.records)-1]
}

type harness struct {
	task     *DirectSendTask
	outbox   *fakeOutbox
	local    *fakeLocal
	tracker  *fakeTracker
	external [][]string // recipients of each SendDirectEach call
}

// newHarness builds a task for a message from alice@ours.test (user 7) to
// rcpts; extResult decides the MX outcome per external recipient.
func newHarness(rcpts string, extResult func(addr string) error) *harness {
	h := &harness{outbox: &fakeOutbox{}, local: &fakeLocal{}, tracker: &fakeTracker{}}
	msg := &models.OutboxMessage{
		ID: 1, UserID: 7, From: "Alice <alice@ours.test>", To: rcpts,
		RawEmail: []byte(fmt.Sprintf(outMsg, rcpts)),
	}
	h.task = &DirectSendTask{
		outboxMessage: msg,
		store:         h.outbox,
		hostname:      "mx.ours.test",
		local:         h.local,
		now:           func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) },
		sendExternal: func(from string, to []string, _ []byte, _ string) map[string]error {
			h.external = append(h.external, to)
			out := map[string]error{}
			for _, a := range to {
				if extResult != nil {
					out[a] = extResult(a)
				}
			}
			return out
		},
		tracker: h.tracker,
	}
	return h
}

func TestDirectSendLocalRecipientsSkipMX(t *testing.T) {
	h := newHarness("Bob <bob@ours.test>, carol@OURS.test", nil)
	if err := h.task.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.external) != 0 {
		t.Fatalf("local recipients went to MX: %v", h.external)
	}
	if strings.Join(h.local.delivered, ",") != "bob@ours.test,carol@ours.test" {
		t.Fatalf("delivered locally: %v", h.local.delivered)
	}
	if !h.outbox.sent || h.tracker.last().Status != outbox.StatusDelivered || len(h.local.dsns) != 0 {
		t.Fatalf("outbox %+v, delivery %+v, dsns %d", h.outbox, h.tracker.last(), len(h.local.dsns))
	}
	// The copy in Sent was made at acceptance; delivery must not add one.
	if h.tracker.copies != 0 {
		t.Fatalf("send task made %d copies in Sent", h.tracker.copies)
	}
}

func TestDirectSendMixedRecipients(t *testing.T) {
	h := newHarness("bob@ours.test, ext@remote.test", nil)
	if err := h.task.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.external) != 1 || strings.Join(h.external[0], ",") != "ext@remote.test" {
		t.Fatalf("MX got %v, want only the external recipient", h.external)
	}
	if strings.Join(h.local.delivered, ",") != "bob@ours.test" {
		t.Fatalf("delivered locally: %v", h.local.delivered)
	}
	if !h.outbox.sent || h.tracker.last().Status != outbox.StatusDelivered || h.tracker.copies != 0 {
		t.Fatalf("outbox %+v, delivery %+v, copies %d", h.outbox, h.tracker.last(), h.tracker.copies)
	}
}

func TestDirectSendToOwnAddressUsesSentCopy(t *testing.T) {
	h := newHarness("alice@ours.test", nil)
	if err := h.task.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.local.delivered) != 0 || len(h.external) != 0 {
		t.Fatalf("self-send delivered %v / MX %v; the Sent copy is the user's copy", h.local.delivered, h.external)
	}
	if !h.outbox.sent || h.tracker.last().Status != outbox.StatusDelivered || h.tracker.copies != 0 {
		t.Fatalf("outbox %+v, delivery %+v, copies %d", h.outbox, h.tracker.last(), h.tracker.copies)
	}
}

func TestDirectSendUnknownLocalMailboxBounces(t *testing.T) {
	h := newHarness("nobody@ours.test", nil)
	if err := h.task.Execute(context.Background()); err == nil {
		t.Fatal("want an error for an undeliverable message")
	}
	if h.outbox.status != "failed" || h.outbox.retries != 0 {
		t.Fatalf("outbox %+v: want failed at once, no retries", h.outbox)
	}
	if !strings.Contains(h.outbox.lastError, "5.1.1") || strings.Contains(h.outbox.lastError, "nobody@") {
		t.Fatalf("last_error %q: want the status, with the address masked", h.outbox.lastError)
	}
	if d := h.tracker.last(); d.Status != outbox.StatusFailed || d.Reason != outbox.ReasonMailboxUnavailable ||
		strings.Join(d.Failed, ",") != "nobody@ours.test" {
		t.Fatalf("delivery %+v: want failed, mailbox_unavailable, nobody@", d)
	}
	if len(h.local.dsns) != 1 {
		t.Fatalf("got %d DSNs, want 1", len(h.local.dsns))
	}
	raw := h.local.dsnRaw[0]
	for _, want := range []string{
		"multipart/report; report-type=delivery-status",
		"Final-Recipient: rfc822; nobody@ours.test",
		"Status: 5.1.1",
		"Diagnostic-Code: smtp; 550 5.1.1 No such user here",
		"MAILER-DAEMON@mx.ours.test",
		"To: <alice@ours.test>",
		"Message-ID: <out1@ours.test>", // original headers attached
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("DSN lacks %q:\n%s", want, raw)
		}
	}
}

func TestDirectSendPartialFailureStillSent(t *testing.T) {
	h := newHarness("bob@ours.test, nobody@ours.test", nil)
	if err := h.task.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.outbox.sent || h.outbox.status != "sent" || !strings.Contains(h.outbox.lastError, "5.1.1") {
		t.Fatalf("outbox %+v: want sent with the failure recorded", h.outbox)
	}
	if d := h.tracker.last(); d.Status != outbox.StatusFailed || strings.Join(d.Failed, ",") != "nobody@ours.test" || len(h.local.dsns) != 1 {
		t.Fatalf("delivery %+v, dsns %d: want failed for nobody@ only", d, len(h.local.dsns))
	}
}

func TestDirectSendTransientExternalRetriesThenExpires(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	h := newHarness("bob@ours.test, ext@remote.test", func(string) error { return refused })

	for attempt := 1; attempt < maxRetries; attempt++ {
		if err := h.task.Execute(context.Background()); err == nil {
			t.Fatalf("attempt %d: want error", attempt)
		}
		if h.outbox.status != "pending" || h.outbox.retries != attempt || len(h.local.dsns) != 0 {
			t.Fatalf("attempt %d: outbox %+v, dsns %d", attempt, h.outbox, len(h.local.dsns))
		}
		if d := h.tracker.last(); d.Status != outbox.StatusPending || d.Reason != outbox.ReasonUnreachable {
			t.Fatalf("attempt %d: delivery %+v, want pending/unreachable", attempt, d)
		}
	}
	// Last attempt: give up, bounce the external recipient; the local one
	// got the message, so the row is sent (with the failure noted).
	_ = h.task.Execute(context.Background())
	if h.outbox.status != "sent" || h.tracker.last().Status != outbox.StatusFailed {
		t.Fatalf("after %d attempts: outbox %+v, delivery %+v", maxRetries, h.outbox, h.tracker.last())
	}
	if len(h.local.dsns) != 1 || !strings.Contains(h.local.dsnRaw[0], "Status: 4.4.7") {
		t.Fatalf("want one DSN with 4.4.7, got %d", len(h.local.dsns))
	}
}

func TestDirectSendPermanentExternalFailsAtOnce(t *testing.T) {
	cases := map[string]error{
		"5xx":      fmt.Errorf("failed to deliver: %w", &textproto.Error{Code: 550, Msg: "5.1.1 user unknown"}),
		"nxdomain": fmt.Errorf("dial failed: %w", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "example.test", IsNotFound: true}}),
		"null MX":  fmt.Errorf("failed to deliver: %w", errNullMX),
	}
	for name, sendErr := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness("x@example.test", func(string) error { return sendErr })
			_ = h.task.Execute(context.Background())
			if h.outbox.status != "failed" || h.outbox.retries != 0 {
				t.Fatalf("outbox %+v: want failed without retries", h.outbox)
			}
			if len(h.local.dsns) != 1 || !strings.Contains(h.local.dsnRaw[0], "Status: 5.") {
				t.Fatalf("want one 5.x.x DSN, got %d", len(h.local.dsns))
			}
		})
	}
}

func TestDirectSendLookupErrorRetries(t *testing.T) {
	h := newHarness("bob@ours.test", nil)
	h.local.lookupErr = errors.New("db down")
	if err := h.task.Execute(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if h.outbox.status != "pending" || h.outbox.retries != 1 || len(h.external) != 0 {
		t.Fatalf("outbox %+v, MX calls %v: a DB error must retry, never route to MX", h.outbox, h.external)
	}
}

func TestIsPermanent(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&textproto.Error{Code: 550, Msg: "no"}, true},
		{fmt.Errorf("wrap: %w", &textproto.Error{Code: 554, Msg: "no"}), true},
		{&textproto.Error{Code: 451, Msg: "later"}, false},
		{&net.DNSError{Err: "no such host", IsNotFound: true}, true},
		{&net.DNSError{Err: "server misbehaving", IsTemporary: true}, false},
		{errors.New("connection refused"), false},
		{errNullMX, true},
	}
	for _, c := range cases {
		if got := IsPermanent(c.err); got != c.want {
			t.Errorf("IsPermanent(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestSendDirectNullMXDoesNotDial(t *testing.T) {
	orig := lookupMX
	t.Cleanup(func() { lookupMX = orig })
	lookupMX = func(string) ([]*net.MX, error) { return []*net.MX{{Host: ".", Pref: 0}}, nil }

	res := SendDirectEach("alice@ours.test", []string{"x@nullmx.test"}, []byte("x"), "mx.ours.test")
	if err := res["x@nullmx.test"]; !errors.Is(err, errNullMX) || !IsPermanent(err) {
		t.Fatalf("got %v, want permanent null-MX error", err)
	}
}
