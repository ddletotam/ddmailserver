package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// ErrDuplicateTask is returned by Submit when the very same logical task (same
// Task.String()) is already waiting in a queue. The scheduler re-submits every
// account / source on every tick, so without this the queue filled up with
// hundreds of identical entries: two IMAP workers chewed through an
// hours-stale FIFO backlog, `queue_size` (1000) was permanently exhausted, and
// anything queued afterwards — including the reverse flag push — never got a
// slot. Not an error condition: the work is already scheduled.
var ErrDuplicateTask = errors.New("task already queued")

// ErrPoolStopped is returned by Submit once Stop has begun. The IDLE manager
// and the outbox trigger submit from their own goroutines and can race with
// shutdown; they get an error instead of a send on a closed channel.
var ErrPoolStopped = errors.New("worker pool is stopped")

// fastLanePriority is the Priority() at which a task takes the priority queue
// instead of the bulk one. Tasks declared their priority since day one and the
// pool ignored it: a FlagSyncTask (2) queued behind a pile of full-mailbox
// SyncTasks (1) is exactly the "push loses to pull" race that made read marks
// bounce back.
const fastLanePriority = 2

// Pool manages a pool of workers that execute tasks
type Pool struct {
	imapQueue       chan Task
	imapFastQueue   chan Task
	smtpQueue       chan Task
	smtpFastQueue   chan Task
	imapWorkerCount int
	smtpWorkerCount int
	wg              sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
	stats           *Stats
	mu              sync.RWMutex

	// stopped is set by Stop under mu; Submit checks it and does its
	// (non-blocking) channel send under the same lock, so nothing enters a
	// queue once shutdown has begun. The queues are never closed — workers
	// leave on ctx — which is what makes a late Submit harmless.
	stopped bool

	// Logical tasks (keyed by Task.String()) waiting for a worker: in a queue
	// or parked in `deferred`. A key leaves `queued` when a worker actually
	// starts it, NOT when it finishes: a trigger that arrives mid-run (IDLE saw
	// new mail during the sync) still earns one follow-up run.
	queued map[string]bool
	// Logical tasks currently executing. Two runs of the same key never
	// overlap: two concurrent syncs of one account pull the same mailbox
	// twice over two connections and race each other on the dedup check.
	running map[string]bool
	// A follow-up taken from the queue while its key was still running. The
	// worker that finishes the running copy executes it next.
	deferred map[string]Task
}

// Stats holds pool statistics
type Stats struct {
	IMAPQueued    int64
	IMAPCompleted int64
	IMAPFailed    int64
	SMTPQueued    int64
	SMTPCompleted int64
	SMTPFailed    int64
	IMAPWorkers   int
	SMTPWorkers   int
}

// NewPool creates a worker pool with fixed worker counts.
//
// Counts are explicit because the work is network-bound: deriving them from
// runtime.NumCPU used to leave a 2-CPU host with no IMAP worker at all, so
// mail sync never ran. Values below 1 are raised to 1 — a kind with no worker
// would accept tasks and never run them.
func NewPool(imapWorkers, smtpWorkers, queueSize int) *Pool {
	if imapWorkers < 1 {
		imapWorkers = 1
	}
	if smtpWorkers < 1 {
		smtpWorkers = 1
	}
	if queueSize < 1 {
		queueSize = 1
	}

	pool := newPool(imapWorkers, smtpWorkers, queueSize)

	log.Printf("Worker pool initialized: %d IMAP, %d SMTP workers, queue size %d",
		imapWorkers, smtpWorkers, queueSize)

	return pool
}

func newPool(imapWorkers, smtpWorkers, queueSize int) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		imapQueue:       make(chan Task, queueSize),
		imapFastQueue:   make(chan Task, queueSize),
		smtpQueue:       make(chan Task, queueSize),
		smtpFastQueue:   make(chan Task, queueSize),
		imapWorkerCount: imapWorkers,
		smtpWorkerCount: smtpWorkers,
		ctx:             ctx,
		cancel:          cancel,
		queued:          make(map[string]bool),
		running:         make(map[string]bool),
		deferred:        make(map[string]Task),
		stats: &Stats{
			IMAPWorkers: imapWorkers,
			SMTPWorkers: smtpWorkers,
		},
	}
}

// Start starts the worker pool
func (p *Pool) Start() {
	// Start IMAP workers
	for i := 0; i < p.imapWorkerCount; i++ {
		p.wg.Add(1)
		go p.imapWorker(i)
	}

	// Start SMTP workers
	for i := 0; i < p.smtpWorkerCount; i++ {
		p.wg.Add(1)
		go p.smtpWorker(i)
	}

	log.Printf("Worker pool started")
}

// imapWorker processes IMAP tasks
func (p *Pool) imapWorker(id int) {
	defer p.wg.Done()

	log.Printf("IMAP worker %d started", id)
	p.workerLoop("IMAP", id, p.imapFastQueue, p.imapQueue)
}

// smtpWorker processes SMTP tasks
func (p *Pool) smtpWorker(id int) {
	defer p.wg.Done()

	log.Printf("SMTP worker %d started", id)
	p.workerLoop("SMTP", id, p.smtpFastQueue, p.smtpQueue)
}

// workerLoop drains `fast` before `bulk`. A short reverse-push (STORE a couple
// of flags) must not wait behind a queue of full-mailbox pulls that each take
// tens of seconds.
func (p *Pool) workerLoop(kind string, id int, fast, bulk chan Task) {
	for {
		// select picks among ready cases at random, so a queued task could win
		// over Done; check explicitly so no new work starts after Stop.
		if p.ctx.Err() != nil {
			log.Printf("%s worker %d shutting down", kind, id)
			return
		}

		// Fast lane first, non-blocking: whenever both lanes have work, the
		// priority task goes now.
		select {
		case t := <-fast:
			p.dispatch(kind, id, t)
			continue
		default:
		}

		select {
		case <-p.ctx.Done():
			log.Printf("%s worker %d shutting down", kind, id)
			return
		case t := <-fast:
			p.dispatch(kind, id, t)
		case t := <-bulk:
			p.dispatch(kind, id, t)
		}
	}
}

// dispatch runs t unless another run of the same logical task is in progress;
// then t is parked and the worker finishing that run executes it next. Either
// way this worker is free again at once — one long sync never pins a second
// worker waiting on it.
func (p *Pool) dispatch(kind string, id int, t Task) {
	key := t.String()

	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	if p.running[key] {
		// At most one parked follow-up per key: Submit refuses while the key
		// is in `queued`, and it stays there until the follow-up starts.
		p.deferred[key] = t
		p.mu.Unlock()
		log.Printf("%s worker %d: %s is already running, follow-up deferred", kind, id, key)
		return
	}
	delete(p.queued, key)
	p.running[key] = true
	p.mu.Unlock()

	for t != nil {
		p.runTask(kind, id, t)

		p.mu.Lock()
		next, ok := p.deferred[key]
		if ok && p.ctx.Err() == nil {
			delete(p.deferred, key)
			delete(p.queued, key)
			t = next
		} else {
			delete(p.running, key)
			t = nil
		}
		p.mu.Unlock()
	}
}

// runTask executes one task with panic recovery and records the outcome.
func (p *Pool) runTask(kind string, id int, t Task) {
	log.Printf("%s worker %d executing: %s", kind, id, t.String())

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
				log.Printf("%s worker %d recovered from panic: %v", kind, id, r)
			}
		}()
		err = t.Execute(p.ctx)
	}()

	p.mu.Lock()
	if err != nil {
		if kind == "IMAP" {
			p.stats.IMAPFailed++
		} else {
			p.stats.SMTPFailed++
		}
		log.Printf("%s worker %d task failed: %s - error: %v", kind, id, t.String(), err)
	} else {
		if kind == "IMAP" {
			p.stats.IMAPCompleted++
		} else {
			p.stats.SMTPCompleted++
		}
		log.Printf("%s worker %d completed: %s", kind, id, t.String())
	}
	p.mu.Unlock()
}

// Submit submits a task to the pool. Returns ErrDuplicateTask when the same
// logical task is already waiting for a worker — callers should treat that as
// "already scheduled", not as a failure — and ErrPoolStopped once Stop began.
func (p *Pool) Submit(task Task) error {
	var fast, bulk chan Task
	var queueType string

	switch task.Type() {
	case TaskTypeIMAP:
		fast, bulk, queueType = p.imapFastQueue, p.imapQueue, "IMAP"
	case TaskTypeSMTP:
		fast, bulk, queueType = p.smtpFastQueue, p.smtpQueue, "SMTP"
	default:
		return fmt.Errorf("unknown task type: %s", task.Type())
	}

	queue := bulk
	if task.Priority() >= fastLanePriority {
		queue = fast
	}

	key := task.String()

	// Admission and the (non-blocking) send happen under one lock: that is
	// what orders every Submit against Stop flipping `stopped`.
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return ErrPoolStopped
	}
	if p.queued[key] {
		return ErrDuplicateTask
	}

	select {
	case queue <- task:
		p.queued[key] = true
		if task.Type() == TaskTypeIMAP {
			p.stats.IMAPQueued++
		} else {
			p.stats.SMTPQueued++
		}
		return nil
	default:
		return fmt.Errorf("%s task queue is full", queueType)
	}
}

// Stop shuts the pool down: refuses new tasks, cancels the context handed to
// running tasks and waits up to `timeout` for the workers to return. Tasks
// still waiting in the queues are dropped — the scheduler derives them again
// from the database after the next start.
//
// A worker that does not return in time is stuck in a call that ignores ctx
// (a network read with no deadline); it is abandoned and process exit takes it
// down, and the returned error names what was still running. Calling Stop more
// than once is safe; later calls return nil immediately.
func (p *Pool) Stop(timeout time.Duration) error {
	p.mu.Lock()
	already := p.stopped
	p.stopped = true
	p.mu.Unlock()
	if already {
		return nil
	}

	log.Printf("Stopping worker pool...")
	p.cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		log.Printf("Worker pool stopped")
		return nil
	case <-timer.C:
		return fmt.Errorf("worker pool: still running after %v: %v", timeout, p.runningKeys())
	}
}

// runningKeys lists the logical tasks currently executing, sorted.
func (p *Pool) runningKeys() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	keys := make([]string, 0, len(p.running))
	for k := range p.running {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Stats returns current pool statistics
func (p *Pool) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return *p.stats
}

// QueueLength returns the current number of tasks waiting in each queue,
// priority lane included.
func (p *Pool) QueueLength() (imap, smtp int) {
	return len(p.imapFastQueue) + len(p.imapQueue), len(p.smtpFastQueue) + len(p.smtpQueue)
}
