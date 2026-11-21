package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The changes of leg 6-api (ruling AP5): the scheduler and the registry
// take a created, saved or removed task at once; a save of a running task
// applies from its next run; the removal of a running task is refused.

// drain takes the ends the store reports, so that a test of many runs
// does not fill the channel.
func drain(t *testing.T, store *memStore) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-store.ended:
			case <-stop:
				return
			}
		}
	}()
}

// signal is the work of a task that sends its word when it starts and
// then waits until it is stopped.
func signal(started chan<- string, word string) Func {
	return func(ctx context.Context) (string, error) {
		started <- word
		<-ctx.Done()
		return word, nil
	}
}

func TestSchedulerReplaceAppliesFromTheNextRun(t *testing.T) {
	clock, store, s := setUp(t, t0)
	started := make(chan string, 4)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeManual}, Enabled: true, Run: signal(started, "old")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Run("t1"); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, started, "start"); got != "old" {
		t.Fatalf("started %q", got)
	}
	daily := Schedule{Type: TypeDaily, StartAt: t0.Add(2 * time.Hour)}
	if err := s.Replace(Task{ID: "t1", Schedule: daily, Enabled: true, Run: signal(started, "new")}); err != nil {
		t.Fatal(err)
	}
	if !s.Running("t1") {
		t.Fatal("the save stopped the run that was going")
	}
	if _, ok := s.Next("t1"); ok {
		t.Fatal("a running task waits for an instant")
	}
	if err := s.Stop("t1"); err != nil {
		t.Fatal(err)
	}
	if r := store.waitEnd(t); r.Message != "old" || r.Result != ResultCanceled {
		t.Fatalf("the run that was going ended %+v, want the work of before the save", r)
	}
	if next, ok := s.Next("t1"); !ok || !next.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("after the run the task waits for %s %v, want the instant of the saved schedule", next, ok)
	}
	clock.Advance(2 * time.Hour)
	if got := receive(t, started, "start"); got != "new" {
		t.Fatalf("the next run did %q, want the saved work", got)
	}
	if err := s.Replace(Task{ID: "none", Schedule: daily, Enabled: true, Run: done}); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("a save of an unknown task: %v", err)
	}
	var field *FieldError
	if err := s.Replace(Task{ID: "t1", Schedule: Schedule{Type: TypeWeekly}, Enabled: true, Run: done}); !errors.As(err, &field) {
		t.Fatalf("a save with a refused schedule: %v", err)
	}
}

func TestSchedulerReplaceDisablesAWaitingTask(t *testing.T) {
	_, _, s := setUp(t, t0)
	daily := Schedule{Type: TypeDaily, StartAt: t0.Add(time.Hour)}
	if err := s.Add(Task{ID: "t1", Schedule: daily, Enabled: true, Run: done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Next("t1"); !ok {
		t.Fatal("the task waits for no instant")
	}
	if err := s.Replace(Task{ID: "t1", Schedule: daily, Enabled: false, Run: done}); err != nil {
		t.Fatal(err)
	}
	if next, ok := s.Next("t1"); ok {
		t.Fatalf("a task saved disabled waits for %s", next)
	}
	if err := s.Run("t1"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("the run of a task saved disabled: %v", err)
	}
}

func TestSchedulerRemoveRefusesARunningTask(t *testing.T) {
	_, store, s := setUp(t, t0)
	started := make(chan string, 4)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeDaily, StartAt: t0.Add(time.Hour)}, Enabled: true,
		Run: signal(started, "run")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Run("t1"); err != nil {
		t.Fatal(err)
	}
	receive(t, started, "start")
	commits := 0
	commit := func() error { commits++; return nil }
	if err := s.Remove("t1", commit); !errors.Is(err, ErrRunning) || commits != 0 {
		t.Fatalf("the removal of a running task: %v, %d commits", err, commits)
	}
	if err := s.Stop("t1"); err != nil {
		t.Fatal(err)
	}
	store.waitEnd(t)
	// a commit that fails leaves the task as it was
	if err := s.Remove("t1", func() error { return errors.New("the file is locked") }); err == nil || err.Error() != "the file is locked" {
		t.Fatalf("a failed commit: %v", err)
	}
	if next, ok := s.Next("t1"); !ok || !next.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after a failed commit the task waits for %s %v", next, ok)
	}
	if err := s.Remove("t1", commit); err != nil || commits != 1 {
		t.Fatalf("the removal of an idle task: %v, %d commits", err, commits)
	}
	if err := s.Run("t1"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("the run of a removed task: %v", err)
	}
	if s.Running("t1") {
		t.Fatal("a removed task runs")
	}
	if err := s.Remove("t1", commit); !errors.Is(err, ErrUnknownTask) || commits != 1 {
		t.Fatalf("a second removal: %v, %d commits", err, commits)
	}
}

// The registry takes a create, a save and a removal at once, each after
// its commit; a commit that fails changes nothing.
func TestRegistryTakesAChangeAtOnce(t *testing.T) {
	started := make(chan string, 4)
	_, store, r := setUpRegistry(t, map[string]Kind{"demo.kind": blocking(started)}, "")
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	task := stored("demo-new", "demo.kind", model.OriginLocal, true, manual)
	if err := r.Create(func() (model.Task, error) { return model.Task{}, errors.New("refused") }); err == nil {
		t.Fatal("a create whose commit fails passed")
	}
	if len(r.List()) != 0 {
		t.Fatalf("a failed create registered %v", r.List())
	}
	if err := r.Create(func() (model.Task, error) { return task, nil }); err != nil {
		t.Fatal(err)
	}
	if s, ok := r.Status("demo-new"); !ok || !s.Enabled || s.Name != "Task demo-new" {
		t.Fatalf("created %+v %v", s, ok)
	}
	if err := r.Run("demo-new"); err != nil {
		t.Fatalf("the run of a created task: %v", err)
	}
	receive(t, started, "start")
	if err := r.Stop("demo-new"); err != nil {
		t.Fatal(err)
	}
	store.waitEnd(t)
	saved := task
	saved.Name, saved.Enabled = "Saved", false
	if err := r.Save("demo-new", func() (model.Task, error) { return saved, nil }); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.Status("demo-new"); s.Enabled || s.Name != "Saved" || s.Reason != "the task demo-new is disabled" {
		t.Fatalf("saved %+v", s)
	}
	if err := r.Save("none", func() (model.Task, error) { return saved, nil }); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("a save of an unknown task: %v", err)
	}
	if err := r.Remove("demo-new", func() error { return errors.New("refused") }); err == nil {
		t.Fatal("a removal whose commit fails passed")
	}
	if _, ok := r.Status("demo-new"); !ok {
		t.Fatal("a failed removal removed the task")
	}
	if err := r.Remove("demo-new", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Status("demo-new"); ok {
		t.Fatal("a removed task has a status")
	}
	if err := r.Run("demo-new"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("the run of a removed task: %v", err)
	}
}

// An imported task is paused (I12) until it is saved enabled as a task of
// origin local; then it runs.
func TestRegistrySaveOfAnImportedTaskLeavesThePausedState(t *testing.T) {
	started := make(chan string, 4)
	_, _, r := setUpRegistry(t, map[string]Kind{"demo.kind": blocking(started)}, "")
	imported := stored("demo-imported", "demo.kind", model.OriginImport, false, manual)
	if err := r.Load([]model.Task{imported}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.Status("demo-imported"); s.Enabled || s.Reason != "the task demo-imported was imported and stays paused until it is saved enabled" {
		t.Fatalf("imported %+v", s)
	}
	saved := imported
	saved.Origin, saved.Enabled = model.OriginLocal, true
	if err := r.Save("demo-imported", func() (model.Task, error) { return saved, nil }); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.Status("demo-imported"); !s.Enabled || s.Reason != "" {
		t.Fatalf("saved enabled %+v", s)
	}
	if err := r.Run("demo-imported"); err != nil {
		t.Fatalf("the run of the saved task: %v", err)
	}
	receive(t, started, "start")
}

// A race of saves and runs of one task: every run starts with the work of
// one save, no run overlaps another, and the last save holds.
func TestRegistrySaveRacesARun(t *testing.T) {
	kind := func(t model.Task) (Job, error) {
		return Job{Run: func(context.Context) (string, error) { return t.Name, nil }}, nil
	}
	_, store, r := setUpRegistry(t, map[string]Kind{"demo.kind": kind}, "")
	drain(t, store)
	task := stored("demo-race", "demo.kind", model.OriginLocal, true, manual)
	if err := r.Load([]model.Task{task}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	const rounds = 200
	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range rounds {
			if err := r.Run("demo-race"); err != nil && !errors.Is(err, ErrRunning) {
				errs <- fmt.Errorf("run: %w", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range rounds {
			next := task
			next.Name = fmt.Sprintf("save %d", i)
			if i%2 == 1 {
				next.Schedule = model.TaskSchedule{Type: TypeDaily, StartAt: t0.Add(time.Hour)}
			}
			if err := r.Save("demo-race", func() (model.Task, error) { return next, nil }); err != nil {
				errs <- fmt.Errorf("save: %w", err)
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if s, _ := r.Status("demo-race"); s.Name != fmt.Sprintf("save %d", rounds-1) || s.Schedule.Type != TypeDaily {
		t.Fatalf("after the race %+v", s)
	}
	if o := store.overlapping(); len(o) > 0 {
		t.Fatalf("runs overlapped: %v", o)
	}
}

// A race of a removal and a run: the removal is refused while the run goes,
// and no run starts once the removal is committed.
func TestRegistryRemoveRacesARun(t *testing.T) {
	var mu sync.Mutex
	committed := map[string]bool{}
	var late []string
	kind := func(task model.Task) (Job, error) {
		return Job{Run: func(context.Context) (string, error) {
			mu.Lock()
			if committed[task.ID] {
				late = append(late, task.ID)
			}
			mu.Unlock()
			return "ran", nil
		}}, nil
	}
	_, store, r := setUpRegistry(t, map[string]Kind{"demo.kind": kind}, "")
	drain(t, store)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		id := fmt.Sprintf("demo-%d", i)
		if err := r.Create(func() (model.Task, error) {
			return stored(id, "demo.kind", model.OriginLocal, true, manual), nil
		}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 5 {
				if err := r.Run(id); err != nil && !errors.Is(err, ErrRunning) && !errors.Is(err, ErrUnknownTask) {
					t.Errorf("run %s: %v", id, err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for {
				err := r.Remove(id, func() error {
					mu.Lock()
					committed[id] = true
					mu.Unlock()
					return nil
				})
				if err == nil {
					return
				}
				if !errors.Is(err, ErrRunning) {
					t.Errorf("remove %s: %v", id, err)
					return
				}
			}
		}()
		wg.Wait()
		if _, ok := r.Status(id); ok {
			t.Fatalf("%s is still registered", id)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(late) > 0 {
		t.Fatalf("runs started after the removal was committed: %v", late)
	}
}

// Finding F4 of the review: a removal waits for the end of the last run
// and offers an end the store refused before its commit; while the store
// refuses it again, the removal fails and commit is not called.
func TestSchedulerRemoveOffersARefusedEnd(t *testing.T) {
	_, store, s := setUp(t, t0)
	if err := s.Add(Task{ID: "t1", Schedule: Schedule{Type: TypeManual}, Enabled: true, Run: done}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	store.failEnd(2)
	if err := s.Run("t1"); err != nil {
		t.Fatal(err)
	}
	receive(t, store.refused, "the refused end of the run")
	commits := 0
	commit := func() error {
		commits++
		if rows := store.rows(); len(rows) != 1 || rows[0].Ended.IsZero() || rows[0].Result != ResultOK {
			t.Errorf("the commit of the removal ran before the store had the end: %+v", rows)
		}
		return nil
	}
	if err := s.Remove("t1", commit); err == nil || commits != 0 {
		t.Fatalf("a removal while the store refuses the end again: %v, %d commits", err, commits)
	}
	receive(t, store.refused, "the end refused a second time")
	if err := s.Remove("t1", commit); err != nil || commits != 1 {
		t.Fatalf("the removal once the store takes the end: %v, %d commits", err, commits)
	}
	if rows := store.rows(); len(rows) != 1 || rows[0].Result != ResultOK || rows[0].Message != "done" {
		t.Fatalf("the run of the removed task ended %+v, want OK", rows)
	}
}

// Finding F5 of the review: a create or a save whose change is stored is
// answered as a success when the scheduler is closed already (shutdown);
// the registry shows the stored change.
func TestRegistryChangeAfterCloseIsASuccess(t *testing.T) {
	started := make(chan string, 4)
	_, _, r := setUpRegistry(t, map[string]Kind{"demo.kind": blocking(started)}, "")
	task := stored("demo-kept", "demo.kind", model.OriginLocal, true, manual)
	if err := r.Load([]model.Task{task}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	created := stored("demo-late", "demo.kind", model.OriginLocal, true, manual)
	if err := r.Create(func() (model.Task, error) { return created, nil }); err != nil {
		t.Fatalf("a stored create after the close: %v", err)
	}
	if s, ok := r.Status("demo-late"); !ok || s.Name != "Task demo-late" {
		t.Fatalf("the stored create is not registered: %+v %v", s, ok)
	}
	saved := task
	saved.Name = "Saved late"
	if err := r.Save("demo-kept", func() (model.Task, error) { return saved, nil }); err != nil {
		t.Fatalf("a stored save after the close: %v", err)
	}
	if s, _ := r.Status("demo-kept"); s.Name != "Saved late" {
		t.Fatalf("the stored save is not registered: %+v", s)
	}
	if err := r.Run("demo-kept"); err == nil {
		t.Fatal("a run started after the close")
	}
}

// Finding F7 of the review: while the commit of a removal runs, the task
// neither starts on request (Run answers ErrRunning) nor waits for an
// instant, also when a save arms it; after a failed commit it waits again.
func TestSchedulerRemoveHoldsTheTaskDuringItsCommit(t *testing.T) {
	clock, _, s := setUp(t, t0)
	started := make(chan string, 4)
	daily := Schedule{Type: TypeDaily, StartAt: t0.Add(time.Hour)}
	if err := s.Add(Task{ID: "t1", Schedule: daily, Enabled: true, Run: signal(started, "run")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	held := func() error {
		if err := s.Run("t1"); !errors.Is(err, ErrRunning) {
			t.Errorf("a run during the commit: %v, want ErrRunning", err)
		}
		if err := s.Replace(Task{ID: "t1", Schedule: daily, Enabled: true, Run: signal(started, "saved")}); err != nil {
			t.Errorf("a save during the commit: %v", err)
		}
		if next, ok := s.Next("t1"); ok {
			t.Errorf("the task waits for %s during the commit", next)
		}
		clock.Advance(2 * time.Hour)
		select {
		case word := <-started:
			t.Errorf("the task started %q during the commit", word)
		default:
		}
		return errors.New("the file is locked")
	}
	if err := s.Remove("t1", held); err == nil || err.Error() != "the file is locked" {
		t.Fatalf("the removal with a failed commit: %v", err)
	}
	if next, ok := s.Next("t1"); !ok || !next.Equal(t0.Add(25*time.Hour)) {
		t.Fatalf("after the failed commit the task waits for %s %v, want the next instant", next, ok)
	}
	if err := s.Remove("t1", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	clock.Advance(48 * time.Hour)
	select {
	case word := <-started:
		t.Fatalf("a removed task started %q", word)
	default:
	}
}

// Finding F7 of the review: a race of runs on the schedule, started by the
// timers of the fake clock, against saves and a removal. Tasks run on their
// schedule, no run overlaps another, and no run starts once the removal is
// committed.
func TestRegistryScheduledRunsRaceSavesAndRemove(t *testing.T) {
	var mu sync.Mutex
	committed := map[string]bool{}
	var late []string
	kind := func(task model.Task) (Job, error) {
		return Job{Run: func(context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if committed[task.ID] {
				late = append(late, task.ID)
			}
			return task.Name, nil
		}}, nil
	}
	clock, store, r := setUpRegistry(t, map[string]Kind{"demo.kind": kind}, "")
	drain(t, store)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	hourly := model.TaskSchedule{Type: TypeHourly, StartAt: t0.Add(time.Hour)}
	for i := range 40 {
		id := fmt.Sprintf("demo-%d", i)
		task := stored(id, "demo.kind", model.OriginLocal, true, hourly)
		if err := r.Create(func() (model.Task, error) { return task, nil }); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(3)
		first := make(chan struct{})
		go func() {
			defer wg.Done()
			for j := range 5 {
				clock.Advance(time.Hour)
				if j == 0 {
					close(first)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := range 5 {
				next := task
				next.Name = fmt.Sprintf("save %d", j)
				if err := r.Save(id, func() (model.Task, error) { return next, nil }); err != nil && !errors.Is(err, ErrUnknownTask) {
					t.Errorf("save %s: %v", id, err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-first
			for {
				err := r.Remove(id, func() error {
					mu.Lock()
					committed[id] = true
					mu.Unlock()
					return nil
				})
				if err == nil {
					return
				}
				if !errors.Is(err, ErrRunning) {
					t.Errorf("remove %s: %v", id, err)
					return
				}
			}
		}()
		wg.Wait()
		if _, ok := r.Status(id); ok {
			t.Fatalf("%s is still registered", id)
		}
	}
	clock.Advance(24 * time.Hour)
	mu.Lock()
	defer mu.Unlock()
	if len(late) > 0 {
		t.Fatalf("runs started after the removal was committed: %v", late)
	}
	if o := store.overlapping(); len(o) > 0 {
		t.Fatalf("runs overlapped: %v", o)
	}
	ran := map[string]bool{}
	for _, row := range store.rows() {
		ran[row.Task] = true
	}
	// a save that comes at an instant waits for the instant after it, so
	// not every task needs a run here
	if len(ran) == 0 {
		t.Fatal("no task ran on the schedule")
	}
}
