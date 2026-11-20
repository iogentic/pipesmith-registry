package rest

import (
	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The tasks (leg 6-system, contract S38 and S40): the list and one task in
// the shape of TaskXO, a run and a stop. The server checks the permission
// and the request token before it calls a function here.

// The states of a task in TaskXO.
const (
	taskWaiting = "WAITING"
	taskRunning = "RUNNING"
)

// LastRuns reads the last run of each task, by task id, and of one task
// alone (state.Store.LastRuns and LastRun).
type LastRuns interface {
	LastRuns() (map[string]state.TaskRun, error)
	LastRun(taskID string) (state.TaskRun, bool, error)
}

// taskXO is one TaskXO. message is the reason a disabled task cannot run,
// else the message of its last run, null for none; schedule is the type of
// its schedule; the times are UTC.
type taskXO struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Message       *string `json:"message"`
	CurrentState  string  `json:"currentState"`
	LastRunResult *string `json:"lastRunResult"`
	NextRun       *string `json:"nextRun"`
	LastRun       *string `json:"lastRun"`
	Schedule      string  `json:"schedule"`
}

type taskPageXO struct {
	Items             []taskXO `json:"items"`
	ContinuationToken *string  `json:"continuationToken"`
}

func text(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func taskOf(r *tasks.Registry, s tasks.Status, last state.TaskRun, hasLast bool) taskXO {
	x := taskXO{ID: s.ID, Name: s.Name, Type: s.TypeID, CurrentState: taskWaiting, Schedule: s.Schedule.Type, Message: text(s.Reason)}
	if s.Running {
		x.CurrentState = taskRunning
	}
	if s.HasNext {
		next := timestamp(s.Next)
		x.NextRun = &next
	}
	if hasLast {
		started := timestamp(last.Started)
		x.LastRun, x.LastRunResult = &started, text(last.Result)
		if x.Message == nil {
			x.Message = text(r.Message(s.ID, last.Message))
		}
	}
	return x
}

func lastRuns(runs LastRuns) (map[string]state.TaskRun, error) {
	if runs == nil {
		return map[string]state.TaskRun{}, nil
	}
	return runs.LastRuns()
}

// Tasks is GET /v1/tasks: every task of the registry on one page, by id.
func Tasks(r *tasks.Registry, runs LastRuns) (Answer, error) {
	last, err := lastRuns(runs)
	if err != nil {
		return Answer{}, err
	}
	list := r.List()
	page := taskPageXO{Items: make([]taskXO, 0, len(list))}
	for _, s := range list {
		run, held := last[s.ID]
		page.Items = append(page.Items, taskOf(r, s, run, held))
	}
	return ok(page)
}

// Task is GET /v1/tasks/{id}: the task, or 404.
func Task(r *tasks.Registry, runs LastRuns, id string) (Answer, error) {
	s, found := r.Status(id)
	if !found {
		return refused(admin.TaskRefusal(id, tasks.ErrUnknownTask))
	}
	var run state.TaskRun
	var held bool
	if runs != nil {
		var err error
		if run, held, err = runs.LastRun(id); err != nil {
			return Answer{}, err
		}
	}
	return ok(taskOf(r, s, run, held))
}

// RunTask is POST /v1/tasks/{id}/run: 204 once the run has started; the
// refusals of admin.TaskRefusal.
func RunTask(store admin.IdentityStore, r *tasks.Registry, id string) (Answer, error) {
	if err := admin.Act(store, func() error { return r.Run(id) }); err != nil {
		return refused(admin.TaskRefusal(id, err))
	}
	return noContent()
}

// StopTask is POST /v1/tasks/{id}/stop: 204 once the run is told to stop;
// 409 for a task that is not running.
func StopTask(store admin.IdentityStore, r *tasks.Registry, id string) (Answer, error) {
	if err := admin.Act(store, func() error { return r.Stop(id) }); err != nil {
		return refused(admin.TaskRefusal(id, err))
	}
	return noContent()
}
