package release

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// The two schema levels of these tests: the data files are at schemaN, the
// release 0.2.0 writes schemaN+1. The service manager plays both releases:
// when it starts 0.2.0 it migrates both files with the step list of N+1 and
// the copy options the openers of state.sqlite and catalog.sqlite use, then
// keeps them open as a server does; when it starts 0.1.0 it opens them with
// the step list of N.
const schemaN = 2

type schemaService struct {
	*fakeService
	data       string
	writes     int      // the schema 0.2.0 writes
	failStart  bool     // the start of 0.2.0 fails after the migration
	removes    []string // what the start of 0.2.0 removes after it migrated catalog.sqlite: "file", "copy"
	mu         sync.Mutex
	held       []*sql.DB
	oldStarts  int
	junkSeen   []string // -wal and -shm files of no use the old release met
	oldRefusal []error
}

var openerOptions = map[string]migrate.Options{
	StateFile:   {Kind: "state file", Backup: true},
	CatalogFile: {Kind: "catalog file", Create: true, CopyWhen: migrate.SpareCopy(migrate.FreeSpace)},
}

func (s *schemaService) Restart(ctx context.Context, unit string) error {
	if err := s.fakeService.Restart(ctx, unit); err != nil {
		return err
	}
	s.fakeService.mu.Lock()
	running := s.fakeService.running
	s.fakeService.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range []string{StateFile, CatalogFile} {
		path := filepath.Join(s.data, name)
		db := openFile(path)
		switch running {
		case "0.2.0":
			res, err := migrate.Run(db, path, testSteps(s.writes), openerOptions[name])
			if err != nil {
				return err
			}
			if res.To > res.From {
				if _, err := db.Exec("INSERT INTO t1 (v) VALUES ('written by 0.2.0')"); err != nil {
					return err
				}
			}
			if name == CatalogFile && len(s.removes) > 0 {
				db.Close()
				if err := s.remove(path, res.From); err != nil {
					return err
				}
				continue
			}
			s.held = append(s.held, db)
		case "0.1.0":
			s.oldStarts++
			for _, side := range []string{"-wal", "-shm"} {
				if data, err := os.ReadFile(path + side); err == nil && string(data) == "junk" {
					s.junkSeen = append(s.junkSeen, name+side)
				}
			}
			if _, err := migrate.Run(db, path, testSteps(schemaN), openerOptions[name]); err != nil {
				s.oldRefusal = append(s.oldRefusal, err)
			}
			db.Close()
		}
	}
	if running == "0.2.0" && s.failStart {
		return errors.New("the unit failed after its start")
	}
	return nil
}

// remove deletes what removes names: the file with its -wal and -shm files,
// and the copy of version from.
func (s *schemaService) remove(path string, from int) error {
	for _, what := range s.removes {
		names := []string{path, path + "-wal", path + "-shm"}
		if what == "copy" {
			names = []string{fmt.Sprintf("%s.v%d", path, from)}
		}
		for _, name := range names {
			if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// Stop closes the files and leaves a -wal and a -shm file of no use behind,
// as a server that was killed can.
func (s *schemaService) Stop(ctx context.Context, unit string) error {
	s.mu.Lock()
	for _, db := range s.held {
		db.Close()
	}
	s.held = nil
	s.mu.Unlock()
	for _, name := range []string{StateFile, CatalogFile} {
		for _, side := range []string{"-wal", "-shm"} {
			if err := os.WriteFile(filepath.Join(s.data, name+side), []byte("junk"), 0o600); err != nil {
				return err
			}
		}
	}
	return s.fakeService.Stop(ctx, unit)
}

// openFile opens path for writing; catalog.sqlite is created when it is
// missing, as the catalog opener does.
func openFile(path string) *sql.DB {
	params := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(10000)"}, "_txlock": {"immediate"}}
	if filepath.Base(path) == CatalogFile {
		params.Set("mode", "rwc")
	}
	db, _ := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()+"?"+params.Encode())
	db.SetMaxOpenConns(1)
	return db
}

// dump lists every row of every table of path, tables by name, rows in the
// order of their columns, read only.
func dump(t *testing.T, path string) string {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dump: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var tables []string
	for names.Next() {
		var n string
		if err := names.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	names.Close()
	var lines []string
	for _, table := range tables {
		rows, err := db.Query(fmt.Sprintf(`SELECT * FROM %q ORDER BY 1, 2`, table))
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			line := table + ":"
			for _, v := range values {
				line += fmt.Sprintf(" %v", v)
			}
			lines = append(lines, line)
		}
		rows.Close()
	}
	return strings.Join(lines, "\n")
}

type schemaRig struct {
	*rig
	svc    *schemaService
	before map[string]string
	dir    string
}

func newSchemaRig(t *testing.T, writes int) *schemaRig {
	t.Helper()
	r := newRig(t, "0.1.0")
	r.dataFiles(schemaN, schemaN)
	sr := &schemaRig{rig: r, svc: &schemaService{fakeService: r.svc, data: r.data, writes: writes}, before: map[string]string{}}
	for _, name := range []string{StateFile, CatalogFile} {
		sr.before[name] = dump(t, filepath.Join(r.data, name))
	}
	r.u.Service = sr.svc
	t.Cleanup(func() {
		sr.svc.mu.Lock()
		defer sr.svc.mu.Unlock()
		for _, db := range sr.svc.held {
			db.Close()
		}
	})
	r.u.HealthTimeout = 300 * time.Millisecond
	r.svc.unhealthy["0.2.0"] = true
	sr.dir = r.release(answering("0.2.0", fmt.Sprintf(`{"catalog":%d,"state":%d}`, writes, writes)))
	return sr
}

func (sr *schemaRig) path(name string) string { return filepath.Join(sr.data, name) }

func TestSchemaWayBack(t *testing.T) {
	for name, failStart := range map[string]bool{"health": false, "start": true} {
		t.Run(name, func(t *testing.T) {
			sr := newSchemaRig(t, schemaN+1)
			sr.svc.failStart = failStart
			before := tree(t, sr.l.Root)
			err := sr.u.Update(sr.dir, false)
			want := ErrHealth
			if failStart {
				want = ErrRestart
			}
			sr.wayBack(err, want, before, "")
			for _, words := range []string{"which answers again", "restored state.sqlite, catalog.sqlite from the copies",
				"changes of state made after the restart are lost", "kept as state.sqlite.failed, catalog.sqlite.failed"} {
				if !strings.Contains(err.Error(), words) {
					t.Fatalf("message %q lacks %q", err, words)
				}
			}
			for _, file := range []string{StateFile, CatalogFile} {
				if got := dump(t, sr.path(file)); got != sr.before[file] {
					t.Fatalf("%s after the way back\n%s\nbefore\n%s", file, got, sr.before[file])
				}
				if _, err := os.Stat(sr.path(file) + fmt.Sprintf(".v%d", schemaN)); err == nil {
					t.Fatalf("the copy of %s is still there", file)
				}
				failed := dump(t, sr.path(file)+".failed")
				if !strings.Contains(failed, fmt.Sprintf("meta: schema %d", schemaN+1)) || !strings.Contains(failed, "written by 0.2.0") {
					t.Fatalf("%s.failed:\n%s", file, failed)
				}
				if data, err := os.ReadFile(sr.path(file) + "-wal"); err == nil && string(data) == "junk" {
					t.Fatalf("the -wal file of the migrated %s is left", file)
				}
			}
			if len(sr.svc.junkSeen) != 0 {
				t.Fatalf("the old release met %v of the migrated files", sr.svc.junkSeen)
			}
			if sr.svc.oldStarts != 2 || len(sr.svc.oldRefusal) != 0 || len(sr.svc.stops) != 1 {
				t.Fatalf("old starts %d, refusals %v, stops %v", sr.svc.oldStarts/2, sr.svc.oldRefusal, sr.svc.stops)
			}
			out := sr.out.String()
			for _, line := range []string{"stopped: " + DefaultUnit + "\n", "restored: state.sqlite from state.sqlite.v2; the migrated file is kept as state.sqlite.failed\n",
				"restored: catalog.sqlite from catalog.sqlite.v2"} {
				if !strings.Contains(out, line) {
					t.Fatalf("output lacks %q:\n%s", line, out)
				}
			}
		})
	}
}

// Without a migration in the plan the way back leaves both files as they
// are, byte for byte, and does not stop the unit.
func TestWayBackWithoutMigration(t *testing.T) {
	sr := newSchemaRig(t, schemaN)
	sums := func() string {
		var out []string
		for _, name := range []string{StateFile, CatalogFile} {
			data, err := os.ReadFile(sr.path(name))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, hexSum(data))
		}
		return strings.Join(out, " ")
	}
	before := sums()
	err := sr.u.Update(sr.dir, false)
	if !errors.Is(err, ErrHealth) || !strings.Contains(err.Error(), "which answers again") {
		t.Fatalf("error %v", err)
	}
	if after := sums(); after != before {
		t.Fatalf("the data files changed: %s, before %s", after, before)
	}
	for _, name := range []string{StateFile, CatalogFile} {
		if _, err := os.Stat(sr.path(name) + fmt.Sprintf(".v%d", schemaN)); err == nil {
			t.Fatalf("a copy of %s was written", name)
		}
	}
	if len(sr.svc.stops) != 0 || strings.Contains(sr.out.String(), "stopped:") {
		t.Fatalf("stops %v", sr.svc.stops)
	}
}

// A failure between the link of <file>.failed and the rename of the copy
// leaves <file> in place with the migrated content, and the copy and the
// .failed file beside it.
func TestWayBackKeepsTheFileBetweenTheSteps(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	sr.u.hook = func(p string) error {
		if p == "back-copy:"+StateFile {
			return errors.New("crash at " + p)
		}
		return nil
	}
	err := sr.u.Update(sr.dir, false)
	if !errors.Is(err, ErrHealth) || !strings.Contains(err.Error(), "rename it to") {
		t.Fatalf("message %q", err)
	}
	file := sr.path(StateFile)
	migrated := dump(t, file)
	if !strings.Contains(migrated, fmt.Sprintf("meta: schema %d", schemaN+1)) || !strings.Contains(migrated, "written by 0.2.0") {
		t.Fatalf("%s after the crash:\n%s", file, migrated)
	}
	a, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file + ".failed")
	if err != nil || string(a) != string(b) {
		t.Fatalf("%s.failed: %v, equal to the migrated file %v", file, err, string(a) == string(b))
	}
	if got := dump(t, file+fmt.Sprintf(".v%d", schemaN)); got != sr.before[StateFile] {
		t.Fatalf("the copy changed:\n%s", got)
	}
}

// An existing <file>.failed of an earlier failed update is never replaced:
// the next update that would migrate the file is refused before any change.
func TestSecondFailedUpdateKeepsTheFirstFailedFile(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	if err := sr.u.Update(sr.dir, false); !errors.Is(err, ErrHealth) {
		t.Fatalf("first update: %v", err)
	}
	first := map[string][]byte{}
	for _, name := range []string{StateFile, CatalogFile} {
		data, err := os.ReadFile(sr.path(name) + ".failed")
		if err != nil {
			t.Fatal(err)
		}
		first[name] = data
	}
	sr.svc.restarts, sr.svc.stops = nil, nil
	sr.refused(ErrSpace, func() error { return sr.u.Update(sr.dir, false) }, sr.path(StateFile)+".failed", "move it away")
	for name, data := range first {
		if got, err := os.ReadFile(sr.path(name) + ".failed"); err != nil || string(got) != string(data) {
			t.Fatalf("%s.failed changed: %v", name, err)
		}
	}
}

// A crash between two steps of the way back stops it with a message that
// says where it stopped, and every version of each file is still on disk.
func TestWayBackCrashPoints(t *testing.T) {
	var points []string
	points = append(points, "back-stop")
	for _, f := range []string{StateFile, CatalogFile} {
		points = append(points, "back-wal:"+f, "back-failed:"+f, "back-copy:"+f)
	}
	points = append(points, "back-start")
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			sr := newSchemaRig(t, schemaN+1)
			sr.u.hook = func(p string) error {
				if p == point {
					return errors.New("crash at " + p)
				}
				return nil
			}
			err := sr.u.Update(sr.dir, false)
			if !errors.Is(err, ErrHealth) || !strings.Contains(err.Error(), "the way back stopped") || !strings.Contains(err.Error(), "crash at "+point) {
				t.Fatalf("message %q", err)
			}
			if sr.link(CurrentLink) != "releases/0.1.0" {
				t.Fatalf("current %q", sr.link(CurrentLink))
			}
			for _, file := range []string{StateFile, CatalogFile} {
				var old, migrated bool
				for _, p := range []string{sr.path(file), sr.path(file) + ".failed", sr.path(file) + fmt.Sprintf(".v%d", schemaN)} {
					if _, err := os.Stat(p); err != nil {
						continue
					}
					d := dump(t, p)
					old = old || d == sr.before[file]
					migrated = migrated || strings.Contains(d, fmt.Sprintf("meta: schema %d", schemaN+1))
				}
				if !old || !migrated {
					t.Fatalf("%s: the file before the update found %v, the migrated file found %v", file, old, migrated)
				}
			}
			if strings.HasPrefix(point, "back-copy:") && !strings.Contains(err.Error(), "rename it to") {
				t.Fatalf("message %q", err)
			}
			if point == "back-start" && (!strings.Contains(err.Error(), "was not started") || sr.svc.oldStarts != 0) {
				t.Fatalf("message %q, old starts %d", err, sr.svc.oldStarts)
			}
		})
	}
}

// A new release that removed catalog.sqlite and left its copy: the way back
// puts the copy back and says that the new release had removed the file.
// state.sqlite is restored as in every way back.
func TestWayBackRestoresARemovedFileFromItsCopy(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	sr.svc.removes = []string{"file"}
	db := openFile(sr.path(CatalogFile))
	if _, err := db.Exec("INSERT INTO t1 (v) VALUES ('a row no fresh file has')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sr.before[CatalogFile] = dump(t, sr.path(CatalogFile))
	err := sr.u.Update(sr.dir, false)
	if !errors.Is(err, ErrHealth) || sr.link(CurrentLink) != "releases/0.1.0" {
		t.Fatalf("error %v, current %q", err, sr.link(CurrentLink))
	}
	for _, file := range []string{StateFile, CatalogFile} {
		if got := dump(t, sr.path(file)); got != sr.before[file] {
			t.Fatalf("%s after the way back\n%s\nbefore\n%s", file, got, sr.before[file])
		}
		if _, err := os.Stat(sr.path(file) + fmt.Sprintf(".v%d", schemaN)); err == nil {
			t.Fatalf("the copy of %s is still there", file)
		}
	}
	if _, err := os.Stat(sr.path(CatalogFile) + ".failed"); err == nil {
		t.Fatal("a catalog.sqlite.failed was made of a removed file")
	}
	for _, words := range []string{"which answers again", "restored state.sqlite from the copies",
		"restored catalog.sqlite from its copy catalog.sqlite.v2", "the new release had removed catalog.sqlite"} {
		if !strings.Contains(err.Error(), words) {
			t.Fatalf("message %q lacks %q", err, words)
		}
	}
	if strings.Contains(err.Error(), "did not change") {
		t.Fatalf("message %q says the removed file did not change", err)
	}
	out := sr.out.String()
	for _, line := range []string{"restored: state.sqlite from state.sqlite.v2; the migrated file is kept as state.sqlite.failed\n",
		"restored: catalog.sqlite from catalog.sqlite.v2; the new release had removed catalog.sqlite\n"} {
		if !strings.Contains(out, line) {
			t.Fatalf("output lacks %q:\n%s", line, out)
		}
	}
	if len(sr.svc.junkSeen) != 0 || sr.svc.oldStarts != 2 || len(sr.svc.oldRefusal) != 0 {
		t.Fatalf("junk %v, old starts %d, refusals %v", sr.svc.junkSeen, sr.svc.oldStarts/2, sr.svc.oldRefusal)
	}
}

// A new release that removed catalog.sqlite and its copy: the way back goes
// on, starts the old release and says that the file is gone.
func TestWayBackNamesAFileRemovedWithoutCopy(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	sr.svc.removes = []string{"file", "copy"}
	err := sr.u.Update(sr.dir, false)
	if !errors.Is(err, ErrHealth) || sr.link(CurrentLink) != "releases/0.1.0" {
		t.Fatalf("error %v, current %q", err, sr.link(CurrentLink))
	}
	if got := dump(t, sr.path(StateFile)); got != sr.before[StateFile] {
		t.Fatalf("state.sqlite after the way back\n%s\nbefore\n%s", got, sr.before[StateFile])
	}
	for _, words := range []string{"which answers again", "restored state.sqlite from the copies",
		"catalog.sqlite is gone: the new release removed it and left no copy", "0.1.0 starts without it",
		"a scan fills again what a scan can"} {
		if !strings.Contains(err.Error(), words) {
			t.Fatalf("message %q lacks %q", err, words)
		}
	}
	if strings.Contains(err.Error(), "did not change") {
		t.Fatalf("message %q says the removed file did not change", err)
	}
	if !strings.Contains(sr.out.String(), "gone: catalog.sqlite was removed by the new release, which left no copy catalog.sqlite.v2\n") {
		t.Fatalf("output:\n%s", sr.out.String())
	}
	if got, err := migrate.FileVersion(sr.path(CatalogFile)); err != nil || got != schemaN {
		t.Fatalf("catalog.sqlite the old release made is at schema %d (%v), want %d", got, err, schemaN)
	}
	if len(sr.svc.junkSeen) != 0 || sr.svc.oldStarts != 2 || len(sr.svc.oldRefusal) != 0 {
		t.Fatalf("junk %v, old starts %d, refusals %v", sr.svc.junkSeen, sr.svc.oldStarts/2, sr.svc.oldRefusal)
	}
}
