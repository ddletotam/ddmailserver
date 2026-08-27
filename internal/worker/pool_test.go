package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yourusername/mailserver/internal/task"
)

// fakeTask is a Task whose execution is externally gated, so a test can hold a
// worker busy and inspect what the queues do meanwhile.
type fakeTask struct {
	name     string
	priority int
	release  chan struct{} // closed/sent to let Execute return
	ran      chan string   // receives name when Execute starts
}

func (f *fakeTask) Type() task.Type { return task.TypeIMAP }
func (f *fakeTask) Priority() int   { return f.priority }
func (f *fakeTask) String() string  { return f.name }
func (f *fakeTask) Execute(context.Context) error {
	f.ran <- f.name
	if f.release != nil {
		<-f.release
	}
	return nil
}

// newTestPool builds a pool with exactly one IMAP worker so ordering is
// observable, bypassing NewPool's CPU-derived sizing.
func newTestPool(queueSize int) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		imapQueue:       make(chan Task, queueSize),
		imapFastQueue:   make(chan Task, queueSize),
		smtpQueue:       make(chan Task, queueSize),
		smtpFastQueue:   make(chan Task, queueSize),
		imapWorkerCount: 1,
		smtpWorkerCount: 0,
		ctx:             ctx,
		cancel:          cancel,
		queued:          make(map[string]bool),
		stats:           &Stats{IMAPWorkers: 1},
	}
}

// TestSubmitRejectsDuplicateWhileQueued is the admission-control guarantee that
// keeps the queue from filling with identical work: the scheduler re-offers
// every account on every tick, and before this the surplus buried short
// priority tasks (the reverse flag push) under an hours-deep FIFO backlog.
func TestSubmitRejectsDuplicateWhileQueued(t *testing.T) {
	p := newTestPool(8)
	defer p.cancel()

	ran := make(chan string, 8)
	first := &fakeTask{name: "IMAP sync for a@b (account 1)", priority: 1, ran: ran}
	dup := &fakeTask{name: "IMAP sync for a@b (account 1)", priority: 1, ran: ran}

	if err := p.Submit(first); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := p.Submit(dup); !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("duplicate submit: got %v, want ErrDuplicateTask", err)
	}
	if got := len(p.imapQueue); got != 1 {
		t.Fatalf("queue depth = %d, want 1", got)
	}
}

// TestSubmitAllowsResubmitOnceRunning: the dedup key is released when a worker
// picks the task up, not when it finishes. New mail arriving mid-sync (IDLE
// trigger) must still earn a follow-up run instead of being swallowed.
func TestSubmitAllowsResubmitOnceRunning(t *testing.T) {
	p := newTestPool(8)
	defer p.cancel()

	ran := make(chan string, 8)
	release := make(chan struct{})
	running := &fakeTask{name: "IMAP sync for a@b (account 1)", priority: 1, release: release, ran: ran}

	p.wg.Add(1)
	go p.imapWorker(0)

	if err := p.Submit(running); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, ran, "IMAP sync for a@b (account 1)")

	// Same logical task, submitted while the first one is still executing.
	followUp := &fakeTask{name: "IMAP sync for a@b (account 1)", priority: 1, ran: ran}
	if err := p.Submit(followUp); err != nil {
		t.Fatalf("resubmit while running: got %v, want nil", err)
	}

	close(release)
	waitFor(t, ran, "IMAP sync for a@b (account 1)")
}

// TestFastLaneRunsBeforeBulk is the starvation fix: a priority-2 task
// (FlagSyncTask — a two-flag STORE) must not wait behind bulk pulls that each
// take tens of seconds. Tasks declared Priority() from day one; the pool used
// to ignore it entirely, which is why a local read mark lost the race against
// the next full pull and bounced back to unread.
func TestFastLaneRunsBeforeBulk(t *testing.T) {
	p := newTestPool(16)
	defer p.cancel()

	ran := make(chan string, 16)
	blockRelease := make(chan struct{})

	// Occupy the single worker so everything else has to queue up.
	blocker := &fakeTask{name: "blocker", priority: 1, release: blockRelease, ran: ran}

	p.wg.Add(1)
	go p.imapWorker(0)

	if err := p.Submit(blocker); err != nil {
		t.Fatalf("submit blocker: %v", err)
	}
	waitFor(t, ran, "blocker")

	// Queue bulk work first, then one priority task: order of submission must
	// not decide order of execution.
	for i := 0; i < 3; i++ {
		bulk := &fakeTask{name: fmt.Sprintf("bulk-%d", i), priority: 1, ran: ran}
		if err := p.Submit(bulk); err != nil {
			t.Fatalf("submit bulk-%d: %v", i, err)
		}
	}
	fast := &fakeTask{name: "Flag sync for a@b (account 1)", priority: 2, ran: ran}
	if err := p.Submit(fast); err != nil {
		t.Fatalf("submit fast: %v", err)
	}

	close(blockRelease)
	waitFor(t, ran, "Flag sync for a@b (account 1)")
}

// TestSubmitFullQueueReleasesKey: a rejected submission must not leave its
// dedup key behind, otherwise that logical task is permanently unschedulable.
func TestSubmitFullQueueReleasesKey(t *testing.T) {
	p := newTestPool(1)
	defer p.cancel()

	ran := make(chan string, 4)
	if err := p.Submit(&fakeTask{name: "filler", priority: 1, ran: ran}); err != nil {
		t.Fatalf("submit filler: %v", err)
	}
	rejected := &fakeTask{name: "victim", priority: 1, ran: ran}
	if err := p.Submit(rejected); err == nil {
		t.Fatal("submit into full queue: got nil, want queue-full error")
	}

	p.mu.RLock()
	stillHeld := p.queued["victim"]
	p.mu.RUnlock()
	if stillHeld {
		t.Fatal("dedup key for a rejected task was not released")
	}
}

func waitFor(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("executed %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q to execute", want)
	}
}
