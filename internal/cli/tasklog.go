package cli

import (
	"sync/atomic"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// serveTasks takes the lines of tasks.log of the runs of serve (L4,
// finding 7b-3). taskRegistry wraps the store with it before the log files
// open; logFiles hands it the files once they are open. A line before that
// is dropped, and the files count a line after their close.
var serveTasks taskLog

type taskLog struct {
	files atomic.Pointer[logs.Files]
}

func (l *taskLog) set(files *logs.Files) { l.files.Store(files) }

// Task writes the line to the files of serve, when they are open.
func (l *taskLog) Task(e logs.TaskEvent) error {
	if f := l.files.Load(); f != nil {
		return f.Task(e)
	}
	return nil
}
