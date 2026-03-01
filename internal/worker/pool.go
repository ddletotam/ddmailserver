package worker

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync"
)

// Pool manages a pool of workers that execute tasks
type Pool struct {
	imapQueue       chan Task
	smtpQueue       chan Task
	imapWorkerCount int
	smtpWorkerCount int
	wg              sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
	stats           *Stats
	mu              sync.RWMutex
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
		smtpQueue:       make(chan Task, queueSize),
		imapWorkerCount: imapWorkers,
		smtpWorkerCount: smtpWorkers,
		ctx:             ctx,
		cancel:          cancel,
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

	for {
		select {
		case <-p.ctx.Done():
			log.Printf("IMAP worker %d shutting down", id)
			return

		case task, ok := <-p.imapQueue:
			if !ok {
				log.Printf("IMAP worker %d: queue closed", id)
				return
			}

			// Execute task
			log.Printf("IMAP worker %d executing: %s", id, task.String())

			err := task.Execute(p.ctx)

			p.mu.Lock()
			if err != nil {
				p.stats.IMAPFailed++
				log.Printf("IMAP worker %d task failed: %s - error: %v", id, task.String(), err)
			} else {
				p.stats.IMAPCompleted++
				log.Printf("IMAP worker %d completed: %s", id, task.String())
			}
			p.mu.Unlock()
		}
	}
}

// smtpWorker processes SMTP tasks
func (p *Pool) smtpWorker(id int) {
	defer p.wg.Done()

	log.Printf("SMTP worker %d started", id)

	for {
		select {
		case <-p.ctx.Done():
			log.Printf("SMTP worker %d shutting down", id)
			return

		case task, ok := <-p.smtpQueue:
			if !ok {
				log.Printf("SMTP worker %d: queue closed", id)
				return
			}

			// Execute task
			log.Printf("SMTP worker %d executing: %s", id, task.String())

			err := task.Execute(p.ctx)

			p.mu.Lock()
			if err != nil {
				p.stats.SMTPFailed++
				log.Printf("SMTP worker %d task failed: %s - error: %v", id, task.String(), err)
			} else {
				p.stats.SMTPCompleted++
				log.Printf("SMTP worker %d completed: %s", id, task.String())
			}
			p.mu.Unlock()
		}
	}
}

// Submit submits a task to the pool
func (p *Pool) Submit(task Task) error {
	var queue chan Task
	var queueType string

	switch task.Type() {
	case TaskTypeIMAP:
		queue = p.imapQueue
		queueType = "IMAP"
	case TaskTypeSMTP:
		queue = p.smtpQueue
		queueType = "SMTP"
	default:
		return fmt.Errorf("unknown task type: %s", task.Type())
	}

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
		return fmt.Errorf("pool is shutting down")
	default:
		return fmt.Errorf("%s task queue is full", queueType)
	}
}

// Stop gracefully stops the worker pool
func (p *Pool) Stop() {
	log.Printf("Stopping worker pool...")
	p.cancel()

	// Close the task queues to signal no more tasks
	close(p.imapQueue)
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

// QueueLength returns the current number of tasks in queues
func (p *Pool) QueueLength() (imap, smtp int) {
	return len(p.imapQueue), len(p.smtpQueue)
}
