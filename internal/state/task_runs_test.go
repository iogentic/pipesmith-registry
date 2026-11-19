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

// withRuns is the list steps with schema4 as its next version; applied
// counts the calls of that step.
func withRuns(applied *int) []migrate.Step {
	return append(slices.Clip(steps), migrate.Step{Version: len(steps) + 1, Apply: func(tx *sql.Tx) error {
		*applied++
		return schema4(tx)
	}})
}

// runsStore is a new state file that has every step and schema4.
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
func TestSchema4OnTheReleasedFile(t *testing.T) {
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

// The step is not in the list of this binary yet, so a file it creates has
// no task_runs; schema4 joins the list after the steps 2 and 3.
func TestSchema4IsNotListedYet(t *testing.T) {
	st, err := Create(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var tables, runs int
	if err := st.readers.QueryRow("SELECT count(*), count(*) FILTER (WHERE name = 'task_runs') FROM sqlite_master"+
		" WHERE type = 'table'").Scan(&tables, &runs); err != nil {
		t.Fatal(err)
	}
	if tables == 0 || runs != 0 {
		t.Fatalf("%d tables, %d of them task_runs; want the tables of the list and no task_runs", tables, runs)
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
