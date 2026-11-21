// Package tasks runs the tasks of the server: on their schedules (the seven
// types of the contract, cron among them), on request, one run per task at
// a time, and records each run with its start, end and result in a Store.
// The time comes from a Clock; FakeClock moves it in tests.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// The results of a run, the words of the contract that the record stores.
// A run that is going has no result.
const (
	ResultOK          = "OK"
	ResultFailed      = "FAILED"
	ResultCanceled    = "CANCELED"
	ResultInterrupted = "INTERRUPTED" // the run had no end when a start found it
)

const interrupted = "the run had no end when the scheduler started"

const loggerName = "pipesmith_registry.tasks"

// maxWait is the longest timer of the scheduler: while a task waits, the
// time of the clock is read again at least this often, so a step of the
// system time moves a run by no more than this.
const maxWait = time.Minute

// Store records the runs of the tasks. *state.Store fulfils it.
type Store interface {
	// BeginRun records a run of the task that started at started and
	// gives its id.
	BeginRun(taskID string, started time.Time) (int64, error)
	// EndRun records the end of a run: its time, its result and a message.
	EndRun(runID int64, ended time.Time, result, message string) error
	// EndOpenRuns ends every run without an end at ended with result and
	// message, and gives their count.
	EndOpenRuns(ended time.Time, result, message string) (int64, error)
}

// Func is the work of a task. Its message goes into the record of the run.
// An error fails the run and its text is the message. A Func returns soon
// after ctx is canceled.
type Func func(ctx context.Context) (string, error)

// Task is a task of the scheduler. A disabled task runs neither on its
// schedule nor on request.
type Task struct {
	ID       string
	Schedule Schedule
	Enabled  bool
	Run      Func
}

// Refusal is why Add, Start, Run or Stop did nothing. Their errors wrap it, except for an invalid task and a store error.
type Refusal string

func (r Refusal) Error() string { return string(r) }

// The refusals.
const (
	ErrUnknownTask Refusal = "no such task"
	ErrDuplicate   Refusal = "a task of this id exists"
	ErrRunning     Refusal = "the task is running"
	ErrNotRunning  Refusal = "the task is not running"
	ErrDisabled    Refusal = "the task is disabled"
	ErrNotStarted  Refusal = "the scheduler has not started"
	ErrStarted     Refusal = "the scheduler has started"
	ErrClosed      Refusal = "the scheduler is closed"
)

// Scheduler runs its tasks on their schedules and on request. A task runs
// once at a time: an instant that comes while it runs starts nothing and is
// not kept, and a run records its start only once the store has the end of
// the run before it. After the start and after each run the next instant of
// a task is the first one strictly after the time of the clock; missed
// instants are not caught up. While a task waits, the clock is read at least
// once a minute, so a run starts when the time of the clock reaches its
// instant, also after a step of the system time. Its methods are safe for
// use by several goroutines, and it calls the store without its lock.
type Scheduler struct {
	clock Clock
	store Store

	mu       sync.Mutex
	tasks    map[string]*entry
	order    []*entry // the tasks in the order of Add
	starting bool     // Start ends the open runs of the store
	started  bool
	closed   bool
	runs     sync.WaitGroup // the runs from the moment they hold their place
}

type entry struct {
	task    Task
	trigger Trigger
	timer   Timer         // set while the task waits for an instant
	armed   int           // counts the timers; the call of an older one does nothing
	next    time.Time     // the instant of timer
	last    time.Time     // the instant of the last start by the schedule
	run     *run          // set from the start of a run until its function returns
	ending  chan struct{} // closed once the store has taken or refused the end of the last run
	refused *runEnd       // the end of a run the store refused, offered again before the next start
	held    bool          // a removal waits for its commit; the task neither waits nor starts
}

type run struct {
	id      int64 // set once the store has recorded the start
	cancel  context.CancelFunc
	stopped bool
}

// runEnd is the end of a run as the store records it.
type runEnd struct {
	run     int64
	at      time.Time
	result  string
	message string
}

// New is a scheduler that takes its time from clock and records runs in
// store. Both must not be nil.
func New(clock Clock, store Store) *Scheduler {
	return &Scheduler{clock: clock, store: store, tasks: map[string]*entry{}}
}

// Add takes a task. Its schedule is checked as Schedule.Trigger does, and a
// refusal is a FieldError. A task that Add takes after Start waits for its
// first instant at once.
func (s *Scheduler) Add(t Task) error {
	if t.ID == "" {
		return errors.New("a task needs an id")
	}
	if t.Run == nil {
		return fmt.Errorf("task %q has no function", t.ID)
	}
	trigger, err := t.Schedule.Trigger()
	if err != nil {
		return fmt.Errorf("task %q: %w", t.ID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch _, ok := s.tasks[t.ID]; {
	case s.closed:
		return ErrClosed
	case ok:
		return fmt.Errorf("task %q: %w", t.ID, ErrDuplicate)
	}
	e := &entry{task: t, trigger: trigger}
	s.tasks[t.ID] = e
	s.order = append(s.order, e)
	s.arm(e)
	return nil
}

// Start ends every run of the store without an end as INTERRUPTED, then
// lets the tasks wait for their instants.
func (s *Scheduler) Start() error {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return ErrClosed
	case s.started || s.starting:
		s.mu.Unlock()
		return ErrStarted
	}
	s.starting = true
	s.mu.Unlock()
	_, err := s.store.EndOpenRuns(s.clock.Now().UTC(), ResultInterrupted, interrupted)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starting = false
	switch {
	case err != nil:
		return fmt.Errorf("the runs without an end: %w", err)
	case s.closed:
		return ErrClosed
	}
	s.started = true
	for _, e := range s.order {
		s.arm(e)
	}
	return nil
}

// Run starts a run of the task now. It refuses a task that is disabled or
// running.
func (s *Scheduler) Run(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(id)
	switch {
	case err != nil:
		return err
	case s.closed:
		return ErrClosed
	case !s.started:
		return ErrNotStarted
	case !e.task.Enabled:
		return fmt.Errorf("task %q: %w", id, ErrDisabled)
	case e.run != nil, e.held:
		return fmt.Errorf("task %q: %w", id, ErrRunning)
	}
	return s.begin(e)
}

// Stop cancels the context of the run of the task. The run is recorded as
// CANCELED when its function returns. It refuses a task that is not
// running.
func (s *Scheduler) Stop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(id)
	if err != nil {
		return err
	}
	if e.run == nil {
		return fmt.Errorf("task %q: %w", id, ErrNotRunning)
	}
	e.run.stopped = true
	e.run.cancel()
	return nil
}

// Running is true from the start of a run of the task until its function
// returns.
func (s *Scheduler) Running(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.tasks[id]
	return e != nil && e.run != nil
}

// Next is the instant the task waits for. It is false while the task runs,
// when it is disabled, before Start, and when its schedule has no instant
// left.
func (s *Scheduler) Next(id string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.tasks[id]
	if e == nil || e.timer == nil {
		return time.Time{}, false
	}
	return e.next, true
}

// Close stops every timer and every run, as Stop does, and waits until the
// functions of the runs have returned and the store has their ends, or ctx
// is done. It offers an end the store refused once more. Nothing starts
// after Close.
func (s *Scheduler) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	for _, e := range s.order {
		s.disarm(e)
		if e.run != nil {
			e.run.stopped = true
			e.run.cancel()
		}
	}
	order := slices.Clone(s.order)
	s.mu.Unlock()
	ended := make(chan struct{})
	go func() {
		s.runs.Wait()
		for _, e := range order {
			if err := s.offer(e); err != nil {
				slog.Warn(err.Error(), "logger", loggerName)
			}
		}
		close(ended)
	}()
	select {
	case <-ended:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("runs still going: %w", ctx.Err())
	}
}

func (s *Scheduler) entry(id string) (*entry, error) {
	e := s.tasks[id]
	if e == nil {
		return nil, fmt.Errorf("task %q: %w", id, ErrUnknownTask)
	}
	return e, nil
}

// arm lets e wait for its next instant: the first strictly after the time
// of the clock, and after the last instant the schedule started, should the
// clock have gone back. It does nothing while e may not start.
func (s *Scheduler) arm(e *entry) {
	s.disarm(e)
	if !s.started || s.closed || !e.task.Enabled || e.run != nil || e.held {
		return
	}
	now := s.clock.Now()
	from := now
	if e.last.After(from) {
		from = e.last
	}
	next, ok := e.trigger.Next(from)
	if !ok {
		return
	}
	e.next = next
	s.wait(e, now)
}

// wait sets the timer of e toward e.next, for maxWait at most.
func (s *Scheduler) wait(e *entry, now time.Time) {
	armed := e.armed
	e.timer = s.clock.AfterFunc(min(e.next.Sub(now), maxWait), func() { s.fire(e, armed) })
}

func (s *Scheduler) disarm(e *entry) {
	if e.timer != nil {
		e.timer.Stop()
	}
	e.timer, e.next = nil, time.Time{}
	e.armed++
}

// fire starts the run of an instant once the time of the clock has reached
// it, and else waits on; a run the store does not record does not start,
// and the task waits for the instant after.
func (s *Scheduler) fire(e *entry, armed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.armed != armed || e.timer == nil || s.closed {
		return
	}
	if now := s.clock.Now(); now.Before(e.next) {
		s.wait(e, now)
		return
	}
	e.last = e.next
	e.timer, e.next = nil, time.Time{}
	if err := s.begin(e); err != nil {
		slog.Warn(fmt.Sprintf("the run of %s at %s did not start: %v", e.task.ID, e.last.Format(time.RFC3339), err), "logger", loggerName)
	}
}

// begin records a run of e and starts its function. It is called with the
// lock held and returns with it held. The run holds its place while it
// waits, without the lock, until the store has the end of the run before,
// has taken an end it refused before, and has recorded this start. A run
// the store does not record does not start, and e waits for its next
// instant.
func (s *Scheduler) begin(e *entry) error {
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel}
	s.disarm(e)
	e.run = r
	s.runs.Add(1)
	before := e.ending
	s.mu.Unlock()
	if before != nil {
		<-before
	}
	err := s.offer(e)
	var id int64
	if err == nil {
		if id, err = s.store.BeginRun(e.task.ID, s.clock.Now().UTC()); err != nil {
			err = fmt.Errorf("task %q: the run was not recorded: %w", e.task.ID, err)
		}
	}
	s.mu.Lock()
	if err != nil {
		cancel()
		e.run = nil
		s.runs.Done()
		s.arm(e)
		return err
	}
	r.id = id
	go s.execute(ctx, e, r, e.task.Run)
	return nil
}

// offer gives the store the end it refused for e, if there is one, without
// the lock.
func (s *Scheduler) offer(e *entry) error {
	s.mu.Lock()
	end := e.refused
	s.mu.Unlock()
	if end == nil {
		return nil
	}
	if err := s.store.EndRun(end.run, end.at, end.result, end.message); err != nil {
		return fmt.Errorf("task %q: the store refused the end of run %d again: %w", e.task.ID, end.run, err)
	}
	s.mu.Lock()
	e.refused = nil
	s.mu.Unlock()
	return nil
}

// execute calls the function of a run. When it returns, the task waits for
// its next instant, and the end goes to the store without the lock; the
// next start of the task waits for that. An end the store refuses is kept
// for the next start and for Close.
func (s *Scheduler) execute(ctx context.Context, e *entry, r *run, work Func) {
	defer s.runs.Done()
	message, err := call(ctx, work)
	ended := s.clock.Now().UTC()
	s.mu.Lock()
	r.cancel()
	result := ResultOK
	switch {
	case r.stopped:
		result = ResultCanceled
	case err != nil:
		result = ResultFailed
	}
	recorded := make(chan struct{})
	e.ending = recorded
	e.run = nil
	s.arm(e)
	s.mu.Unlock()
	defer close(recorded)
	if err != nil {
		message = err.Error()
	}
	if result == ResultCanceled && message == "" {
		message = "stopped"
	}
	if err := s.store.EndRun(r.id, ended, result, message); err != nil {
		slog.Warn(fmt.Sprintf("the store refused the end of run %d of %s, which the next start offers again: %v", r.id, e.task.ID, err), "logger", loggerName)
		s.mu.Lock()
		e.refused = &runEnd{run: r.id, at: ended, result: result, message: message}
		s.mu.Unlock()
	}
}

// call calls f; a panic of f is an error.
func call(ctx context.Context, f Func) (message string, err error) {
	defer func() {
		if p := recover(); p != nil {
			message, err = "", fmt.Errorf("the task panicked: %v", p)
		}
	}()
	return f(ctx)
}

// Replace takes the schedule, enabled and the work of t for the task of its
// id. A run that is going keeps the work it started with; the task waits
// for an instant of the new schedule after it. The schedule is checked as
// Add checks it.
func (s *Scheduler) Replace(t Task) error {
	if t.Run == nil {
		return fmt.Errorf("task %q has no function", t.ID)
	}
	trigger, err := t.Schedule.Trigger()
	if err != nil {
		return fmt.Errorf("task %q: %w", t.ID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(t.ID)
	switch {
	case err != nil:
		return err
	case s.closed:
		return ErrClosed
	}
	// the id stays, so a run that reads it needs no lock
	e.task.Schedule, e.task.Enabled, e.task.Run = t.Schedule, t.Enabled, t.Run
	e.trigger = trigger
	s.arm(e)
	return nil
}

// Remove takes the task away once commit, called without the lock, has
// returned nil. A running task is refused with ErrRunning and commit is not
// called; while commit runs the task neither waits nor starts. Before
// commit the store has the end of the last run: Remove waits for it and
// offers an end the store refused once more; while the store refuses it,
// Remove gives that error and commit is not called. A commit that fails
// leaves the task as it was and gives its error.
func (s *Scheduler) Remove(id string, commit func() error) error {
	s.mu.Lock()
	e, err := s.entry(id)
	switch {
	case err != nil:
		s.mu.Unlock()
		return err
	case e.run != nil || e.held:
		s.mu.Unlock()
		return fmt.Errorf("task %q: %w", id, ErrRunning)
	}
	e.held = true
	s.disarm(e)
	ending := e.ending
	s.mu.Unlock()
	if ending != nil {
		<-ending
	}
	if err = s.offer(e); err == nil {
		err = commit()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.held = false
	if err != nil {
		s.arm(e)
		return err
	}
	delete(s.tasks, id)
	s.order = slices.DeleteFunc(s.order, func(x *entry) bool { return x == e })
	return nil
}
