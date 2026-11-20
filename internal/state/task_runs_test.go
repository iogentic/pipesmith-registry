package state

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// *Store records the runs of the scheduler.
var _ tasks.Store = (*Store)(nil)

// withRuns is the list steps with a count of the calls of its step 5 in
// applied.
func withRuns(applied *int) []migrate.Step {
	list := slices.Clone(steps)
	for i, step := range list {
		if step.Version == 4 {
			apply := step.Apply
			list[i].Apply = func(tx *sql.Tx) error {
				*applied++
				return apply(tx)
			}
		}
	}
	return list
}

// runsStore is a new state file that has every step and schema5.
func runsStore(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var applied int
	st, err := openWith(Path(dir), true, withRuns(&applied))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// tableRows is every row of a table as text, in the order of the dump.
func tableRows(t *testing.T, path, table string) string {
	t.Helper()
	var keep []string
	in := false
	for _, line := range strings.Split(dump(t, path, true), "\n") {
		switch {
		case strings.HasPrefix(line, "rows of "):
			in = line == "rows of "+table
		case in:
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// The step opens the released file of schema 1: task_runs exists with its
// columns and its index, the records of schema 1 are as they were, and a
// second open applies nothing.
func TestSchema5OnTheReleasedFile(t *testing.T) {
	dir := dataDirOf(t, "v1.sqlite")
	path := Path(dir)
	released := copied(t, path, filepath.Join(t.TempDir(), "released.sqlite"))
	var applied int
	st, err := openWith(path, false, withRuns(&applied))
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("the step ran %d times, want 1", applied)
	}
	sameRecords(t, got, recordsV1())
	for _, table := range []string{"users", "user_roles", "roles", "role_privileges", "role_roles", "privileges",
		"ldap_servers", "settings", "blob_stores", "repositories"} {
		if before, after := tableRows(t, released, table), tableRows(t, path, table); before != after || before == "" {
			t.Fatalf("rows of %s\nbefore\n%s\nafter\n%s", table, before, after)
		}
	}

	name, err := dsn(path, url.Values{"mode": {"ro"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var columns []string
	rows, err := db.Query("SELECT name, type, \"notnull\", pk FROM pragma_table_info('task_runs') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, kind string
		var notNull, pk int
		if err := rows.Scan(&name, &kind, &notNull, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, strings.Join([]string{name, kind, map[int]string{0: "null", 1: "not null"}[notNull],
			map[int]string{0: "", 1: "key"}[pk]}, " "))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"id INTEGER null key", "task_id TEXT not null ", "started_ms INTEGER not null ", "ended_ms INTEGER null ",
		"result TEXT null ", "message TEXT not null "}
	if !slices.Equal(columns, want) {
		t.Fatalf("columns of task_runs\n%q\nwant\n%q", columns, want)
	}
	var indexed []string
	rows, err = db.Query("SELECT i.name FROM pragma_index_list('task_runs') l, pragma_index_info(l.name) i ORDER BY i.seqno")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexed = append(indexed, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indexed, []string{"task_id", "started_ms"}) {
		t.Fatalf("index of task_runs over %v, want task_id, started_ms", indexed)
	}

	migrated := dump(t, path, true)
	again, err := openWith(path, false, withRuns(&applied))
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("the second open ran the step again (%d times)", applied)
	}
	if after := dump(t, path, true); after != migrated {
		t.Fatalf("the second open changed the file\n%s", after)
	}
	if _, err := os.Stat(path + ".v2"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the second open wrote a copy: %v", err)
	}
}

// BeginRun gives the id of an open run, EndRun closes it once, EndOpenRuns
// closes the open ones, and TaskRuns gives the runs of a task in the order
// of their start, in UTC milliseconds.
func TestTaskRunsAccessors(t *testing.T) {
	st := runsStore(t)
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	late, err := st.BeginRun("a", base.Add(2*time.Hour+123*time.Millisecond+456*time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	early, err := st.BeginRun("a", base.In(time.FixedZone("CET", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.BeginRun("b", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if early == late || other == late || other == early {
		t.Fatalf("ids %d, %d, %d are not distinct", late, early, other)
	}
	var open int
	if err := st.readers.QueryRow("SELECT count(*) FROM task_runs WHERE ended_ms IS NULL AND result IS NULL AND message = ''").
		Scan(&open); err != nil || open != 3 {
		t.Fatalf("%d runs with no end and no result, want 3: %v", open, err)
	}
	if err := st.EndRun(early, base.Add(time.Minute), "OK", "12 files"); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"a second end":        st.EndRun(early, base.Add(2*time.Minute), "FAILED", "again"),
		"an unknown run":      st.EndRun(late+other+early, base, "OK", ""),
		"an end of no result": st.EndRun(late, base, "", "no result"),
	} {
		if err == nil {
			t.Errorf("EndRun of %s succeeded", name)
		}
	}
	runs, err := st.TaskRuns("a")
	if err != nil {
		t.Fatal(err)
	}
	want := []TaskRun{
		{ID: early, TaskID: "a", Started: base, Ended: base.Add(time.Minute), Result: "OK", Message: "12 files"},
		{ID: late, TaskID: "a", Started: base.Add(2*time.Hour + 123*time.Millisecond)},
	}
	if !slices.EqualFunc(runs, want, sameRun) {
		t.Fatalf("runs of a\n%+v\nwant\n%+v", runs, want)
	}
	n, err := st.EndOpenRuns(base.Add(3*time.Hour), "INTERRUPTED", "found open")
	if err != nil || n != 2 {
		t.Fatalf("EndOpenRuns closed %d: %v, want 2", n, err)
	}
	if n, err := st.EndOpenRuns(base.Add(4*time.Hour), "INTERRUPTED", "found open"); err != nil || n != 0 {
		t.Fatalf("a second EndOpenRuns closed %d: %v, want 0", n, err)
	}
	runs, err = st.TaskRuns("a")
	if err != nil {
		t.Fatal(err)
	}
	want[1].Ended, want[1].Result, want[1].Message = base.Add(3*time.Hour), "INTERRUPTED", "found open"
	if !slices.EqualFunc(runs, want, sameRun) {
		t.Fatalf("runs of a after EndOpenRuns\n%+v\nwant\n%+v", runs, want)
	}
	if runs, err := st.TaskRuns("c"); err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("runs of a task without runs: %v %v, want an empty list", runs, err)
	}
}

func sameRun(a, b TaskRun) bool {
	return a.ID == b.ID && a.TaskID == b.TaskID && a.Started.Equal(b.Started) && a.Ended.Equal(b.Ended) &&
		a.Result == b.Result && a.Message == b.Message && a.Started.Location() == time.UTC &&
		(a.Ended.IsZero() || a.Ended.Location() == time.UTC)
}

// endSignal is the store with a signal after every EndRun, so the test can
// wait for the end of a run.
type endSignal struct {
	*Store
	ended chan int64
}

func (s endSignal) EndRun(runID int64, ended time.Time, result, message string) error {
	err := s.Store.EndRun(runID, ended, result, message)
	s.ended <- runID
	return err
}

// The scheduler at the fake clock records each run in task_runs with its
// start, end and result: a run found open at the start as INTERRUPTED, then
// OK, FAILED and CANCELED.
func TestSchedulerRecordsRunsInTaskRuns(t *testing.T) {
	st := runsStore(t)
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if _, err := st.BeginRun("compact", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	clock := tasks.NewFakeClock(t0)
	store := endSignal{Store: st, ended: make(chan int64, 10)}
	s := tasks.New(clock, store)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	for _, task := range []tasks.Task{
		{ID: "compact", Schedule: tasks.Schedule{Type: tasks.TypeHourly, StartAt: t0.Add(30 * time.Minute)}, Enabled: true,
			Run: func(context.Context) (string, error) { return "freed 3 blobs", nil }},
		{ID: "rescan", Schedule: tasks.Schedule{Type: tasks.TypeManual}, Enabled: true,
			Run: func(context.Context) (string, error) { return "", errors.New("the store is unavailable") }},
		{ID: "rebuild", Schedule: tasks.Schedule{Type: tasks.TypeManual}, Enabled: true,
			Run: func(ctx context.Context) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}},
	} {
		if err := s.Add(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	wait := func() {
		t.Helper()
		select {
		case <-store.ended:
		case <-time.After(5 * time.Second):
			t.Fatal("no run ended within 5 s")
		}
	}
	clock.Advance(30 * time.Minute)
	wait()
	if err := s.Run("rescan"); err != nil {
		t.Fatal(err)
	}
	wait()
	clock.Advance(time.Minute)
	if err := s.Run("rebuild"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if err := s.Stop("rebuild"); err != nil {
		t.Fatal(err)
	}
	wait()
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	for id, want := range map[string][]TaskRun{
		"compact": {
			{TaskID: "compact", Started: at(-time.Hour), Ended: t0, Result: "INTERRUPTED"},
			{TaskID: "compact", Started: at(30 * time.Minute), Ended: at(30 * time.Minute), Result: "OK", Message: "freed 3 blobs"},
		},
		"rescan": {{TaskID: "rescan", Started: at(30 * time.Minute), Ended: at(30 * time.Minute), Result: "FAILED",
			Message: "the store is unavailable"}},
		"rebuild": {{TaskID: "rebuild", Started: at(31 * time.Minute), Ended: at(32 * time.Minute), Result: "CANCELED",
			Message: "context canceled"}},
	} {
		runs, err := st.TaskRuns(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != len(want) {
			t.Fatalf("runs of %s: %+v, want %d", id, runs, len(want))
		}
		for i, r := range runs {
			w := want[i]
			w.ID = r.ID
			if w.Message == "" && w.Result == "INTERRUPTED" {
				w.Message = r.Message
			}
			if !sameRun(r, w) || r.Ended.IsZero() || r.Result == "" || r.Message == "" {
				t.Fatalf("run %d of %s: %+v, want %+v with start, end and result", i, id, r, w)
			}
		}
	}
}

// LastRuns gives the run of each task with the latest start, the one with
// the highest id among runs of one start, a run without an end among them;
// a task without runs is not in the map.
func TestLastRuns(t *testing.T) {
	st := runsStore(t)
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	empty, err := st.LastRuns()
	if err != nil || len(empty) != 0 {
		t.Fatalf("%v %v", empty, err)
	}
	first, _ := st.BeginRun("demo-a", at)
	if err := st.EndRun(first, at.Add(time.Second), tasks.ResultFailed, "first"); err != nil {
		t.Fatal(err)
	}
	second, _ := st.BeginRun("demo-a", at.Add(time.Minute))
	if err := st.EndRun(second, at.Add(2*time.Minute), tasks.ResultOK, "second"); err != nil {
		t.Fatal(err)
	}
	same, _ := st.BeginRun("demo-a", at.Add(time.Minute))
	open, _ := st.BeginRun("demo-b", at.Add(time.Hour))
	last, err := st.LastRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 2 || last["demo-a"].ID != same || last["demo-a"].Result != "" || !last["demo-a"].Ended.IsZero() ||
		last["demo-b"].ID != open || !last["demo-b"].Started.Equal(at.Add(time.Hour)) {
		t.Fatalf("%+v", last)
	}
	if err := st.EndRun(same, at.Add(3*time.Minute), tasks.ResultCanceled, "stopped"); err != nil {
		t.Fatal(err)
	}
	if last, _ = st.LastRuns(); last["demo-a"].Result != tasks.ResultCanceled || last["demo-a"].Message != "stopped" {
		t.Fatalf("%+v", last["demo-a"])
	}
}

// PropertySecretFiles lists the secret files of the properties of one task,
// and of no other task or capability.
func TestPropertySecretFiles(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{PropertySecretPath(dir, SecretKindTask, "demo", "remotePassword"), PropertySecretPath(dir, SecretKindTask, "demo", "token"),
		PropertySecretPath(dir, SecretKindTask, "demo-other", "token"), PropertySecretPath(dir, SecretKindCapability, "demo", "token")} {
		if err := WriteSecret(p, []byte("example-secret")); err != nil {
			t.Fatal(err)
		}
	}
	files, err := PropertySecretFiles(dir, SecretKindTask, "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{PropertySecretPath(dir, SecretKindTask, "demo", "remotePassword"), PropertySecretPath(dir, SecretKindTask, "demo", "token")}
	if !slices.Equal(files, want) {
		t.Fatalf("%v, want %v", files, want)
	}
	if none, err := PropertySecretFiles(t.TempDir(), SecretKindTask, "demo"); err != nil || len(none) != 0 {
		t.Fatalf("without a folder: %v %v", none, err)
	}
}

// Ruling SB12: the end of a run removes the runs of its task beyond the
// last KeptRuns, the oldest first, and leaves the runs of other tasks; the
// end of the open runs at a start does the same for every task.
func TestRunsKeepTheLastRunsOfEachTask(t *testing.T) {
	st := runsStore(t)
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	other, _ := st.BeginRun("demo-b", at)
	if err := st.EndRun(other, at, tasks.ResultOK, ""); err != nil {
		t.Fatal(err)
	}
	var first int64
	for i := 0; i <= KeptRuns; i++ {
		id, err := st.BeginRun("demo-a", at.Add(time.Duration(i)*time.Second))
		if err == nil {
			err = st.EndRun(id, at.Add(time.Duration(i)*time.Second), tasks.ResultOK, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id
		}
	}
	runs, err := st.TaskRuns("demo-a")
	if err != nil || len(runs) != KeptRuns || runs[0].ID == first || !runs[0].Started.Equal(at.Add(time.Second)) {
		t.Fatalf("%d runs of demo-a, first %+v, %v", len(runs), runs[0], err)
	}
	if b, _ := st.TaskRuns("demo-b"); len(b) != 1 {
		t.Fatalf("%d runs of demo-b", len(b))
	}
	// one open run more, ended by a start
	if _, err := st.BeginRun("demo-a", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := st.EndOpenRuns(at.Add(2*time.Hour), tasks.ResultInterrupted, "start"); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if runs, _ = st.TaskRuns("demo-a"); len(runs) != KeptRuns || runs[len(runs)-1].Result != tasks.ResultInterrupted {
		t.Fatalf("%d runs after the start", len(runs))
	}
}

// LastRun reads the last run of one task through the index on task and
// start, and finds none for a task without runs.
func TestLastRunReadsOneTask(t *testing.T) {
	st := runsStore(t)
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	a1, _ := st.BeginRun("demo-a", at)
	a2, _ := st.BeginRun("demo-a", at.Add(time.Minute))
	_, _ = st.BeginRun("demo-b", at.Add(time.Hour))
	_ = a1
	last, ok, err := st.LastRun("demo-a")
	if err != nil || !ok || last.ID != a2 {
		t.Fatalf("%+v %v %v", last, ok, err)
	}
	if _, ok, err := st.LastRun("demo-none"); ok || err != nil {
		t.Fatalf("a task without runs: %v %v", ok, err)
	}
	rows, err := st.readers.QueryContext(bg, "EXPLAIN QUERY PLAN "+lastRunQuery, "demo-a")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if text := strings.Join(plan, "; "); !strings.Contains(text, "SEARCH task_runs USING INDEX task_runs_task_started (task_id=?)") {
		t.Fatalf("plan %s", text)
	}
}
