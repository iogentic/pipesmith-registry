package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	statefile "github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

const tasksPath = "/service/rest/v1/tasks"

// memRuns keeps the runs of the scheduler in memory and answers the last
// run of each task, as state.Store does.
type memRuns struct {
	mu    sync.Mutex
	runs  []statefile.TaskRun
	ended chan statefile.TaskRun
	reads []string // the tasks of each read of the last runs, * for all
}

func newMemRuns() *memRuns { return &memRuns{ended: make(chan statefile.TaskRun, 10)} }

func (m *memRuns) BeginRun(taskID string, started time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, statefile.TaskRun{ID: int64(len(m.runs) + 1), TaskID: taskID, Started: started.UTC()})
	return int64(len(m.runs)), nil
}

func (m *memRuns) EndRun(runID int64, ended time.Time, result, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.runs {
		if m.runs[i].ID == runID && m.runs[i].Ended.IsZero() {
			m.runs[i].Ended, m.runs[i].Result, m.runs[i].Message = ended.UTC(), result, message
			m.ended <- m.runs[i]
			return nil
		}
	}
	return errors.New("no open run of that id")
}

func (m *memRuns) EndOpenRuns(time.Time, string, string) (int64, error) { return 0, nil }

// LastRun is the last run of one task; reads lists the tasks it was asked
// for.
func (m *memRuns) LastRun(taskID string) (statefile.TaskRun, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads = append(m.reads, taskID)
	for i := len(m.runs) - 1; i >= 0; i-- {
		if m.runs[i].TaskID == taskID {
			return m.runs[i], true, nil
		}
	}
	return statefile.TaskRun{}, false, nil
}

func (m *memRuns) LastRuns() (map[string]statefile.TaskRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads = append(m.reads, "*")
	last := map[string]statefile.TaskRun{}
	for _, r := range m.runs {
		last[r.TaskID] = r
	}
	return last, nil
}

func (m *memRuns) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runs)
}

// taskStart is the time of the fake clock of the tasks tests.
var taskStart = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// taskApp is a server with security on whose registry holds three tasks:
// demo-local of an injected kind that runs until it is stopped, demo-daily
// of that kind waiting for 11:00, and the imported demo-cron of a type
// without a kind. started gets the id of each run that begins.
func taskApp(t *testing.T, sec *fakeSecurity) (*secureApp, *memRuns, chan string) {
	t.Helper()
	runs, started := newMemRuns(), make(chan string, 4)
	kind := func(task model.Task) (tasks.Job, error) {
		return tasks.Job{Run: func(ctx context.Context) (string, error) {
			started <- task.ID
			<-ctx.Done()
			return "", nil
		}}, nil
	}
	registry := tasks.NewRegistry(tasks.NewFakeClock(taskStart), runs, tasks.RegistryOptions{Kinds: map[string]tasks.Kind{"demo.kind": kind}})
	err := registry.Load([]model.Task{
		{ID: "demo-local", Name: "Invented local task", TypeID: "demo.kind", Enabled: true, Schedule: model.TaskSchedule{Type: tasks.TypeManual},
			Origin: model.OriginLocal},
		{ID: "demo-daily", Name: "Invented daily task", TypeID: "demo.kind", Enabled: true,
			Schedule: model.TaskSchedule{Type: tasks.TypeDaily, StartAt: taskStart.Add(time.Hour)}, Origin: model.OriginLocal},
		{ID: "demo-cron", Name: "Invented task demo-cron", TypeID: "blobstore.compact", Schedule: model.TaskSchedule{Type: tasks.TypeCron, Cron: "0 0 2 * * ?"},
			Origin: model.OriginImport},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := registry.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return &secureApp{app: startTasksApp(t, sec, registry, runs), sec: sec}, runs, started
}

// taskItems is the list of the answer, by id, and its continuation token.
func taskItems(t *testing.T, a *answer) (map[string]map[string]any, any) {
	t.Helper()
	status(t, a, 200)
	var page struct {
		Items             []map[string]any `json:"items"`
		ContinuationToken any              `json:"continuationToken"`
	}
	dec := json.NewDecoder(strings.NewReader(string(a.body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&page); err != nil {
		t.Fatalf("%v %s", err, a.body)
	}
	items := map[string]map[string]any{}
	for _, item := range page.Items {
		keys := slices.Sorted(func(yield func(string) bool) {
			for k := range item {
				if !yield(k) {
					return
				}
			}
		})
		if !slices.Equal(keys, []string{"currentState", "id", "lastRun", "lastRunResult", "message", "name", "nextRun", "schedule", "type"}) {
			t.Fatalf("keys %v", keys)
		}
		items[item["id"].(string)] = item
	}
	return items, page.ContinuationToken
}

func receiveID(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no run started")
	}
	return ""
}

// The tasks of contract S38 and S40: the list and one task in the shape of
// TaskXO, a run and a stop of a task of an injected kind with 204 at a fake
// clock, the run of a task without a kind 405 with its reason (ruling SB1),
// the stop of an idle task 409, an unknown task 404. Each run and stop ends
// in the hook of a change (ruling SB3).
func TestTasks(t *testing.T) {
	sec := newFakeSecurity()
	a, runs, started := taskApp(t, sec)
	items, token := taskItems(t, a.send("GET", tasksPath, "basic", asAdmin))
	if len(items) != 3 || token != nil {
		t.Fatalf("%v %v", items, token)
	}
	cron, daily, local := items["demo-cron"], items["demo-daily"], items["demo-local"]
	if cron["name"] != "Invented task demo-cron" || cron["type"] != "blobstore.compact" || cron["message"] != "the task type blobstore.compact is not built yet; leg 6-cleanup builds it" ||
		cron["currentState"] != "WAITING" || cron["lastRunResult"] != nil || cron["nextRun"] != nil || cron["lastRun"] != nil || cron["schedule"] != "cron" {
		t.Fatalf("demo-cron %v", cron)
	}
	if daily["nextRun"] != "2026-11-19T11:00:00.000Z" || daily["message"] != nil || daily["schedule"] != "daily" {
		t.Fatalf("demo-daily %v", daily)
	}
	runs.mu.Lock()
	runs.reads = nil
	runs.mu.Unlock()
	one := a.send("GET", tasksPath+"/demo-cron", "basic", asAdmin)
	status(t, one, 200)
	runs.mu.Lock()
	reads := slices.Clone(runs.reads)
	runs.mu.Unlock()
	if !slices.Equal(reads, []string{"demo-cron"}) {
		t.Fatalf("the read of one task read the runs of %v (ruling SB12)", reads)
	}
	if got := one.object(t); got["message"] != cron["message"] || got["id"] != "demo-cron" || len(got) != len(cron) {
		t.Fatalf("one task %v", got)
	}
	if r := a.send("GET", tasksPath+"/demo-none", "basic", asAdmin); r.status != 404 || r.object(t)["message"] != "no task has the id demo-none" {
		t.Fatalf("unknown task %d %s", r.status, r.body)
	}
	_ = local

	disabled := a.send("POST", tasksPath+"/demo-cron/run", "basic", asAdmin)
	if disabled.status != 405 || disabled.object(t)["message"] != "the task type blobstore.compact is not built yet; leg 6-cleanup builds it" {
		t.Fatalf("run of demo-cron %d %s", disabled.status, disabled.body)
	}
	idle := a.send("POST", tasksPath+"/demo-local/stop", "basic", asAdmin)
	if idle.status != 409 || idle.object(t)["message"] != "the task demo-local is not running" {
		t.Fatalf("stop of an idle task %d %s", idle.status, idle.body)
	}
	if r := a.send("POST", tasksPath+"/demo-none/run", "basic", asAdmin); r.status != 404 {
		t.Fatalf("run of an unknown task %d %s", r.status, r.body)
	}
	if runs.count() != 0 {
		t.Fatal("a refused run was recorded")
	}
	_, resets := sec.writes()

	run := a.send("POST", tasksPath+"/demo-local/run", "basic", asAdmin)
	if run.status != 204 || len(run.body) != 0 {
		t.Fatalf("run %d %s", run.status, run.body)
	}
	if id := receiveID(t, started); id != "demo-local" {
		t.Fatalf("started %s", id)
	}
	items, _ = taskItems(t, a.send("GET", tasksPath, "basic", asAdmin))
	if items["demo-local"]["currentState"] != "RUNNING" || items["demo-local"]["lastRun"] != "2026-11-19T10:00:00.000Z" ||
		items["demo-local"]["lastRunResult"] != nil {
		t.Fatalf("running %v", items["demo-local"])
	}
	if again := a.send("POST", tasksPath+"/demo-local/run", "basic", asAdmin); again.status != 409 {
		t.Fatalf("second run %d %s", again.status, again.body)
	}
	stop := a.send("POST", tasksPath+"/demo-local/stop", "basic", asAdmin)
	if stop.status != 204 || len(stop.body) != 0 {
		t.Fatalf("stop %d %s", stop.status, stop.body)
	}
	select {
	case <-runs.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
	after := a.send("GET", tasksPath+"/demo-local", "basic", asAdmin).object(t)
	if after["currentState"] != "WAITING" || after["lastRunResult"] != "CANCELED" || after["message"] != "stopped" {
		t.Fatalf("after the stop %v", after)
	}
	if _, now := sec.writes(); now != resets+2 {
		t.Fatalf("%d hooks for a run and a stop, want 2", now-resets)
	}
}

// A subject without the permissions of the tasks gets 403 for each of the
// four requests and starts nothing; a session needs the request token for
// a run and a stop (A22); a subject with only nexus:tasks:read reads and
// may neither run nor stop.
func TestTasksRights(t *testing.T) {
	sec := newFakeSecurity()
	sec.addBasic("demo-tasks-reader:tasks-pw", subject("demo-tasks-reader", "", "nexus:tasks:read"))
	sec.addBasic("demo-tasks-runner:runner-pw", subject("demo-tasks-runner", "", "nexus:tasks:start", "nexus:tasks:stop"))
	a, runs, _ := taskApp(t, sec)
	for _, who := range []string{asReader, asNobody, "demo-tasks-reader:tasks-pw", "demo-tasks-runner:runner-pw"} {
		for _, c := range []struct{ method, target, permission string }{
			{"GET", tasksPath, "nexus:tasks:read"}, {"GET", tasksPath + "/demo-local", "nexus:tasks:read"},
			{"POST", tasksPath + "/demo-local/run", "nexus:tasks:start"}, {"POST", tasksPath + "/demo-local/stop", "nexus:tasks:stop"},
		} {
			holds := who == "demo-tasks-reader:tasks-pw" && c.method == "GET" || who == "demo-tasks-runner:runner-pw" && c.method == "POST"
			if holds {
				continue
			}
			r := a.send(c.method, c.target, "basic", who)
			if r.status != 403 || r.object(t)["message"] != "this needs the permission "+c.permission {
				t.Fatalf("%s %s by %s: %d %s", c.method, c.target, who, r.status, r.body)
			}
		}
	}
	status(t, a.send("GET", tasksPath, "basic", "demo-tasks-reader:tasks-pw"), 200)
	if runs.count() != 0 {
		t.Fatal("a refused request started a run")
	}
	for _, target := range []string{tasksPath + "/demo-local/run", tasksPath + "/demo-local/stop"} {
		for _, headers := range [][]string{{"Cookie", "fake-session=s1"}, {"Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1x"}} {
			r := do(t, a.addr(), "POST", target, nil, headers...)
			if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
				t.Fatalf("%s %v: %d %s", target, headers, r.status, r.body)
			}
		}
	}
	if runs.count() != 0 {
		t.Fatal("a run without the request token started")
	}
	// with the token the stop of the idle task passes the gate and the scheduler refuses it
	r := do(t, a.addr(), "POST", tasksPath+"/demo-local/stop", nil, "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1")
	if r.status != 409 {
		t.Fatalf("stop with the token %d %s", r.status, r.body)
	}
}

// Without a registry, a server whose state keeps no tasks, the four
// requests answer 503 and the request token still counts first.
func TestTasksWithoutARegistry(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	for _, c := range []struct{ method, target string }{
		{"GET", tasksPath}, {"GET", tasksPath + "/demo-local"}, {"POST", tasksPath + "/demo-local/run"}, {"POST", tasksPath + "/demo-local/stop"},
	} {
		r := a.send(c.method, c.target, "basic", asAdmin)
		if r.status != 503 || r.object(t)["message"] != "the tasks of this server are not available" {
			t.Fatalf("%s %s: %d %s", c.method, c.target, r.status, r.body)
		}
	}
}

// startTasksApp is startSecured with a registry of tasks and the reader of
// its runs.
func startTasksApp(t *testing.T, sec *fakeSecurity, registry *tasks.Registry, runs TaskRuns) *app {
	t.Helper()
	base := t.TempDir()
	stores := []config.StoreConfig{blobStore(t, base, "store-a", spec("demo-maven", jarPath, []byte("jar bytes"), "application/java-archive"))}
	opts := appOptions{defs: []*pyjson.Object{def("demo-maven", "maven2", "hosted")}, security: sec, tasks: registry, taskRuns: runs}
	return startApp(t, base, prepare(t, base, stores, opts), opts)
}

// failingRuns is memRuns whose end of the open runs fails, so that the
// scheduler does not start.
type failingRuns struct{ *memRuns }

func (failingRuns) EndOpenRuns(time.Time, string, string) (int64, error) {
	return 0, errors.New("the file is locked")
}

// A scheduler that does not start stops the start of Serve before it
// answers, and the port is given back.
func TestServeRefusesAStartOfAFailedScheduler(t *testing.T) {
	sec := newFakeSecurity()
	registry := tasks.NewRegistry(tasks.NewFakeClock(taskStart), failingRuns{newMemRuns()}, tasks.RegistryOptions{})
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	cfg := serveConfig(t)
	cfg.Port = freePort(t)
	err := Serve(context.Background(), cfg, ServeOptions{Security: sec, Identity: sec, Tasks: registry,
		Ready: func([]net.Addr) { t.Error("ready") }})
	if err == nil || err.Error() != "the task scheduler did not start: the runs without an end: the file is locked" {
		t.Fatalf("err %v", err)
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		t.Fatalf("the port stays taken: %v", err)
	}
	l.Close()
}

// The server is the host of the kinds of its registry once it is built,
// and Close takes it away (leg 6-kinds).
func TestTheServerIsTheHostOfItsKinds(t *testing.T) {
	ref := &tasks.HostRef{}
	registry := tasks.NewRegistry(tasks.NewFakeClock(taskStart), newMemRuns(), tasks.RegistryOptions{Kinds: tasks.Kinds(ref), Host: ref})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = registry.Close(ctx) // the test ends here; a failed close changes nothing it checked
	})
	if _, err := ref.Host(); !errors.Is(err, tasks.ErrNoHost) {
		t.Fatalf("a host before the server: %v", err)
	}
	a := startTasksApp(t, newFakeSecurity(), registry, newMemRuns())
	if h, err := ref.Host(); err != nil || h != tasks.Host(a.srv) {
		t.Fatalf("the host is %v, %v", h, err)
	}
	_ = a.srv.Close() // the cleanup of startApp closes it again; this test reads the host only
	if _, err := ref.Host(); !errors.Is(err, tasks.ErrNoHost) {
		t.Fatalf("the host after Close: %v", err)
	}
}
