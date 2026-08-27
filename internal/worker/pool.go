package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
)

// ErrDuplicateTask is returned by Submit when the very same logical task (same
// Task.String()) is already waiting in a queue. The scheduler re-submits every
// account / source on every tick, so without this the queue filled up with
// hundreds of identical entries: two IMAP workers chewed through an
// hours-stale FIFO backlog, `queue_size` (1000) was permanently exhausted, and
// anything queued afterwards — including the reverse flag push — never got a
// slot. Not an error condition: the work is already scheduled.
var ErrDuplicateTask = errors.New("task already queued")

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
	// Logical tasks currently waiting for a worker, keyed by Task.String().
	// A key is released the moment a worker picks the task up, NOT when it
	// finishes: a trigger that arrives while the task runs (IDLE saw new mail
	// mid-sync) still gets its own follow-up run, so at most one extra run per
	// key is ever in flight.
	queued map[string]bool
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

// NewPool creates a new worker pool
func NewPool(cpuLimit, imapPercent, queueSize int) *Pool {
	// Calculate total workers based on CPU limit
	totalCPUs := runtime.NumCPU()
	maxWorkers := (totalCPUs * cpuLimit) / 100
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	// Split workers between IMAP and SMTP
	imapWorkers := (maxWorkers * imapPercent) / 100
	smtpWorkers := maxWorkers - imapWorkers

	// Ensure at least 1 worker of each type if we have enough workers
	if imapWorkers == 0 && maxWorkers > 1 {
		imapWorkers = 1
		smtpWorkers = maxWorkers - 1
	}
	if smtpWorkers == 0 && maxWorkers > 1 {
		smtpWorkers = 1
		imapWorkers = maxWorkers - 1
	}

	ctx, cancel := context.WithCancel(context.Background())

	pool := &Pool{
		imapQueue:       make(chan Task, queueSize),
		imapFastQueue:   make(chan Task, queueSize),
		smtpQueue:       make(chan Task, queueSize),
		smtpFastQueue:   make(chan Task, queueSize),
		imapWorkerCount: imapWorkers,
		smtpWorkerCount: smtpWorkers,
		ctx:             ctx,
		cancel:          cancel,
		queued:          make(map[string]bool),
		stats: &Stats{
			IMAPWorkers: imapWorkers,
			SMTPWorkers: smtpWorkers,
		},
	}

	log.Printf("Worker pool initialized: %d CPUs, %d%% limit = %d total workers (%d IMAP, %d SMTP)",
		totalCPUs, cpuLimit, maxWorkers, imapWorkers, smtpWorkers)

	return pool
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
		// Fast lane first, non-blocking: whenever both lanes have work, the
		// priority task goes now.
		select {
		case <-p.ctx.Done():
			log.Printf("%s worker %d shutting down", kind, id)
			return
		case t, ok := <-fast:
			if !ok {
				log.Printf("%s worker %d: queue closed", kind, id)
				return
			}
			p.runTask(kind, id, t)
			continue
		default:
		}

		select {
		case <-p.ctx.Done():
			log.Printf("%s worker %d shutting down", kind, id)
			return
		case t, ok := <-fast:
			if !ok {
				log.Printf("%s worker %d: queue closed", kind, id)
				return
			}
			p.runTask(kind, id, t)
		case t, ok := <-bulk:
			if !ok {
				log.Printf("%s worker %d: queue closed", kind, id)
				return
			}
			p.runTask(kind, id, t)
		}
	}
}

// runTask executes one task with panic recovery and records the outcome. The
// dedup key is released up front — the task is no longer "waiting", so a fresh
// trigger for the same work can queue a follow-up run.
func (p *Pool) runTask(kind string, id int, t Task) {
	p.release(t.String())

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

// release drops a dedup key so the same logical task can be queued again.
func (p *Pool) release(key string) {
	p.mu.Lock()
	delete(p.queued, key)
	p.mu.Unlock()
}

// Submit submits a task to the pool. Returns ErrDuplicateTask when the same
// logical task is already waiting for a worker — callers should treat that as
// "already scheduled", not as a failure.
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
	p.mu.Lock()
	if p.queued[key] {
		p.mu.Unlock()
		return ErrDuplicateTask
	}
	p.queued[key] = true
	p.mu.Unlock()

	select {
	case queue <- task:
		p.mu.Lock()
		if task.Type() == TaskTypeIMAP {
			p.stats.IMAPQueued++
		} else {
			p.stats.SMTPQueued++
		}
		p.mu.Unlock()
		return nil
	case <-p.ctx.Done():
		p.release(key)
		return fmt.Errorf("pool is shutting down")
	default:
		p.release(key)
		return fmt.Errorf("%s task queue is full", queueType)
	}
}

// Stop gracefully stops the worker pool
func (p *Pool) Stop() {
	log.Printf("Stopping worker pool...")
	p.cancel()

	// Close the task queues to signal no more tasks
	close(p.imapFastQueue)
	close(p.imapQueue)
	close(p.smtpFastQueue)
	close(p.smtpQueue)

	// Wait for all workers to finish
	p.wg.Wait()

	log.Printf("Worker pool stopped")
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
