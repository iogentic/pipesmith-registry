package migrate_test

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
	_ "modernc.org/sqlite"
)

const kind = "test file"

// open opens path as the state package opens its writer: one connection,
// BEGIN IMMEDIATE and a busy timeout. A missing file is created empty first.
func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	params := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(30000)"}, "_txlock": {"immediate"}}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()+"?"+params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ledger counts how often each test step ran.
type ledger struct{ runs [4]atomic.Int32 }

// steps are the first n of three test steps. Step v creates table t<v>; step
// 3 also changes the row of t1, so a copy taken before it keeps the old row.
func (l *ledger) steps(n int) []migrate.Step {
	all := []migrate.Step{
		{Version: 1, Apply: l.run(1, "CREATE TABLE t1 (v TEXT NOT NULL)", "INSERT INTO t1 VALUES ('one')")},
		{Version: 2, Apply: l.run(2, "CREATE TABLE t2 (v TEXT NOT NULL)", "INSERT INTO t2 VALUES ('two')")},
		{Version: 3, Apply: l.run(3, "CREATE TABLE t3 (v TEXT NOT NULL)", "UPDATE t1 SET v = 'changed by step 3'")},
	}
	return all[:n]
}

func (l *ledger) run(version int, statements ...string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		l.runs[version].Add(1)
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	}
}

func (l *ledger) ran(t *testing.T, want ...int32) {
	t.Helper()
	for v, n := range want {
		if got := l.runs[v+1].Load(); got != n {
			t.Errorf("step %d ran %d times, want %d", v+1, got, n)
		}
	}
}

func query(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	var text sql.NullString
	if err := db.QueryRow(q).Scan(&text); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return text.String
}

func schemaRow(t *testing.T, db *sql.DB) string {
	t.Helper()
	return query(t, db, "SELECT value FROM meta WHERE key = 'schema'")
}

func tables(t *testing.T, db *sql.DB) string {
	t.Helper()
	return query(t, db, "SELECT group_concat(name, ' ') FROM (SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name)")
}

// at makes a file of the given version with the test steps.
func at(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	db := open(t, path)
	var l ledger
	if _, err := migrate.Run(db, path, l.steps(version), migrate.Options{Kind: kind, Create: true}); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func noCopies(t *testing.T, path string) {
	t.Helper()
	found, err := filepath.Glob(path + ".v*")
	if err != nil || len(found) != 0 {
		t.Fatalf("copies %v, error %v; want none", found, err)
	}
}

// A file without any table takes every step and goes into WAL mode; there
// is nothing to copy.
func TestNewFileTakesEveryStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sqlite")
	db := open(t, path)
	var l ledger
	got, err := migrate.Run(db, path, l.steps(3), migrate.Options{Kind: kind, Create: true, Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != (migrate.Result{From: 0, To: 3}) {
		t.Fatalf("result %+v, want from 0 to 3 without a copy", got)
	}
	if v, names := schemaRow(t, db), tables(t, db); v != "3" || names != "meta t1 t2 t3" {
		t.Fatalf("schema row %s, tables %q", v, names)
	}
	if mode := query(t, db, "PRAGMA journal_mode"); mode != "wal" {
		t.Fatalf("journal mode %s, want wal", mode)
	}
	l.ran(t, 1, 1, 1)
	noCopies(t, path)
}

// Without Create a file without any table is refused, as Open of release
// 0.1.1 refused it, and nothing is written.
func TestNewFileNeedsCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sqlite")
	db := open(t, path)
	var l ledger
	_, err := migrate.Run(db, path, l.steps(2), migrate.Options{Kind: kind})
	if err == nil || err.Error() != path+" has no meta table, so it is no test file" {
		t.Fatalf("error %v", err)
	}
	if names := tables(t, db); names != "" {
		t.Fatalf("tables %q after the refusal", names)
	}
	l.ran(t, 0, 0)
}

func TestFileWithoutMetaIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sqlite")
	db := open(t, path)
	if _, err := db.Exec("CREATE TABLE other (x)"); err != nil {
		t.Fatal(err)
	}
	var l ledger
	_, err := migrate.Run(db, path, l.steps(1), migrate.Options{Kind: kind, Create: true})
	if err == nil || err.Error() != path+" has no meta table, so it is no test file" {
		t.Fatalf("error %v", err)
	}
	if names := tables(t, db); names != "other" {
		t.Fatalf("tables %q", names)
	}
}

// An older file is copied to <file>.v<old> first, then takes the steps it
// lacks. The copy holds the file as it was, with mode 0600.
func TestOlderFileIsCopiedThenMigrated(t *testing.T) {
	path, db := at(t, 1)
	var l ledger
	got, err := migrate.Run(db, path, l.steps(3), migrate.Options{Kind: kind, Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := (migrate.Result{From: 1, To: 3, Copy: path + ".v1"}); got != want {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	if v, names, row := schemaRow(t, db), tables(t, db), query(t, db, "SELECT v FROM t1"); v != "3" ||
		names != "meta t1 t2 t3" || row != "changed by step 3" {
		t.Fatalf("migrated file: schema row %s, tables %q, row of t1 %q", v, names, row)
	}
	l.ran(t, 0, 1, 1)
	info, err := os.Stat(got.Copy)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("copy mode %o, want 600", perm)
	}
	old := open(t, got.Copy)
	if v, names, row := schemaRow(t, old), tables(t, old), query(t, old, "SELECT v FROM t1"); v != "1" ||
		names != "meta t1" || row != "one" {
		t.Fatalf("copy: schema row %s, tables %q, row of t1 %q", v, names, row)
	}
}

func TestNoCopyWithoutBackup(t *testing.T) {
	path, db := at(t, 1)
	var l ledger
	got, err := migrate.Run(db, path, l.steps(3), migrate.Options{Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	if got != (migrate.Result{From: 1, To: 3}) || schemaRow(t, db) != "3" {
		t.Fatalf("result %+v, schema row %s", got, schemaRow(t, db))
	}
	noCopies(t, path)
}

// A file at the last version takes no step and gets no copy.
func TestCurrentFileIsLeftAlone(t *testing.T) {
	path, db := at(t, 3)
	var l ledger
	got, err := migrate.Run(db, path, l.steps(3), migrate.Options{Kind: kind, Create: true, Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != (migrate.Result{From: 3, To: 3}) {
		t.Fatalf("result %+v", got)
	}
	l.ran(t, 0, 0, 0)
	noCopies(t, path)
}

// A file of a newer version is refused with both versions named, and
// neither the file nor a copy is written.
func TestNewerFileIsRefused(t *testing.T) {
	path, db := at(t, 3)
	var l ledger
	got, err := migrate.Run(db, path, l.steps(2), migrate.Options{Kind: kind, Create: true, Backup: true})
	var newer *migrate.NewerError
	if !errors.As(err, &newer) || newer.Found != 3 || newer.Knows != 2 {
		t.Fatalf("error %v, want a NewerError of 3 and 2", err)
	}
	if err.Error() != path+" has schema 3, this binary knows schema 2" {
		t.Fatalf("refusal %q", err)
	}
	if got.From != 3 || got.To != 3 || got.Copy != "" || schemaRow(t, db) != "3" {
		t.Fatalf("result %+v, schema row %s", got, schemaRow(t, db))
	}
	l.ran(t, 0, 0)
	noCopies(t, path)
}

// A step runs in one transaction: when it fails, the file keeps its version
// and its tables, and the copy of this run is removed again.
func TestFailedStepChangesNothing(t *testing.T) {
	path, db := at(t, 1)
	var l ledger
	steps := l.steps(2)
	steps[1].Apply = func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE t2 (v TEXT)"); err != nil {
			return err
		}
		return errors.New("step two broke")
	}
	got, err := migrate.Run(db, path, steps, migrate.Options{Kind: kind, Backup: true})
	if err == nil || !strings.Contains(err.Error(), "schema step 2") || !strings.Contains(err.Error(), "step two broke") {
		t.Fatalf("error %v", err)
	}
	if v, names := schemaRow(t, db), tables(t, db); v != "1" || names != "meta t1" {
		t.Fatalf("schema row %s, tables %q after a failed step", v, names)
	}
	if got != (migrate.Result{From: 1, To: 1}) {
		t.Fatalf("result %+v", got)
	}
	noCopies(t, path)
}

// When a later step fails, the steps before it stay, and so does the copy:
// it is the way back to the version the file had.
func TestLaterFailedStepKeepsTheCopy(t *testing.T) {
	path, db := at(t, 1)
	var l ledger
	steps := l.steps(3)
	steps[2].Apply = func(*sql.Tx) error { return errors.New("step three broke") }
	got, err := migrate.Run(db, path, steps, migrate.Options{Kind: kind, Backup: true})
	if err == nil || !strings.Contains(err.Error(), "schema step 3") {
		t.Fatalf("error %v", err)
	}
	if want := (migrate.Result{From: 1, To: 2, Copy: path + ".v1"}); got != want {
		t.Fatalf("result %+v, want %+v", got, want)
	}
	if v, names := schemaRow(t, db), tables(t, db); v != "2" || names != "meta t1 t2" {
		t.Fatalf("schema row %s, tables %q", v, names)
	}
	if v := schemaRow(t, open(t, got.Copy)); v != "1" {
		t.Fatalf("copy has schema row %s", v)
	}
}

// An existing copy is never replaced: the run stops before the first step.
func TestExistingCopyIsNeverReplaced(t *testing.T) {
	path, db := at(t, 1)
	if err := os.WriteFile(path+".v1", []byte("an earlier copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	var l ledger
	_, err := migrate.Run(db, path, l.steps(2), migrate.Options{Kind: kind, Backup: true})
	if err == nil || !strings.Contains(err.Error(), path+".v1") {
		t.Fatalf("error %v, want a refusal that names the copy", err)
	}
	l.ran(t, 0, 0)
	if v := schemaRow(t, db); v != "1" {
		t.Fatalf("schema row %s", v)
	}
	if data, err := os.ReadFile(path + ".v1"); err != nil || string(data) != "an earlier copy" {
		t.Fatalf("the earlier copy now holds %q, %v", data, err)
	}
}

func TestStepsAreNumberedFromOne(t *testing.T) {
	apply := func(*sql.Tx) error { return nil }
	for name, steps := range map[string][]migrate.Step{
		"none":      nil,
		"gap":       {{Version: 1, Apply: apply}, {Version: 3, Apply: apply}},
		"from two":  {{Version: 2, Apply: apply}},
		"no change": {{Version: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.sqlite")
			db := open(t, path)
			if _, err := migrate.Run(db, path, steps, migrate.Options{Kind: kind, Create: true}); err == nil {
				t.Fatal("a broken step list was run")
			}
			if names := tables(t, db); names != "" {
				t.Fatalf("tables %q", names)
			}
		})
	}
}

func TestBrokenFilesAreRefused(t *testing.T) {
	var l ledger
	t.Run("no SQLite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "x.sqlite")
		if err := os.WriteFile(path, []byte(strings.Repeat("no database here ", 64)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := migrate.Run(open(t, path), path, l.steps(1), migrate.Options{Kind: kind, Create: true})
		if err == nil || !strings.HasPrefix(err.Error(), path+" is not a SQLite file") {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("no schema row", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "x.sqlite")
		db := open(t, path)
		if _, err := db.Exec(migrate.Meta); err != nil {
			t.Fatal(err)
		}
		_, err := migrate.Run(db, path, l.steps(1), migrate.Options{Kind: kind, Create: true})
		if err == nil || !strings.HasPrefix(err.Error(), path+" has no schema row") {
			t.Fatalf("error %v", err)
		}
	})
	l.ran(t, 0)
}

// Two connections that open one new file at the same time apply each step
// once, and both end at the last version.
func TestTwoConnectionsApplyEachStepOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sqlite")
	dbs := []*sql.DB{open(t, path), open(t, path)}
	var l ledger
	steps := l.steps(3)
	errs := make([]error, len(dbs))
	var wg sync.WaitGroup
	for i, db := range dbs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = migrate.Run(db, path, steps, migrate.Options{Kind: kind, Create: true})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	l.ran(t, 1, 1, 1)
	if v := schemaRow(t, dbs[0]); v != "3" {
		t.Fatalf("schema row %s", v)
	}
}
