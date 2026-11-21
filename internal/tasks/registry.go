package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The registry holds the stored tasks of a running server on a Scheduler
// (ruling SB1 of leg 6-system): every stored task is registered, and a task
// that cannot run is registered disabled with its reason. It answers what
// the REST answers of the tasks need besides the stored runs.

// MaxMessage is the most characters of the message of a run (ruling SB5).
const MaxMessage = 1024

// Kind makes the job of a stored task of one task type. An error leaves the
// task disabled with the text of the error as its reason.
type Kind func(t model.Task) (Job, error)

// Job is the work of a task and the secrets of the task, which no message of
// its runs may show.
type Job struct {
	Run     Func
	Secrets []string
}

// RegistryOptions are the kinds of the task types, by type id, the data
// directory, whose path no message shows, the logger, nil for the default
// one, and the secrets of a task, which no reason and no message shows
// (ruling SB11), nil for none besides those of its Job. Host is the late
// bound host of the kinds, which SetHost sets; nil for none.
type RegistryOptions struct {
	Kinds   map[string]Kind
	DataDir string
	Log     *slog.Logger
	Secrets func(model.Task) []string
	Host    *HostRef
}

// ErrPaused is the refusal of a run while the server is frozen.
var ErrPaused = errors.New("the server is frozen, so no task starts")

// PauseWhile sets what tells the registry that the server is frozen
// (ruling SB8): paused gives true while frozen and the number of the
// freeze. While paused an instant of a schedule is skipped, logged once per
// freeze, and a run on request is refused with ErrPaused; a run that is
// going runs to its end.
func (r *Registry) PauseWhile(paused func() (bool, uint64)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = paused
}

// pausedNow reports whether the server is frozen, and logs the first skip
// of each freeze when skip is true.
func (r *Registry) pausedNow(skip bool, id string) bool {
	r.mu.Lock()
	paused := r.paused
	r.mu.Unlock()
	if paused == nil {
		return false
	}
	frozen, number := paused()
	if !frozen {
		return false
	}
	r.mu.Lock()
	first := skip && number != r.loggedFreeze
	if first {
		r.loggedFreeze = number
	}
	r.mu.Unlock()
	if first {
		r.log.Info(fmt.Sprintf("the scheduled runs are skipped while the server is frozen, first %s", id), "logger", loggerName)
	}
	return true
}

// pausing is the store of the scheduler: it refuses the start of a run
// while the server is frozen, so that the scheduler skips the instant and
// waits for the next one.
type pausing struct {
	Store
	r *Registry
}

func (p pausing) BeginRun(taskID string, started time.Time) (int64, error) {
	if p.r.pausedNow(true, taskID) {
		return 0, ErrPaused
	}
	return p.Store.BeginRun(taskID, started)
}

// Registry is the tasks of the state on one Scheduler. Its methods are safe
// for use by several goroutines.
type Registry struct {
	s       *Scheduler
	kinds   map[string]Kind
	dataDir string
	secrets func(model.Task) []string
	host    *HostRef

	log *slog.Logger

	// change orders the creates, saves and removals against the runs on
	// request: a run holds it for reading while it starts
	change sync.RWMutex

	mu           sync.Mutex
	tasks        map[string]*registered
	paused       func() (bool, uint64)
	loggedFreeze uint64 // the freeze whose first skip is logged
}

type registered struct {
	task     model.Task
	schedule Schedule
	enabled  bool
	reason   string // why the task is disabled; empty when it is enabled
	secrets  []string
	inSched  bool // the scheduler holds the task
}

// Status is a task of the registry as the answers show it. Next is the
// instant the task waits for when HasNext is true.
type Status struct {
	ID, Name, TypeID string
	Schedule         Schedule
	Enabled          bool
	Reason           string
	Running          bool
	Next             time.Time
	HasNext          bool
}

// DisabledError is the refusal of a run of a disabled task with its reason.
// It is ErrDisabled for errors.Is.
type DisabledError struct{ ID, Reason string }

func (e *DisabledError) Error() string { return e.Reason }

// Is makes a DisabledError match ErrDisabled.
func (e *DisabledError) Is(target error) bool { return target == ErrDisabled }

// NewRegistry is a registry on a new scheduler of clock and store.
func NewRegistry(clock Clock, store Store, opts RegistryOptions) *Registry {
	r := &Registry{kinds: opts.Kinds, dataDir: opts.DataDir, log: opts.Log, secrets: opts.Secrets, host: opts.Host,
		tasks: map[string]*registered{}}
	if r.log == nil {
		r.log = slog.Default()
	}
	r.s = New(clock, pausing{Store: store, r: r})
	return r
}

// ScheduleOf is the schedule of a stored task.
func ScheduleOf(s model.TaskSchedule) Schedule {
	return Schedule{Type: s.Type, StartAt: s.StartAt, WeekDays: slices.Clone(s.WeekDays), MonthDays: slices.Clone(s.MonthDays), Cron: s.Cron}
}

// Load registers stored tasks. A task whose schedule the scheduler refuses
// is disabled with the refusal; one whose type has no kind is disabled
// with the reason of its row of the table of kinds (kinds.go); an imported
// task is paused (I12); a task stored disabled stays disabled. A task id is
// registered once.
func (r *Registry) Load(list []model.Task) error {
	for _, t := range list {
		if err := r.add(t, false); err != nil {
			return err
		}
	}
	return nil
}

// add registers t. committed: the store holds t already, so a closed
// scheduler is no error (stored).
func (r *Registry) add(t model.Task, committed bool) error {
	e, run := r.build(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[t.ID]; ok {
		return fmt.Errorf("task %q: %w", t.ID, ErrDuplicate)
	}
	if e.inSched {
		err := r.s.Add(r.task(e, run))
		if committed {
			err = r.stored(t.ID, err)
		}
		if err != nil {
			return err
		}
	}
	r.tasks[t.ID] = e
	return nil
}

// stored is the answer of a change of the scheduler after the change of
// the task of id is stored: ErrClosed, the scheduler of a shutdown, is
// logged and no error, since the store holds the change and nothing runs
// any more.
func (r *Registry) stored(id string, err error) error {
	if !errors.Is(err, ErrClosed) {
		return err
	}
	r.log.Warn(fmt.Sprintf("the change of the task %s is stored; the scheduler is closed, and the next start of the server loads it", id), "logger", loggerName)
	return nil
}

// build is the entry of a stored task and its work. inSched tells whether
// the scheduler takes the schedule.
func (r *Registry) build(t model.Task) (*registered, Func) {
	e := &registered{task: t, schedule: ScheduleOf(t.Schedule)}
	if r.secrets != nil {
		e.secrets = slices.Clone(r.secrets(t))
	}
	run := notBuilt(t.TypeID)
	_, scheduleErr := e.schedule.Trigger()
	kind, built := r.kinds[t.TypeID]
	switch {
	case scheduleErr != nil:
		e.reason = fmt.Sprintf("the schedule of the task %s is refused: %v", t.ID, scheduleErr)
	case !built:
		e.reason = reasonOf(t.TypeID)
	default:
		job, err := kind(t)
		switch {
		case err != nil:
			e.reason = fmt.Sprintf("the task %s cannot run: %v", t.ID, err)
		case job.Run == nil:
			e.reason = fmt.Sprintf("the task type %s gives no work", t.TypeID)
		default:
			run, e.secrets = job.Run, append(e.secrets, job.Secrets...)
			switch {
			case t.Origin == model.OriginImport:
				e.reason = fmt.Sprintf("the task %s was imported and stays paused until it is saved enabled", t.ID)
			case !t.Enabled:
				e.reason = fmt.Sprintf("the task %s is disabled", t.ID)
			default:
				e.enabled = true
			}
		}
	}
	e.reason = r.clean(e.reason, e.secrets)
	e.inSched = scheduleErr == nil
	return e, run
}

// task is the task of the scheduler of an entry.
func (r *Registry) task(e *registered, run Func) Task {
	clean := r.cleaning(e.secrets)
	return Task{ID: e.task.ID, Schedule: e.schedule, Enabled: e.enabled, Run: func(ctx context.Context) (message string, err error) {
		// a panic is a failed run whose text is cleaned like any other
		defer func() {
			if p := recover(); p != nil {
				message, err = "", errors.New(clean(fmt.Sprintf("the task panicked: %v", p)))
			}
		}()
		if message, err = run(ctx); err != nil {
			return "", errors.New(clean(err.Error()))
		}
		return clean(message), nil
	}}
}

// Create registers the task that commit stores, once commit returns it
// without an error (ruling AP5). The task waits for its first instant at
// once. A scheduler closed by a shutdown is logged, and the stored create
// is no error.
func (r *Registry) Create(commit func() (model.Task, error)) error {
	r.change.Lock()
	defer r.change.Unlock()
	t, err := commit()
	if err != nil {
		return err
	}
	return r.add(t, true)
}

// Save replaces the registered task of the id with the task that commit
// stores, once commit returns it without an error (ruling AP5). A run that
// is going keeps its work; the saved task applies from its next run. A task
// that is not registered is refused with ErrUnknownTask before commit. A
// scheduler closed by a shutdown is logged, and the stored save is no
// error.
func (r *Registry) Save(id string, commit func() (model.Task, error)) error {
	r.change.Lock()
	defer r.change.Unlock()
	r.mu.Lock()
	old := r.tasks[id]
	r.mu.Unlock()
	if old == nil {
		return fmt.Errorf("task %q: %w", id, ErrUnknownTask)
	}
	t, err := commit()
	if err != nil {
		return err
	}
	if t.ID != id {
		return fmt.Errorf("the save of the task %s gives the task %s", id, t.ID)
	}
	e, run := r.build(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case e.inSched && old.inSched:
		err = r.s.Replace(r.task(e, run))
	case e.inSched:
		err = r.s.Add(r.task(e, run))
	case old.inSched:
		// the scheduler keeps the task, disabled, under its last schedule
		err = r.s.Replace(Task{ID: id, Schedule: old.schedule, Run: notBuilt(t.TypeID)})
		e.inSched = true
	}
	r.tasks[id] = e
	return r.stored(id, err)
}

// Remove takes the registered task of the id away once commit returns nil
// (ruling AP5). A running task is refused with ErrRunning and commit is not
// called; no run of the task starts while commit runs or after it.
func (r *Registry) Remove(id string, commit func() error) error {
	r.change.Lock()
	defer r.change.Unlock()
	r.mu.Lock()
	e := r.tasks[id]
	r.mu.Unlock()
	if e == nil {
		return fmt.Errorf("task %q: %w", id, ErrUnknownTask)
	}
	var err error
	if e.inSched {
		err = r.s.Remove(id, commit)
	} else {
		err = commit()
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.tasks, id)
	r.mu.Unlock()
	return nil
}

// notBuilt is the work of a task without a kind; the scheduler never runs
// it, because the task is disabled.
func notBuilt(typeID string) Func {
	return func(context.Context) (string, error) {
		return "", fmt.Errorf("the task type %s is not built", typeID)
	}
}

// SetHost sets the host the kinds call, once the server is built; nil takes
// it away. A registry without a HostRef ignores it.
func (r *Registry) SetHost(h Host) {
	if r.host != nil {
		r.host.Set(h)
	}
}

// Start ends the runs of the store without an end and starts the
// scheduler.
func (r *Registry) Start() error { return r.s.Start() }

// Close stops every run and waits for their ends, or until ctx is done.
func (r *Registry) Close(ctx context.Context) error { return r.s.Close(ctx) }

// List is the status of every registered task, in the order of the ids.
func (r *Registry) List() []Status {
	r.mu.Lock()
	ids := make([]string, 0, len(r.tasks))
	for id := range r.tasks {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	slices.Sort(ids)
	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		if s, ok := r.Status(id); ok {
			out = append(out, s)
		}
	}
	return out
}

// Status is the status of the task, false when none is registered.
func (r *Registry) Status(id string) (Status, bool) {
	r.mu.Lock()
	e := r.tasks[id]
	r.mu.Unlock()
	if e == nil {
		return Status{}, false
	}
	s := Status{ID: e.task.ID, Name: e.task.Name, TypeID: e.task.TypeID, Schedule: e.schedule, Enabled: e.enabled, Reason: e.reason}
	if e.inSched {
		s.Running = r.s.Running(id)
		s.Next, s.HasNext = r.s.Next(id)
	}
	return s, true
}

// Run starts a run of the task now. A disabled task is refused with a
// DisabledError; the other refusals are those of Scheduler.Run.
func (r *Registry) Run(id string) error {
	r.change.RLock()
	defer r.change.RUnlock()
	r.mu.Lock()
	e := r.tasks[id]
	r.mu.Unlock()
	switch {
	case e == nil:
		return fmt.Errorf("task %q: %w", id, ErrUnknownTask)
	case !e.enabled:
		return &DisabledError{ID: id, Reason: e.reason}
	case r.pausedNow(false, id):
		return ErrPaused
	}
	return r.s.Run(id)
}

// Stop cancels the run of the task; the refusals are those of
// Scheduler.Stop.
func (r *Registry) Stop(id string) error {
	r.mu.Lock()
	e := r.tasks[id]
	r.mu.Unlock()
	switch {
	case e == nil:
		return fmt.Errorf("task %q: %w", id, ErrUnknownTask)
	case !e.inSched:
		return fmt.Errorf("task %q: %w", id, ErrNotRunning)
	}
	return r.s.Stop(id)
}

// Message is a message of a run of the task as an answer may show it: the
// data directory written as <data>, each secret of the task as ***, and cut
// to MaxMessage characters.
func (r *Registry) Message(id, message string) string {
	r.mu.Lock()
	var secrets []string
	if e := r.tasks[id]; e != nil {
		secrets = e.secrets
	}
	r.mu.Unlock()
	return r.clean(message, secrets)
}

func (r *Registry) cleaning(secrets []string) func(string) string {
	return func(message string) string { return r.clean(message, secrets) }
}

func (r *Registry) clean(message string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "***")
		}
	}
	if r.dataDir != "" {
		dirs := []string{r.dataDir}
		if abs, err := filepath.Abs(r.dataDir); err == nil {
			dirs = append(dirs, abs)
		}
		if real, err := filepath.EvalSymlinks(r.dataDir); err == nil {
			dirs = append(dirs, real)
		}
		// the longest first, so no part of a longer form stays
		slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
		for _, dir := range dirs {
			if dir != "" && dir != string(filepath.Separator) {
				message = strings.ReplaceAll(message, dir, "<data>")
			}
		}
	}
	if runes := []rune(message); len(runes) > MaxMessage {
		message = string(runes[:MaxMessage])
	}
	return message
}
