package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/authfail"
	"github.com/ddletotam/ddmailserver/internal/task"
)

func TestIdleRetryDelay(t *testing.T) {
	netErr := errors.New("failed to connect: lookup imap.mail.me.com: i/o timeout")
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute}
	for i, w := range want {
		d, ok := idleRetryDelay(i+1, netErr)
		if !ok || d != w {
			t.Fatalf("attempt %d: got (%v, %v), want (%v, true)", i+1, d, ok, w)
		}
	}
	if _, ok := idleRetryDelay(len(want)+1, netErr); ok {
		t.Fatal("retries must stop after the last delay")
	}
	if _, ok := idleRetryDelay(1, nil); ok {
		t.Fatal("successful sync must not be retried")
	}
	if _, ok := idleRetryDelay(1, fmt.Errorf("login: %w", authfail.MarkLoginRefusal(errors.New("NO [AUTHENTICATIONFAILED]")))); ok {
		t.Fatal("rejected credentials must not be retried (auth_backoff owns them)")
	}
	if _, ok := idleRetryDelay(1, context.Canceled); ok {
		t.Fatal("shutdown must not be retried")
	}
}

type idleFakeTask struct{ err error }

func (f idleFakeTask) Execute(context.Context) error { return f.err }
func (f idleFakeTask) Type() task.Type               { return task.TypeIMAP }
func (f idleFakeTask) Priority() int                 { return 7 }
func (f idleFakeTask) String() string                { return "IMAP sync for a@b (account 1)" }

func TestOnDoneTaskPassesResultAndKeepsIdentity(t *testing.T) {
	boom := errors.New("boom")
	var got error
	called := false
	w := onDoneTask{Task: idleFakeTask{err: boom}, done: func(err error) { got, called = err, true }}
	if err := w.Execute(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Execute returned %v", err)
	}
	if !called || !errors.Is(got, boom) {
		t.Fatalf("done got %v (called=%v)", got, called)
	}
	if w.String() != "IMAP sync for a@b (account 1)" || w.Priority() != 7 || w.Type() != task.TypeIMAP {
		t.Fatal("wrapper must keep String/Type/Priority for pool dedupe and lanes")
	}
}
