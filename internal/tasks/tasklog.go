package tasks

import (
	"strconv"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// TaskLog takes the lines of tasks.log; *logs.Files fulfils it.
type TaskLog interface {
	Task(logs.TaskEvent) error
}

// Logged is store with the lines of tasks.log (L4): a start line once the
// store gave a run its id, an end line once it took the end of the run,
// both with that id. The end line holds the result the row holds and the
// time from start to end by the clock of the scheduler. A run the store
// refused writes no line, and the runs EndOpenRuns ends write none.
func Logged(store Store, log TaskLog) Store {
	return &loggedStore{Store: store, log: log, open: map[int64]begun{}}
}

type loggedStore struct {
	Store
	log  TaskLog
	mu   sync.Mutex
	open map[int64]begun
}

type begun struct {
	task    string
	started time.Time
}

func (l *loggedStore) BeginRun(taskID string, started time.Time) (int64, error) {
	id, err := l.Store.BeginRun(taskID, started)
	if err != nil {
		return id, err
	}
	l.mu.Lock()
	l.open[id] = begun{task: taskID, started: started}
	l.mu.Unlock()
	_ = l.log.Task(logs.TaskEvent{RunID: strconv.FormatInt(id, 10), TaskID: taskID, Event: "start"}) // a lost line is counted by the files
	return id, nil
}

func (l *loggedStore) EndRun(runID int64, ended time.Time, result, message string) error {
	if err := l.Store.EndRun(runID, ended, result, message); err != nil {
		return err
	}
	l.mu.Lock()
	b, ok := l.open[runID]
	delete(l.open, runID)
	l.mu.Unlock()
	e := logs.TaskEvent{RunID: strconv.FormatInt(runID, 10), TaskID: b.task, Event: "end", Result: result}
	if ok {
		e.Duration = ended.Sub(b.started)
	}
	_ = l.log.Task(e) // a lost line is counted by the files
	return nil
}
