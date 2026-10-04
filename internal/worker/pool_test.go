package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ddletotam/ddmailserver/internal/task"
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
// observable. Workers are started by the test itself (p.wg.Add + go
// p.imapWorker), so it controls exactly when tasks begin to drain.
func newTestPool(queueSize int) *Pool {
	return newPool(1, 0, queueSize)
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

// overlapTask records how many runs of it are executing at once.
type overlapTask struct {
	name    string
	active  *int32
	maxSeen *int32
	runs    *int32
	hold    time.Duration
}

func (o *overlapTask) Type() task.Type { return task.TypeIMAP }
func (o *overlapTask) Priority() int   { return 1 }
func (o *overlapTask) String() string  { return o.name }
func (o *overlapTask) Execute(context.Context) error {
	n := atomic.AddInt32(o.active, 1)
	for {
		m := atomic.LoadInt32(o.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(o.maxSeen, m, n) {
			break
		}
	}
	time.Sleep(o.hold)
	atomic.AddInt32(o.active, -1)
	atomic.AddInt32(o.runs, 1)
	return nil
}

// TestSameKeyNeverRunsConcurrently: with several idle workers, a follow-up
// sync of an account must not start while the previous sync of that account is
// still running — it waits and runs right after. Before, the dedup key was
// released at pick-up and a second worker happily ran the follow-up in
// parallel: two connections pulling the same mailbox, racing on dedup.
func TestSameKeyNeverRunsConcurrently(t *testing.T) {
	p := newPool(4, 1, 16)
	p.Start()
	defer func() {
		if err := p.Stop(5 * time.Second); err != nil {
			t.Error(err)
		}
	}()

	var active, maxSeen, runs int32
	mk := func() *overlapTask {
		return &overlapTask{name: "IMAP sync for a@b (account 1)", active: &active, maxSeen: &maxSeen, runs: &runs, hold: 50 * time.Millisecond}
	}

	accepted := int32(0)
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := p.Submit(mk()); err == nil {
			accepted++
		} else if !errors.Is(err, ErrDuplicateTask) {
			t.Fatalf("submit: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	waitUntil(t, func() bool { return atomic.LoadInt32(&runs) == accepted })
	if got := atomic.LoadInt32(&maxSeen); got != 1 {
		t.Fatalf("same logical task ran %d-way concurrently, want 1", got)
	}
	if accepted < 2 {
		t.Fatalf("only %d runs accepted, test did not exercise follow-ups", accepted)
	}
}

// TestDifferentKeysRunInParallel guards against over-serialising: the
// per-key limit must not turn the pool into a single worker.
func TestDifferentKeysRunInParallel(t *testing.T) {
	p := newPool(2, 1, 8)
	p.Start()
	defer p.Stop(5 * time.Second)

	ran := make(chan string, 2)
	release := make(chan struct{})
	a := &fakeTask{name: "IMAP sync for a@b (account 1)", priority: 1, release: release, ran: ran}
	b := &fakeTask{name: "IMAP sync for c@d (account 2)", priority: 1, release: release, ran: ran}
	if err := p.Submit(a); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(b); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case n := <-ran:
			seen[n] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("only %v started; different accounts must sync in parallel", seen)
		}
	}
	close(release)
}

// TestStopWithConcurrentSubmits is the shutdown crash: Stop used to close the
// queues while the IDLE manager and TriggerOutbox were still submitting from
// their own goroutines — panic: send on closed channel. Run with -race.
func TestStopWithConcurrentSubmits(t *testing.T) {
	p := newPool(2, 2, 64)
	p.Start()

	var wg sync.WaitGroup
	stopSubmitting := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stopSubmitting:
					return
				default:
				}
				ft := &fakeTask{name: fmt.Sprintf("t-%d-%d", g, i), priority: 1 + i%2, ran: make(chan string, 1)}
				err := p.Submit(ft)
				if errors.Is(err, ErrPoolStopped) {
					return
				}
			}
		}(g)
	}

	time.Sleep(20 * time.Millisecond)
	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Submitters keep going after Stop for a moment: every one must get
	// ErrPoolStopped, none may panic.
	time.Sleep(10 * time.Millisecond)
	close(stopSubmitting)
	wg.Wait()

	if err := p.Submit(&fakeTask{name: "late", priority: 1, ran: make(chan string, 1)}); !errors.Is(err, ErrPoolStopped) {
		t.Fatalf("submit after stop: got %v, want ErrPoolStopped", err)
	}
	if err := p.Stop(time.Second); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// stuckTask ignores its context, like a network read without a deadline.
type stuckTask struct{ release chan struct{} }

func (s *stuckTask) Type() task.Type               { return task.TypeSMTP }
func (s *stuckTask) Priority() int                 { return 1 }
func (s *stuckTask) String() string                { return "stuck" }
func (s *stuckTask) Execute(context.Context) error { <-s.release; return nil }

// TestStopTimesOut: a task that ignores ctx must not hold shutdown past the
// budget — this is what got the process SIGKILLed by systemd.
func TestStopTimesOut(t *testing.T) {
	p := newPool(1, 1, 4)
	p.Start()
	st := &stuckTask{release: make(chan struct{})}
	defer close(st.release)
	if err := p.Submit(st); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return len(p.runningKeys()) == 1 })

	start := time.Now()
	err := p.Stop(200 * time.Millisecond)
	if err == nil {
		t.Fatal("Stop returned nil while a task was stuck")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Stop took %v, budget was 200ms", el)
	}
}

// TestStopCancelsContext: a task that honours ctx ends promptly on Stop.
func TestStopCancelsContext(t *testing.T) {
	p := newPool(1, 1, 4)
	p.Start()
	started := make(chan struct{})
	ct := &ctxTask{started: started}
	if err := p.Submit(ct); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Stop(2 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

type ctxTask struct{ started chan struct{} }

func (c *ctxTask) Type() task.Type { return task.TypeIMAP }
func (c *ctxTask) Priority() int   { return 1 }
func (c *ctxTask) String() string  { return "ctx" }
func (c *ctxTask) Execute(ctx context.Context) error {
	close(c.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestNewPoolExplicitCounts(t *testing.T) {
	p := NewPool(4, 2, 10)
	if p.imapWorkerCount != 4 || p.smtpWorkerCount != 2 {
		t.Fatalf("workers = %d IMAP / %d SMTP, want 4 / 2", p.imapWorkerCount, p.smtpWorkerCount)
	}
	// Zero must never mean "no IMAP worker" — that was the 2-CPU outage.
	p = NewPool(0, 0, 0)
	if p.imapWorkerCount < 1 || p.smtpWorkerCount < 1 || cap(p.imapQueue) < 1 {
		t.Fatalf("degenerate pool: %d IMAP / %d SMTP / queue %d", p.imapWorkerCount, p.smtpWorkerCount, cap(p.imapQueue))
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5s")
		}
		time.Sleep(5 * time.Millisecond)
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
