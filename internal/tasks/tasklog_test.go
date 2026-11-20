package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// Task 7.19: with the fake clock, a run writes a start line and an end line
// with the same run id to tasks.log, and the end line holds the result of
// the row of the store. The store is memStore: task_runs of state.sqlite is
// not a step of its migrations before phase 6 merges (schema4.go).

type taskLine struct {
	RunID      string  `json:"run_id"`
	TaskID     string  `json:"task_id"`
	Event      string  `json:"event"`
	Result     string  `json:"result"`
	DurationMS float64 `json:"duration_ms"`
}

func readTaskLines(t *testing.T, dir string) []taskLine {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "tasks.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []taskLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line taskLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, line)
	}
	return out
}

// endedRun waits for the next row the store closes.
func endedRun(t *testing.T, store *memStore) memRun {
	t.Helper()
	select {
	case run := <-store.ended:
		return run
	case <-time.After(guard):
		t.Fatal("no run ended")
	}
	return memRun{}
}

func TestTaskLogStartAndEndOfARun(t *testing.T) {
	store := newMemStore()
	clock := NewFakeClock(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	dir := filepath.Join(t.TempDir(), "logs")
	files := logs.Open(logs.Options{Dir: dir, Now: clock.Now})
	sched := New(clock, Logged(store, files))
	release := make(chan struct{})
	for _, task := range []Task{
		{ID: "demo-ok", Schedule: Schedule{Type: TypeManual}, Enabled: true, Run: func(context.Context) (string, error) {
			<-release
			clock.Step(1500 * time.Millisecond)
			return "done", nil
		}},
		{ID: "demo-failing", Schedule: Schedule{Type: TypeManual}, Enabled: true, Run: func(context.Context) (string, error) {
			return "", errors.New("demo failure")
		}},
	} {
		if err := sched.Add(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	if err := sched.Run("demo-ok"); err != nil {
		t.Fatal(err)
	}
	close(release)
	okRun := endedRun(t, store)
	if err := sched.Run("demo-failing"); err != nil {
		t.Fatal(err)
	}
	failedRun := endedRun(t, store)
	if err := sched.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readTaskLines(t, dir)
	if len(lines) != 4 {
		t.Fatalf("%d lines, want 4: %+v", len(lines), lines)
	}
	for i, run := range []memRun{okRun, failedRun} {
		start, end := lines[2*i], lines[2*i+1]
		id := strconv.FormatInt(run.ID, 10)
		if start.Event != "start" || end.Event != "end" || start.RunID != id || end.RunID != id ||
			start.TaskID != run.Task || end.TaskID != run.Task {
			t.Errorf("run %+v: lines %+v %+v", run, start, end)
		}
		if end.Result != run.Result || start.Result != "" {
			t.Errorf("run %+v: end result %q, start result %q", run, end.Result, start.Result)
		}
	}
	if okRun.Task != "demo-ok" || okRun.Result != ResultOK || failedRun.Task != "demo-failing" || failedRun.Result != ResultFailed {
		t.Errorf("results %q %q", okRun.Result, failedRun.Result)
	}
	if lines[1].DurationMS != 1500 {
		t.Errorf("duration of the run of demo-ok %v ms, want 1500 by the fake clock", lines[1].DurationMS)
	}
}
