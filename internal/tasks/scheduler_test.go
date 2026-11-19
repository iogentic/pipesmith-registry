package tasks

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// guard bounds every wait for a goroutine; it only keeps a broken test from
// hanging.
const guard = 5 * time.Second

// memRun is one row of memStore.
type memRun struct {
	ID      int64
	Task    string
	Started time.Time
	Ended   time.Time // zero while open
	Result  string
	Message string
}

// memStore keeps runs in memory and sends every closed row on ended.
type memStore struct {
	mu       sync.Mutex
	runs     []memRun
	ended    chan memRun
	failNext error            // the next BeginRun returns it
	held     map[string]*gate // the next call of "begin" or "end" waits at its gate
	failEnds int              // the next calls of EndRun fail, this many
	refused  chan int64       // the run of each EndRun that failed
	overlaps []string         // a task that got a row while one of its rows had no end
}

// gate holds one call of the store: entered is closed when the call waits,
// and the call goes on once open is closed.
type gate struct{ entered, open chan struct{} }

func newMemStore() *memStore {
	return &memStore{ended: make(chan memRun, 100), held: map[string]*gate{}, refused: make(chan int64, 100)}
}

// hold makes the next call of op, "begin" or "end", wait at a gate.
func (m *memStore) hold(op string) *gate {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := &gate{entered: make(chan struct{}), open: make(chan struct{})}
	m.held[op] = g
	return g
}

func (m *memStore) wait(op string) {
	m.mu.Lock()
	g := m.held[op]
	delete(m.held, op)
	m.mu.Unlock()
	if g != nil {
		close(g.entered)
		<-g.open
	}
}

func (m *memStore) BeginRun(taskID string, started time.Time) (int64, error) {
	m.wait("begin")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failNext; err != nil {
		m.failNext = nil
		return 0, err
	}
	for _, r := range m.runs {
		if r.Task == taskID && r.Ended.IsZero() {
			m.overlaps = append(m.overlaps, taskID)
		}
	}
	m.runs = append(m.runs, memRun{ID: int64(len(m.runs) + 1), Task: taskID, Started: started})
	return int64(len(m.runs)), nil
}

func (m *memStore) EndRun(runID int64, ended time.Time, result, message string) error {
	m.wait("end")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failEnds > 0 {
		m.failEnds--
		m.refused <- runID
		return errors.New("the file is locked")
	}
	for i := range m.runs {
		if m.runs[i].ID == runID && m.runs[i].Ended.IsZero() {
			m.runs[i].Ended, m.runs[i].Result, m.runs[i].Message = ended, result, message
			m.ended <- m.runs[i]
			return nil
		}
	}
	return errors.New("no open run of that id")
}

func (m *memStore) EndOpenRuns(ended time.Time, result, message string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for i := range m.runs {
		if m.runs[i].Ended.IsZero() {
			m.runs[i].Ended, m.runs[i].Result, m.runs[i].Message = ended, result, message
			n++
		}
	}
	return n, nil
}

func (m *memStore) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext = err
}

// failEnd makes the next n calls of EndRun fail.
func (m *memStore) failEnd(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failEnds = n
}

func (m *memStore) overlapping() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.overlaps)
}

func (m *memStore) rows() []memRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.runs)
}

// waitEnd is the next row the store closes.
func (m *memStore) waitEnd(t *testing.T) memRun {
	t.Helper()
	select {
	case r := <-m.ended:
		return r
	case <-time.After(guard):
		t.Fatalf("no run ended within %s", guard)
	}
	return memRun{}
}

// receive is the next value of ch.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(guard):
		t.Fatalf("no %s within %s", what, guard)
	}
	var zero T
	return zero
}

// t0 is the time of the fake clock when a test starts: Thursday 2026-11-19,
// 10:00 UTC.
var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func setUp(t *testing.T, now time.Time) (*FakeClock, *memStore, *Scheduler) {
	t.Helper()
	clock, store := NewFakeClock(now), newMemStore()
	s := New(clock, store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), guard)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return clock, store, s
}

func done(context.Context) (string, error) { return "done", nil }

// advanceTo moves the clock to at, which must be the instant the task
// waits for, and waits for the run it starts to end.
func advanceTo(t *testing.T, clock *FakeClock, store *memStore, s *Scheduler, id string, at time.Time) memRun {
	t.Helper()
	if next, ok := s.Next(id); !ok || !next.Equal(at) {
		t.Fatalf("the task waits for %s %v, want %s", next, ok, at)
	}
	clock.Advance(at.Sub(clock.Now()))
	r := store.waitEnd(t)
	if r.Task != id || !r.Started.Equal(at) || r.Result != "OK" {
		t.Fatalf("run %+v, want one of %s started at %s with OK", r, id, at)
	}
	return r
}

// Each of the seven types starts its runs at the instants of the rulings at
// the fake clock, and only there.
func TestSchedulerFiresEachTypeAtItsInstants(t *testing.T) {
	for _, c := range []struct {
		name     string
		schedule Schedule
		next     []string
		then     string // the instant after the last of next, or none
	}{
		{"manual", Schedule{Type: TypeManual}, nil, "none"},
		{"once", Schedule{Type: TypeOnce, StartAt: at(t, "2026-11-20T22:00:00Z")}, []string{"2026-11-20T22:00:00Z"}, "none"},
		{"once, startAt before the scheduler takes the task", Schedule{Type: TypeOnce, StartAt: at(t, "2026-11-19T09:00:00Z")},
			nil, "none"},
		{"once, startAt the moment the scheduler takes the task", Schedule{Type: TypeOnce, StartAt: t0}, nil, "none"},
		{"hourly", Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-19T10:15:30Z")},
			[]string{"2026-11-19T10:15:30Z", "2026-11-19T11:15:30Z", "2026-11-19T12:15:30Z"}, "2026-11-19T13:15:30Z"},
		{"hourly from a startAt in the past", Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-18T08:45:00Z")},
			[]string{"2026-11-19T10:45:00Z", "2026-11-19T11:45:00Z"}, "2026-11-19T12:45:00Z"},
		{"daily", Schedule{Type: TypeDaily, StartAt: at(t, "2026-09-01T01:00:00Z")},
			[]string{"2026-11-20T01:00:00Z", "2026-11-21T01:00:00Z"}, "2026-11-22T01:00:00Z"},
		{"weekly", Schedule{Type: TypeWeekly, StartAt: at(t, "2026-11-19T12:00:00Z"), WeekDays: []string{"MON", "FRI"}},
			[]string{"2026-11-20T12:00:00Z", "2026-11-23T12:00:00Z", "2026-11-27T12:00:00Z"}, "2026-11-30T12:00:00Z"},
		{"monthly", Schedule{Type: TypeMonthly, StartAt: at(t, "2026-11-19T03:00:00Z"), MonthDays: []int{1, 31}},
			[]string{"2026-12-01T03:00:00Z", "2026-12-31T03:00:00Z", "2027-01-01T03:00:00Z"}, "2027-01-31T03:00:00Z"},
		{"cron", Schedule{Type: TypeCron, Cron: "0 0 4 ? * MON-FRI"},
			[]string{"2026-11-20T04:00:00Z", "2026-11-23T04:00:00Z"}, "2026-11-24T04:00:00Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			clock, store, s := setUp(t, t0)
			if err := s.Add(Task{ID: "t1", Schedule: c.schedule, Enabled: true, Run: done}); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			for _, want := range c.next {
				advanceTo(t, clock, store, s, "t1", at(t, want))
			}
			next, ok := s.Next("t1")
			if c.then == "none" {
				if ok {
					t.Fatalf("the task waits for %s, want no instant", next)
				}
				clock.Advance(40 * 24 * time.Hour)
				if rows := store.rows(); len(rows) != len(c.next) {
					t.Fatalf("%d runs, want %d: %+v", len(rows), len(c.next), rows)
				}
				return
			}
			if !ok || !next.Equal(at(t, c.then)) {
				t.Fatalf("then %s %v, want %s", next, ok, c.then)
			}
			if rows := store.rows(); len(rows) != len(c.next) {
				t.Fatalf("%d runs, want %d", len(rows), len(c.next))
			}
		})
	}
}

// A clock that reads another zone moves no instant: the tasks fire at the
// instants in UTC, and the store gets the times in UTC.
func TestSchedulerAtAClockInAnotherZone(t *testing.T) {
	friday := time.Date(2026, 11, 20, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))
	clock, store, s := setUp(t, friday)
	thursday := at(t, "2026-11-19T23:45:00Z")
	for _, task := range []Task{
		{ID: "weekly", Schedule: Schedule{Type: TypeWeekly, StartAt: at(t, "2026-11-01T23:45:00Z"), WeekDays: []string{"THU"}}},
		{ID: "cron", Schedule: Schedule{Type: TypeCron, Cron: "0 45 23 ? * THU"}},
	} {
		task.Enabled, task.Run = true, done
		if err := s.Add(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"weekly", "cron"} {
		if next, ok := s.Next(id); !ok || !next.Equal(thursday) {
			t.Fatalf("%s waits for %s %v, want %s", id, next, ok, thursday)
		}
	}
	clock.Advance(15 * time.Minute)
	for range 2 {
		r := store.waitEnd(t)
		if !r.Started.Equal(thursday) || r.Started.Location() != time.UTC || r.Ended.Location() != time.UTC || r.Result != "OK" {
			t.Fatalf("run %+v, want one started at %s in UTC", r, thursday)
		}
	}
}

// A step of the system time while a task waits moves no run: it starts when
// the time of the clock reaches its instant. After a step back during a run,
// the instant of that run does not come again.
func TestSchedulerFollowsAStepOfTheClock(t *testing.T) {
	midnight := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	three := midnight.Add(3 * time.Hour)
	daily := func(run Func) Task {
		return Task{ID: "daily", Schedule: Schedule{Type: TypeDaily, StartAt: at(t, "2026-11-01T03:00:00Z")}, Enabled: true, Run: run}
	}
	for _, step := range []time.Duration{time.Hour, -time.Hour} {
		t.Run("a step of "+step.String()+" at 01:00", func(t *testing.T) {
			clock, store, s := setUp(t, midnight)
			if err := s.Add(daily(done)); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Hour)
			clock.Step(step)
			clock.Advance(three.Sub(clock.Now()) - time.Second)
			if rows := store.rows(); len(rows) != 0 {
				t.Fatalf("runs %+v before 03:00 by the clock", rows)
			}
			clock.Advance(time.Second)
			if rows := store.rows(); len(rows) != 1 || !rows[0].Started.Equal(three) {
				t.Fatalf("runs %+v at 03:00 by the clock, want the one of 03:00", rows)
			}
			store.waitEnd(t)
		})
	}
	t.Run("a step back during a run", func(t *testing.T) {
		clock, store, s := setUp(t, midnight)
		release, started := make(chan struct{}), make(chan struct{}, 1)
		if err := s.Add(daily(func(context.Context) (string, error) {
			started <- struct{}{}
			<-release
			return "", nil
		})); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		clock.Advance(3 * time.Hour)
		receive(t, started, "the start of the run of 03:00")
		clock.Step(-time.Hour)
		close(release)
		store.waitEnd(t)
		if next, ok := s.Next("daily"); !ok || !next.Equal(three.Add(24*time.Hour)) {
			t.Fatalf("the task waits for %s %v at 02:00 by the clock, want 03:00 of the next day", next, ok)
		}
	})
}

// T9: a missed instant is not caught up. After the start the next instant
// is the first one after the clock; after a run, the first one after its
// end.
func TestSchedulerCatchesNothingUp(t *testing.T) {
	clock, store, s := setUp(t, t0)
	release := make(chan struct{})
	started := make(chan time.Time, 10)
	slow := func(ctx context.Context) (string, error) {
		started <- clock.Now()
		<-release
		return "slow", nil
	}
	if err := s.Add(Task{ID: "daily", Schedule: Schedule{Type: TypeDaily, StartAt: at(t, "2026-11-01T01:00:00Z")},
		Enabled: true, Run: done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Task{ID: "hourly", Schedule: Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-19T10:30:00Z")},
		Enabled: true, Run: slow}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if next, ok := s.Next("daily"); !ok || !next.Equal(at(t, "2026-11-20T01:00:00Z")) {
		t.Fatalf("daily waits for %s %v, want 2026-11-20T01:00:00Z: no run for the instants since 2026-11-01", next, ok)
	}
	clock.Advance(30 * time.Minute)
	receive(t, started, "start of the hourly run")
	clock.Advance(3*time.Hour + 40*time.Minute) // 14:10, past 11:30, 12:30 and 13:30
	close(release)
	r := store.waitEnd(t)
	if !r.Ended.Equal(at(t, "2026-11-19T14:10:00Z")) || r.Result != "OK" || r.Message != "slow" {
		t.Fatalf("run %+v", r)
	}
	if next, ok := s.Next("hourly"); !ok || !next.Equal(at(t, "2026-11-19T14:30:00Z")) {
		t.Fatalf("hourly waits for %s %v after a run that ended at 14:10, want 14:30", next, ok)
	}
	if rows := store.rows(); len(rows) != 1 {
		t.Fatalf("%d runs, want the one: %+v", len(rows), rows)
	}
}

// T10: one run per task at a time. An instant that comes while the run is
// going starts nothing and is not kept for later; a manual start of a
// running task is refused.
func TestSchedulerRunsATaskOnceAtATime(t *testing.T) {
	clock, store, s := setUp(t, t0)
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-19T11:00:00Z")}, Enabled: true,
		Run: func(ctx context.Context) (string, error) {
			started <- struct{}{}
			<-release
			return "", nil
		}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	receive(t, started, "start of the run")
	if !s.Running("t1") {
		t.Fatal("the task does not run at 11:00")
	}
	if next, ok := s.Next("t1"); ok {
		t.Fatalf("a running task waits for %s", next)
	}
	for range 3 {
		clock.Advance(time.Hour)
	}
	var refusal Refusal
	if err := s.Run("t1"); !errors.Is(err, ErrRunning) || !errors.As(err, &refusal) {
		t.Fatalf("Run of a running task: %v, want ErrRunning", err)
	}
	if rows := store.rows(); len(rows) != 1 {
		t.Fatalf("%d runs while the first went on, want 1", len(rows))
	}
	close(release)
	store.waitEnd(t)
	if s.Running("t1") {
		t.Fatal("the task still runs after its function returned")
	}
	if next, ok := s.Next("t1"); !ok || !next.Equal(at(t, "2026-11-19T15:00:00Z")) {
		t.Fatalf("the task waits for %s %v, want 15:00 and no run for 12:00 to 14:00", next, ok)
	}
	if rows := store.rows(); len(rows) != 1 {
		t.Fatalf("%d runs after the end, want 1: no queued run", len(rows))
	}
	clock.Advance(time.Hour)
	receive(t, started, "start of the run")
	if rows := store.rows(); len(rows) != 2 || !rows[1].Started.Equal(at(t, "2026-11-19T15:00:00Z")) {
		t.Fatalf("runs %+v, want the second at 15:00", rows)
	}
}

// A manual start runs the task at once; the schedule goes on after it.
func TestSchedulerRunsOnRequest(t *testing.T) {
	clock, store, s := setUp(t, t0)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeDaily, StartAt: at(t, "2026-11-19T12:00:00Z")}, Enabled: true,
		Run: done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run("t1"); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Run before Start: %v, want ErrNotStarted", err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if err := s.Run("t1"); err != nil {
		t.Fatal(err)
	}
	r := store.waitEnd(t)
	if !r.Started.Equal(t0.Add(time.Minute)) || r.Result != "OK" || r.Message != "done" {
		t.Fatalf("run %+v", r)
	}
	advanceTo(t, clock, store, s, "t1", at(t, "2026-11-19T12:00:00Z"))
}

// A task that Add takes after Start waits for its first instant at once and
// runs there; a disabled one waits for none.
func TestSchedulerTakesATaskAfterStart(t *testing.T) {
	clock, store, s := setUp(t, t0)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{
		{ID: "late", Schedule: Schedule{Type: TypeHourly, StartAt: t0.Add(30 * time.Minute)}, Enabled: true, Run: done},
		{ID: "off", Schedule: Schedule{Type: TypeHourly, StartAt: t0.Add(30 * time.Minute)}, Run: done},
	} {
		if err := s.Add(task); err != nil {
			t.Fatal(err)
		}
	}
	if next, ok := s.Next("off"); ok {
		t.Fatalf("a disabled task that Add took after Start waits for %s", next)
	}
	advanceTo(t, clock, store, s, "late", t0.Add(30*time.Minute))
	if next, ok := s.Next("late"); !ok || !next.Equal(t0.Add(90*time.Minute)) {
		t.Fatalf("late waits for %s %v after its run, want 11:30", next, ok)
	}
}

// T11: a disabled task runs neither on its schedule nor on request.
func TestSchedulerRunsNoDisabledTask(t *testing.T) {
	clock, store, s := setUp(t, t0)
	if err := s.Add(Task{ID: "off", Schedule: Schedule{Type: TypeHourly, StartAt: t0}, Enabled: false, Run: done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Task{ID: "on", Schedule: Schedule{Type: TypeHourly, StartAt: t0}, Enabled: true, Run: done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if next, ok := s.Next("off"); ok {
		t.Fatalf("a disabled task waits for %s", next)
	}
	var refusal Refusal
	if err := s.Run("off"); !errors.Is(err, ErrDisabled) || !errors.As(err, &refusal) {
		t.Fatalf("Run of a disabled task: %v, want ErrDisabled", err)
	}
	advanceTo(t, clock, store, s, "on", t0.Add(time.Hour))
	advanceTo(t, clock, store, s, "on", t0.Add(2*time.Hour))
	for _, r := range store.rows() {
		if r.Task != "on" {
			t.Fatalf("the disabled task ran: %+v", r)
		}
	}
}

// T12: Stop cancels the context of the run, and the row is closed as
// CANCELED within 1 s of real time. Stop of a task that does not run is
// refused.
func TestSchedulerStopCancelsWithinASecond(t *testing.T) {
	_, store, s := setUp(t, t0)
	started := make(chan struct{})
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeManual}, Enabled: true,
		Run: func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	var refusal Refusal
	if err := s.Stop("t1"); !errors.Is(err, ErrNotRunning) || !errors.As(err, &refusal) {
		t.Fatalf("Stop of a task that does not run: %v, want ErrNotRunning", err)
	}
	if err := s.Run("t1"); err != nil {
		t.Fatal(err)
	}
	receive(t, started, "start of the run")
	begin := time.Now()
	if err := s.Stop("t1"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-store.ended:
		if r.Result != "CANCELED" || r.Ended.IsZero() || r.Message == "" {
			t.Fatalf("run %+v, want CANCELED with an end and a message", r)
		}
	case <-time.After(time.Second):
		t.Fatal("the row was not closed within 1 s of the stop")
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("the stop took %s", took)
	}
	if err := s.Stop("t1"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop after the end: %v, want ErrNotRunning", err)
	}
}

// T13: a run that the start finds without an end is closed as INTERRUPTED
// at the time of the start; closed rows stay as they are.
func TestSchedulerStartInterruptsOpenRuns(t *testing.T) {
	_, store, s := setUp(t, t0)
	before := t0.Add(-time.Hour)
	if _, err := store.BeginRun("old", before); err != nil {
		t.Fatal(err)
	}
	closedID, err := store.BeginRun("old", before.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndRun(closedID, before, "OK", "fine"); err != nil {
		t.Fatal(err)
	}
	receive(t, store.ended, "end of the run")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	rows := store.rows()
	if r := rows[0]; !r.Ended.Equal(t0) || r.Result != "INTERRUPTED" || r.Message == "" {
		t.Fatalf("open run after the start: %+v, want INTERRUPTED at %s", r, t0)
	}
	if r := rows[1]; !r.Ended.Equal(before) || r.Result != "OK" || r.Message != "fine" {
		t.Fatalf("closed run after the start: %+v", r)
	}
	if err := s.Start(); err == nil {
		t.Fatal("a second Start succeeded")
	}
}

// T13: a run ends OK with the message of its function, FAILED with the text
// of its error, also after a panic.
func TestSchedulerRecordsResults(t *testing.T) {
	_, store, s := setUp(t, t0)
	for id, run := range map[string]Func{
		"ok":     func(context.Context) (string, error) { return "12 files", nil },
		"failed": func(context.Context) (string, error) { return "half", errors.New("the store is gone") },
		"panic":  func(context.Context) (string, error) { panic("broken") },
	} {
		if err := s.Add(Task{ID: id, Schedule: Schedule{Type: TypeManual}, Enabled: true, Run: run}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{"ok": {"OK", "12 files"}, "failed": {"FAILED", "the store is gone"},
		"panic": {"FAILED", "the task panicked: broken"}}
	for _, id := range []string{"ok", "failed", "panic"} {
		if err := s.Run(id); err != nil {
			t.Fatal(err)
		}
		r := store.waitEnd(t)
		if r.Task != id || r.Result != want[id][0] || r.Message != want[id][1] || !r.Started.Equal(t0) || !r.Ended.Equal(t0) {
			t.Fatalf("run %+v, want %s %v", r, id, want[id])
		}
	}
}

// Add refuses a task without an id, without a function, of an id it holds,
// or with a schedule its checks refuse; Run and Stop refuse an unknown task.
func TestSchedulerRefusals(t *testing.T) {
	_, _, s := setUp(t, t0)
	task := Task{ID: "t1", Schedule: Schedule{Type: TypeManual}, Enabled: true, Run: done}
	if err := s.Add(task); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(task); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Add of an id it holds: %v, want ErrDuplicate", err)
	}
	if err := s.Add(Task{Schedule: Schedule{Type: TypeManual}, Run: done}); err == nil {
		t.Fatal("Add of a task without an id succeeded")
	}
	if err := s.Add(Task{ID: "t2", Schedule: Schedule{Type: TypeManual}}); err == nil {
		t.Fatal("Add of a task without a function succeeded")
	}
	var field *FieldError
	if err := s.Add(Task{ID: "t3", Schedule: Schedule{Type: TypeWeekly, StartAt: t0}, Run: done}); !errors.As(err, &field) ||
		field.Field != "weekDays" {
		t.Fatalf("Add with a weekly schedule without days: %v, want a FieldError at weekDays", err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(string) error{"Run": s.Run, "Stop": s.Stop} {
		if err := call("t9"); !errors.Is(err, ErrUnknownTask) {
			t.Errorf("%s of an unknown task: %v, want ErrUnknownTask", name, err)
		}
	}
	if s.Running("t9") {
		t.Fatal("an unknown task runs")
	}
	if _, ok := s.Next("t9"); ok {
		t.Fatal("an unknown task waits for an instant")
	}
}

// A run that the store cannot record does not start; the schedule goes on.
func TestSchedulerSkipsARunTheStoreRefuses(t *testing.T) {
	clock, store, s := setUp(t, t0)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeHourly, StartAt: t0}, Enabled: true, Run: done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	store.fail(errors.New("disk full"))
	if err := s.Run("t1"); err == nil {
		t.Fatal("Run succeeded while the store refused the row")
	}
	store.fail(errors.New("disk full"))
	clock.Advance(time.Hour)
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("runs %+v, want none", rows)
	}
	advanceTo(t, clock, store, s, "t1", t0.Add(2*time.Hour))
}

// The store is called without the lock of the scheduler. While the store
// holds the start or the end of the run of one task, Stop, Running, Next
// and Run answer at once, and the run that Stop cancels ends within 1 s.
// The task holds its place while its start is recorded and waits for its
// next instant while its end is recorded.
func TestSchedulerHoldsNoLockWhileTheStoreWorks(t *testing.T) {
	for _, op := range []string{"begin", "end"} {
		t.Run(op, func(t *testing.T) {
			clock, store, s := setUp(t, t0)
			waits := func(ctx context.Context) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}
			for _, task := range []Task{
				{ID: "a", Schedule: Schedule{Type: TypeHourly, StartAt: t0.Add(time.Hour)}, Run: done},
				{ID: "b", Schedule: Schedule{Type: TypeManual}, Run: waits},
				{ID: "c", Schedule: Schedule{Type: TypeManual}, Run: done},
			} {
				task.Enabled = true
				if err := s.Add(task); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			if err := s.Run("b"); err != nil {
				t.Fatal(err)
			}
			held := store.hold(op)
			go clock.Advance(time.Hour)
			receive(t, held.entered, "the call of the store for the run of a")

			var late []chan struct{}
			quick := func(what string, f func()) {
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					f()
				}()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Errorf("%s waited for the store", what)
					late = append(late, finished)
				}
			}
			var stopped, ran error
			var running, waiting bool
			var next time.Time
			stop := time.Now()
			quick("Stop of b", func() { stopped = s.Stop("b") })
			quick("Running of a", func() { running = s.Running("a") })
			quick("Next of a", func() { next, waiting = s.Next("a") })
			quick("Run of c", func() { ran = s.Run("c") })
			ends := map[string]memRun{}
			for deadline := time.After(time.Second); ends["b"].Task == "" || ends["c"].Task == ""; {
				select {
				case r := <-store.ended:
					ends[r.Task] = r
				case <-deadline:
					t.Errorf("runs %+v within 1 s of the stop, want those of b and c", ends)
					ends["b"], ends["c"] = memRun{Task: "b"}, memRun{Task: "c"}
				}
			}
			took := time.Since(stop)
			close(held.open)
			for _, finished := range late {
				receive(t, finished, "the end of a call")
			}
			if t.Failed() {
				return
			}
			if stopped != nil || ran != nil {
				t.Fatalf("Stop %v and Run %v, want nil", stopped, ran)
			}
			starting := op == "begin"
			if running != starting || waiting == starting || waiting && !next.Equal(t0.Add(2*time.Hour)) {
				t.Fatalf("while the store holds the %s of a: running %v, waiting %v for %s", op, running, waiting, next)
			}
			if b, c := ends["b"], ends["c"]; b.Result != "CANCELED" || c.Result != "OK" || took > time.Second {
				t.Fatalf("b %+v and c %+v after %s, want CANCELED and OK within 1 s", b, c, took)
			}
			if a := store.waitEnd(t); a.Task != "a" || a.Result != "OK" || !a.Started.Equal(t0.Add(time.Hour)) {
				t.Fatalf("run %+v, want the one of a at 11:00", a)
			}
		})
	}
}

// An end that the store refuses is offered again before the next start of
// the task, so the row gets the end its run had and the task never has two
// rows without an end. While the store refuses it, the task starts no run;
// Close offers it once more.
func TestSchedulerOffersARefusedEndAgain(t *testing.T) {
	clock, store, s := setUp(t, t0)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeHourly, StartAt: t0.Add(time.Hour)}, Enabled: true,
		Run: func(context.Context) (string, error) { return "12 files", nil }}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	eleven, twelve, one := t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	store.failEnd(1)
	clock.Advance(time.Hour)
	receive(t, store.refused, "the refused end of the run of 11:00")
	if rows := store.rows(); len(rows) != 1 || !rows[0].Ended.IsZero() {
		t.Fatalf("runs %+v, want the one of 11:00 without an end", rows)
	}
	if next, ok := s.Next("t1"); s.Running("t1") || !ok || !next.Equal(twelve) {
		t.Fatalf("next %s %v, want the task to wait for 12:00", next, ok)
	}
	clock.Advance(time.Hour)
	if first := store.waitEnd(t); first.ID != 1 || !first.Ended.Equal(eleven) || first.Result != "OK" || first.Message != "12 files" {
		t.Fatalf("run %+v, want the one of 11:00 with the end it had; rows %+v", first, store.rows())
	}
	if second := store.waitEnd(t); !second.Started.Equal(twelve) || second.Result != "OK" {
		t.Fatalf("run %+v, want the one of 12:00", second)
	}

	store.failEnd(3)
	clock.Advance(time.Hour)
	receive(t, store.refused, "the refused end of the run of 13:00")
	clock.Advance(time.Hour)
	receive(t, store.refused, "the refused end offered again at 14:00")
	if err := s.Run("t1"); err == nil {
		t.Fatal("Run succeeded while the store refused the end of the run before")
	}
	receive(t, store.refused, "the refused end offered again by Run")
	if rows := store.rows(); len(rows) != 3 || !rows[2].Started.Equal(one) || !rows[2].Ended.IsZero() {
		t.Fatalf("runs %+v, want none after the one of 13:00, which has no end", rows)
	}
	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if r := store.waitEnd(t); r.ID != 3 || !r.Ended.Equal(one) || r.Result != "OK" || r.Message != "12 files" {
		t.Fatalf("run %+v after Close, want the one of 13:00 with its end", r)
	}
	if overlaps := store.overlapping(); len(overlaps) != 0 {
		t.Fatalf("a row began while another row of %v had no end", overlaps)
	}
}

// Close cancels the running runs, waits for their end, and nothing starts
// after it.
func TestSchedulerClose(t *testing.T) {
	clock, store := NewFakeClock(t0), newMemStore()
	s := New(clock, store)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeHourly, StartAt: t0}, Enabled: true,
		Run: func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Run("t1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := store.rows(); len(rows) != 1 || rows[0].Result != "CANCELED" || rows[0].Ended.IsZero() {
		t.Fatalf("runs %+v after Close, want one CANCELED", rows)
	}
	clock.Advance(48 * time.Hour)
	if err := s.Run("t1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after Close: %v, want ErrClosed", err)
	}
	if rows := store.rows(); len(rows) != 1 {
		t.Fatalf("runs after Close: %+v", rows)
	}
}
