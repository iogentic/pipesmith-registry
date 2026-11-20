package tasks

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// blocking is a kind whose run waits until it is stopped; started gets a
// value when a run begins.
func blocking(started chan<- string) Kind {
	return func(t model.Task) (Job, error) {
		return Job{Run: func(ctx context.Context) (string, error) {
			started <- t.ID
			<-ctx.Done()
			return "", nil
		}}, nil
	}
}

func setUpRegistry(t *testing.T, kinds map[string]Kind, dataDir string) (*FakeClock, *memStore, *Registry) {
	t.Helper()
	clock, store := NewFakeClock(t0), newMemStore()
	r := NewRegistry(clock, store, RegistryOptions{Kinds: kinds, DataDir: dataDir})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), guard)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return clock, store, r
}

func stored(id, typeID, origin string, enabled bool, schedule model.TaskSchedule) model.Task {
	return model.Task{ID: id, Name: "Task " + id, TypeID: typeID, Enabled: enabled, Schedule: schedule,
		Properties: map[string]string{}, Origin: origin}
}

var manual = model.TaskSchedule{Type: TypeManual}

// Ruling SB1: every stored task is listed; a type without a kind, a
// schedule the scheduler refuses and an import are disabled with their
// reason, and a local task of a built kind is enabled.
func TestRegistryListsEveryStoredTask(t *testing.T) {
	started := make(chan string, 4)
	kinds := map[string]Kind{"demo.kind": blocking(started)}
	_, _, r := setUpRegistry(t, kinds, "")
	if err := r.Load([]model.Task{
		stored("e-local", "demo.kind", model.OriginLocal, true, model.TaskSchedule{Type: TypeDaily, StartAt: t0.Add(time.Hour)}),
		stored("b-unbuilt", "blobstore.compact", model.OriginLocal, true, manual),
		stored("c-stricter", "demo.kind", model.OriginLocal, true, model.TaskSchedule{Type: TypeCron, Cron: "0 0 12 15W,20 * ?"}),
		stored("d-imported", "demo.kind", model.OriginImport, true, manual),
		stored("a-off", "demo.kind", model.OriginLocal, false, manual),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	list := r.List()
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, " ") != "a-off b-unbuilt c-stricter d-imported e-local" {
		t.Fatalf("listed %v", ids)
	}
	want := map[string]struct {
		enabled bool
		reason  string
	}{
		"a-off":      {false, "the task a-off is disabled"},
		"b-unbuilt":  {false, "the task type blobstore.compact is not built yet; leg 6-cleanup builds it"},
		"d-imported": {false, "the task d-imported was imported and stays paused until it is saved enabled"},
		"e-local":    {true, ""},
		"c-stricter": {false, ""}, // the reason of the cron parser, checked below
	}
	for _, s := range list {
		w := want[s.ID]
		if s.Enabled != w.enabled {
			t.Errorf("%s enabled %v, want %v", s.ID, s.Enabled, w.enabled)
		}
		if s.ID != "c-stricter" && s.Reason != w.reason {
			t.Errorf("%s reason %q, want %q", s.ID, s.Reason, w.reason)
		}
	}
	stricter, ok := r.Status("c-stricter")
	if !ok || !strings.HasPrefix(stricter.Reason, "the schedule of the task c-stricter is refused: cron: ") {
		t.Errorf("c-stricter %+v %v", stricter, ok)
	}
	local, _ := r.Status("e-local")
	if !local.HasNext || !local.Next.Equal(t0.Add(time.Hour)) || local.Running || local.Schedule.Type != TypeDaily {
		t.Errorf("e-local %+v", local)
	}
	if off, _ := r.Status("a-off"); off.HasNext {
		t.Errorf("a disabled task waits for %s", off.Next)
	}
	if _, ok := r.Status("none"); ok {
		t.Error("a task that is not stored has a status")
	}
}

// The run of a disabled task is refused with ErrDisabled and its reason;
// an unknown task with ErrUnknownTask, and a stop of an idle task with
// ErrNotRunning.
func TestRegistryRefusals(t *testing.T) {
	_, store, r := setUpRegistry(t, map[string]Kind{}, "")
	if err := r.Load([]model.Task{stored("demo-cron", "blobstore.compact", model.OriginImport, false,
		model.TaskSchedule{Type: TypeCron, Cron: "0 0 2 * * ?"})}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	err := r.Run("demo-cron")
	var disabled *DisabledError
	if !errors.Is(err, ErrDisabled) || !errors.As(err, &disabled) || disabled.Reason != "the task type blobstore.compact is not built yet; leg 6-cleanup builds it" {
		t.Fatalf("run of a task without a kind: %v", err)
	}
	if err := r.Run("none"); !errors.Is(err, ErrUnknownTask) {
		t.Errorf("run of an unknown task: %v", err)
	}
	if err := r.Stop("demo-cron"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stop of an idle task: %v", err)
	}
	if err := r.Stop("none"); !errors.Is(err, ErrUnknownTask) {
		t.Errorf("stop of an unknown task: %v", err)
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Errorf("runs recorded: %+v", rows)
	}
}

// A run and a stop of a task of an injected kind succeed at a fake clock,
// and the run is recorded as CANCELED.
func TestRegistryRunAndStop(t *testing.T) {
	started := make(chan string, 1)
	_, store, r := setUpRegistry(t, map[string]Kind{"demo.kind": blocking(started)}, "")
	if err := r.Load([]model.Task{stored("demo-local", "demo.kind", model.OriginLocal, true, manual)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run("demo-local"); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("run before start: %v", err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run("demo-local"); err != nil {
		t.Fatal(err)
	}
	receive(t, started, "start of the run")
	if s, _ := r.Status("demo-local"); !s.Running {
		t.Fatal("the task does not run")
	}
	if err := r.Run("demo-local"); !errors.Is(err, ErrRunning) {
		t.Errorf("second run: %v", err)
	}
	if err := r.Stop("demo-local"); err != nil {
		t.Fatal(err)
	}
	row := store.waitEnd(t)
	if row.Task != "demo-local" || row.Result != ResultCanceled || !row.Started.Equal(t0) {
		t.Errorf("run %+v", row)
	}
}

// Ruling SB5: the message of a run names no path of the data directory and
// no secret of its task, and is cut to 1024 characters, before the store
// gets it and in Message.
func TestRegistryCleansMessages(t *testing.T) {
	dataDir := "/srv/demo-data"
	long := strings.Repeat("é", 2000)
	kinds := map[string]Kind{"demo.kind": func(t model.Task) (Job, error) {
		return Job{Secrets: []string{"example-secret-9f1"}, Run: func(context.Context) (string, error) {
			return "", errors.New("cannot open " + dataDir + "/blobs/x with example-secret-9f1 " + long)
		}}, nil
	}}
	_, store, r := setUpRegistry(t, kinds, dataDir)
	if err := r.Load([]model.Task{stored("demo-local", "demo.kind", model.OriginLocal, true, manual)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run("demo-local"); err != nil {
		t.Fatal(err)
	}
	row := store.waitEnd(t)
	for _, message := range []string{row.Message, r.Message("demo-local", "at "+dataDir+" example-secret-9f1 "+long)} {
		if strings.Contains(message, dataDir) || strings.Contains(message, "example-secret-9f1") {
			t.Errorf("message holds a path or a secret: %.120s", message)
		}
		if n := len([]rune(message)); n != MaxMessage {
			t.Errorf("message of %d characters, want %d", n, MaxMessage)
		}
	}
	if row.Result != ResultFailed || !strings.HasPrefix(row.Message, "cannot open <data>/blobs/x with *** ") {
		t.Errorf("run %s %.60s", row.Result, row.Message)
	}
	if short := r.Message("demo-local", "fine"); short != "fine" {
		t.Errorf("a short message became %q", short)
	}
}

// A task id is registered once.
func TestRegistryRefusesADuplicate(t *testing.T) {
	_, _, r := setUpRegistry(t, nil, "")
	err := r.Load([]model.Task{stored("demo", "x", model.OriginLocal, true, manual), stored("demo", "x", model.OriginLocal, true, manual)})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
}

// lockedBuffer is a buffer for a logger that several goroutines write.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// freeze is the state PauseWhile reads: frozen, and the number of the freeze.
type freeze struct {
	mu     sync.Mutex
	frozen bool
	number uint64
}

func (f *freeze) set(frozen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if frozen && !f.frozen {
		f.number++
	}
	f.frozen = frozen
}

func (f *freeze) state() (bool, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frozen, f.number
}

// Ruling SB8: while frozen an instant of a schedule is skipped, not caught
// up, and logged once per freeze; a run on request is refused; a run that
// is going when the freeze comes runs to its end; after the release the
// next instant runs.
func TestRegistrySkipsInstantsWhileFrozen(t *testing.T) {
	log := &lockedBuffer{}
	finish := make(chan struct{})
	kinds := map[string]Kind{
		"demo.kind": func(model.Task) (Job, error) { return Job{Run: done}, nil },
		"demo.slow": func(model.Task) (Job, error) {
			return Job{Run: func(context.Context) (string, error) { <-finish; return "finished", nil }}, nil
		},
	}
	clock, store := NewFakeClock(t0), newMemStore()
	r := NewRegistry(clock, store, RegistryOptions{Kinds: kinds, Log: slog.New(slog.NewTextHandler(log, nil))})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), guard)
		defer cancel()
		_ = r.Close(ctx)
	})
	f := &freeze{}
	r.PauseWhile(f.state)
	if err := r.Load([]model.Task{
		stored("demo-daily", "demo.kind", model.OriginLocal, true, model.TaskSchedule{Type: TypeDaily, StartAt: t0.Add(time.Hour)}),
		stored("demo-slow", "demo.slow", model.OriginLocal, true, manual),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run("demo-slow"); err != nil {
		t.Fatal(err)
	}
	f.set(true)
	if err := r.Run("demo-slow"); !errors.Is(err, ErrPaused) {
		t.Fatalf("run while frozen: %v", err)
	}
	close(finish)
	if row := store.waitEnd(t); row.Task != "demo-slow" || row.Result != ResultOK {
		t.Fatalf("the run of before the freeze %+v", row)
	}
	clock.Advance(time.Hour)
	clock.Advance(24 * time.Hour)
	for _, row := range store.rows() {
		if row.Task == "demo-daily" {
			t.Fatalf("a run while frozen: %+v", row)
		}
	}
	if next, ok := r.s.Next("demo-daily"); !ok || !next.Equal(t0.Add(49*time.Hour)) {
		t.Fatalf("the task waits for %s %v", next, ok)
	}
	if n := log.count("skipped while the server is frozen"); n != 1 {
		t.Fatalf("%d lines for one freeze", n)
	}
	f.set(false)
	f.set(true)
	clock.Advance(24 * time.Hour)
	if n := log.count("skipped while the server is frozen"); n != 2 {
		t.Fatalf("%d lines for two freezes", n)
	}
	f.set(false)
	clock.Advance(24 * time.Hour)
	if row := store.waitEnd(t); row.Task != "demo-daily" || !row.Started.Equal(t0.Add(73*time.Hour)) {
		t.Fatalf("after the release %+v", row)
	}
}

// Ruling SB11: the reason of a task that cannot run holds no value of a
// property the secret word rule marks; the values that RegistryOptions.Secrets
// gives are replaced before the reason is stored or answered.
func TestRegistryKeepsSecretsOutOfAReason(t *testing.T) {
	secret := "example-secret-5d2"
	kinds := map[string]Kind{"demo.kind": func(model.Task) (Job, error) {
		return Job{}, errors.New("the remote refused the password " + secret)
	}}
	clock, store := NewFakeClock(t0), newMemStore()
	r := NewRegistry(clock, store, RegistryOptions{Kinds: kinds, Secrets: func(task model.Task) []string {
		if task.ID == "demo-secret" {
			return []string{secret}
		}
		return nil
	}})
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	if err := r.Load([]model.Task{stored("demo-secret", "demo.kind", model.OriginLocal, true, manual)}); err != nil {
		t.Fatal(err)
	}
	s, _ := r.Status("demo-secret")
	if s.Enabled || strings.Contains(s.Reason, secret) || !strings.Contains(s.Reason, "the remote refused the password ***") {
		t.Fatalf("reason %q", s.Reason)
	}
	var disabled *DisabledError
	if err := r.Run("demo-secret"); !errors.As(err, &disabled) || strings.Contains(disabled.Reason, secret) {
		t.Fatalf("run %v", err)
	}
	if m := r.Message("demo-secret", "a message with "+secret); strings.Contains(m, secret) {
		t.Fatalf("message %q", m)
	}
}

// A panic of the work of a task fails its run, and its text is cleaned of
// the data directory and the secrets before the store records it.
func TestRegistryCleansAPanic(t *testing.T) {
	dataDir := "/srv/demo-data"
	kinds := map[string]Kind{"demo.kind": func(model.Task) (Job, error) {
		return Job{Secrets: []string{"example-secret-77"}, Run: func(context.Context) (string, error) {
			panic("cannot read " + dataDir + "/secrets/x holding example-secret-77")
		}}, nil
	}}
	_, store, r := setUpRegistry(t, kinds, dataDir)
	if err := r.Load([]model.Task{stored("demo-panic", "demo.kind", model.OriginLocal, true, manual)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run("demo-panic"); err != nil {
		t.Fatal(err)
	}
	row := store.waitEnd(t)
	if row.Result != ResultFailed || row.Message != "the task panicked: cannot read <data>/secrets/x holding ***" {
		t.Fatalf("%s %q", row.Result, row.Message)
	}
}
