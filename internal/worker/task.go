package worker

// Re-export task types for convenience
import "github.com/yourusername/mailserver/internal/task"

type Task = task.Task
type TaskType = task.Type

const (
	TaskTypeIMAP = task.TypeIMAP
	TaskTypeSMTP = task.TypeSMTP
)
