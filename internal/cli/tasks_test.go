package cli

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// serve registers the stored tasks and starts the scheduler once the
// server is built: a run without an end is INTERRUPTED before the first
// answer, and the log counts the tasks.
func TestServeStartsTheTaskScheduler(t *testing.T) {
	dir := importedFixture(t)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = st.PutTask(model.Task{ID: "demo-local-task", Name: "Invented task", TypeID: "blobstore.compact", Enabled: true,
		Schedule: model.TaskSchedule{Type: tasks.TypeManual}, Properties: map[string]string{}, Origin: model.OriginLocal})
	var open int64
	if err == nil {
		open, err = st.BeginRun("demo-local-task", time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC))
	}
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	_, stop := serveData(t)
	st, err = state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := st.TaskRuns("demo-local-task")
	_ = st.Close()
	if err != nil || len(runs) != 1 || runs[0].ID != open || runs[0].Result != tasks.ResultInterrupted {
		t.Fatalf("runs %+v %v", runs, err)
	}
	log := strings.Join(stop(), "\n")
	if !strings.Contains(log, "tasks: 1 registered, 1 disabled") {
		t.Fatalf("log %q", log)
	}
}

// A run that never ends keeps serve from stopping no longer than the bound
// of the close of the tasks plus the drain of the listeners, and the tasks
// close before the state file.
func TestServeClosesTheTasksWithinTheBound(t *testing.T) {
	dir := importedFixture(t)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = st.PutTask(model.Task{ID: "demo-never", Name: "Invented task", TypeID: "demo.never", Enabled: true,
		Schedule: model.TaskSchedule{Type: tasks.TypeManual}, Properties: map[string]string{}, Origin: model.OriginLocal})
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() { close(release) })
	kinds := taskKinds
	taskKinds = func(ref *tasks.HostRef) map[string]tasks.Kind {
		m := kinds(ref)
		m["demo.never"] = func(model.Task) (tasks.Job, error) {
			return tasks.Job{Run: func(context.Context) (string, error) {
				started <- struct{}{}
				<-release // the context is ignored
				return "", nil
			}}, nil
		}
		return m
	}
	t.Cleanup(func() { taskKinds = kinds })
	bound := tasksClose
	tasksClose = time.Second
	t.Cleanup(func() { tasksClose = bound })
	var mu sync.Mutex
	var order []string
	closing = func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}
	t.Cleanup(func() { closing = func(string) {} })

	base, stop := serveData(t)
	admin := map[string]string{"Authorization": "Basic " + basicValue("admin", "example-admin-1")}
	if resp, body := call(t, "POST", base+"/service/rest/v1/tasks/demo-never/run", nil, admin); resp.StatusCode != 204 {
		t.Fatalf("run %d %s", resp.StatusCode, body)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not start")
	}
	begun := time.Now()
	log := strings.Join(stop(), "\n")
	if took := time.Since(begun); took > tasksClose+4*time.Second {
		t.Fatalf("serve stopped after %s", took)
	}
	mu.Lock()
	got := strings.Join(order, " ")
	mu.Unlock()
	if got != "tasks security" {
		t.Fatalf("closed in the order %q", got)
	}
	if !strings.Contains(log, "the task scheduler did not close within 1s") {
		t.Fatalf("log %q", log)
	}
}

// The secrets of a task are the values of its properties that the secret
// word rule marks and the values of its secret files (ruling SB11).
func TestTaskSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteSecret(state.PropertySecretPath(dir, state.SecretKindTask, "demo", "remotePassword"), []byte("example-file-secret")); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteSecret(state.PropertySecretPath(dir, state.SecretKindTask, "demo-other", "token"), []byte("example-other")); err != nil {
		t.Fatal(err)
	}
	got := taskSecrets(dir)(model.Task{ID: "demo", Properties: map[string]string{"apiToken": "example-property-secret", "location": "/srv/demo"}})
	slices.Sort(got)
	if !slices.Equal(got, []string{"example-file-secret", "example-property-secret"}) {
		t.Fatalf("%v", got)
	}
}

// serve registers the kinds of the built types of the table (leg 6-kinds).
func TestTaskKindsAreTheTable(t *testing.T) {
	var built []string
	for _, tt := range tasks.TypeList() {
		built = append(built, tt.ID)
	}
	slices.Sort(built)
	got := slices.Sorted(maps.Keys(taskKinds(&tasks.HostRef{})))
	if len(got) == 0 || !slices.Equal(got, built) {
		t.Fatalf("kinds %v, want %v", got, built)
	}
}

// Finding 7b-3: a run of serve writes its start and end lines to tasks.log
// of the log directory, through the registry that taskRegistry builds.
func TestServeRunsWriteTasksLog(t *testing.T) {
	dir := importedFixture(t)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = st.PutTask(model.Task{ID: "demo-quick", Name: "Invented task", TypeID: "demo.quick", Enabled: true,
		Schedule: model.TaskSchedule{Type: tasks.TypeManual}, Properties: map[string]string{}, Origin: model.OriginLocal})
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	kinds := taskKinds
	taskKinds = func(ref *tasks.HostRef) map[string]tasks.Kind {
		m := kinds(ref)
		m["demo.quick"] = func(model.Task) (tasks.Job, error) {
			return tasks.Job{Run: func(context.Context) (string, error) { return "done", nil }}, nil
		}
		return m
	}
	t.Cleanup(func() { taskKinds = kinds })
	base, stop := serveData(t)
	admin := map[string]string{"Authorization": "Basic " + basicValue("admin", "example-admin-1")}
	if resp, body := call(t, "POST", base+"/service/rest/v1/tasks/demo-quick/run", nil, admin); resp.StatusCode != 204 {
		t.Fatalf("run %d %s", resp.StatusCode, body)
	}
	path := filepath.Join(dir, "logs", "tasks.log")
	deadline := time.Now().Add(5 * time.Second)
	var events []string
	for {
		events = events[:0]
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var e struct {
				RunID  string `json:"run_id"`
				TaskID string `json:"task_id"`
				Event  string `json:"event"`
				Result string `json:"result"`
			}
			if json.Unmarshal([]byte(line), &e) == nil && e.TaskID == "demo-quick" && e.RunID != "" {
				events = append(events, e.Event+" "+e.Result)
			}
		}
		if len(events) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	if strings.Join(events, ",") != "start ,end OK" {
		t.Fatalf("tasks.log events %q", events)
	}
}
