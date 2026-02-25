package task

import "context"

// Task represents a unit of work that can be executed by a worker
type Task interface {
	// Execute runs the task
	Execute(ctx context.Context) error

	// Type returns the task type (imap, smtp, etc.)
	Type() Type

	// Priority returns task priority (higher = more important)
	Priority() int

	// String returns a human-readable description of the task
	String() string
}

// Type represents the type of task
type Type string

const (
	TypeIMAP Type = "imap"
	TypeSMTP Type = "smtp"
)
