package index_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// A scan writes the rows of a store in one transaction, about 40 minutes for
// a large store on the host. A process that only reads opens the index with
// OpenRead beside it. The writer here is a second connection that holds
// BEGIN IMMEDIATE and has emptied the rows in it, as ReplaceStore does first.

// readLimit is how long OpenRead and each read may take beside the writer.
// The busy timeout of the index is 30 s.
const readLimit = time.Second

// holdWriteLock begins a write transaction on a connection of its own and
// runs the statements in it without a commit. release rolls it back; the
// cleanup of the test calls it as well.
func holdWriteLock(t *testing.T, path string, statements ...string) (release func()) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_, _ = conn.ExecContext(ctx, "ROLLBACK") // fails only when BEGIN failed
			conn.Close()
			db.Close()
		})
	}
	t.Cleanup(release)
	for _, statement := range append([]string{"BEGIN IMMEDIATE"}, statements...) {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return release
}

// emptied are the statements of a scan that has begun: the rows of every
// store, the counts and the hints deleted, the generation counted up.
func emptied(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT id FROM stores ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("DELETE FROM files_%d", id), fmt.Sprintf("DELETE FROM dirs_%d", id))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("the index has no store")
	}
	return append(out, "DELETE FROM repo_counts", "DELETE FROM hints", "UPDATE meta SET value = value + 1 WHERE key = 'generation'")
}

// within runs call and fails the test when it takes longer than readLimit.
// A call that waits for the lock goes on in the background until the
// cleanup of the test releases the lock.
func within[T any](t *testing.T, name string, call func() (T, error)) (T, error) {
	t.Helper()
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call()
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-time.After(readLimit):
		t.Fatalf("%s did not return within %s while another connection holds the write lock", name, readLimit)
	}
	var zero T
	return zero, nil
}

func journalMode(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

// TestOpenReadBesideAWriter: while another connection holds the write lock,
// OpenRead and every read return within a second and answer the rows of the
// last finished write. The open writes nothing, so the generation stays.
func TestOpenReadBesideAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	writer, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustReplace(t, writer, "a", slice(entry("x/one.txt"), entry("x/two.txt")))
	if err := writer.SetHints("a", map[string]string{"demo-releases": "maven2"}); err != nil {
		t.Fatal(err)
	}
	generation := mustGeneration(t, writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, path, emptied(t, path)...)

	ix, err := within(t, "OpenRead", func() (*index.Index, error) { return index.OpenRead(path) })
	if err != nil {
		t.Fatalf("OpenRead beside a writer: %v", err)
	}
	one := int64(1) // size and creation time of each entry
	for _, c := range []struct {
		name string
		call func() (any, error)
		want any
	}{
		{"Lookup", func() (any, error) { return ix.Lookup("demo-releases", "x/one.txt") },
			&index.Asset{Repo: "demo-releases", Path: "x/one.txt", Store: "a", Location: "loc/demo-releases/x/one.txt", Size: 1, CreatedMS: 1}},
		{"Children", func() (any, error) { return ix.Children("demo-releases", "x", 10, 0) },
			[]index.Child{{Name: "one.txt", Size: &one, CreatedMS: &one}, {Name: "two.txt", Size: &one, CreatedMS: &one}}},
		{"IsDir", func() (any, error) { return ix.IsDir("demo-releases", "x") }, true},
		{"RepoStats", func() (any, error) { return ix.RepoStats() }, []index.RepoStat{{Repo: "demo-releases", Files: 2, Bytes: 2, NewestMS: 1}}},
		{"StoreStats", func() (any, error) { return ix.StoreStats() }, []index.StoreStat{{Store: "a", Files: 2, Bytes: 2}}},
		{"RepoNames", func() (any, error) { return ix.RepoNames() }, []string{"demo-releases"}},
		{"Hints", func() (any, error) { return ix.Hints() }, map[string]string{"demo-releases": "maven2"}},
		{"Generation", func() (any, error) { return ix.Generation() }, generation},
	} {
		got, err := within(t, c.name, c.call)
		if err != nil {
			t.Errorf("%s beside a writer: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s beside a writer: %#v, want the rows of the last finished write %#v", c.name, got, c.want)
		}
	}
	if _, err := within(t, "Close", func() (struct{}, error) { return struct{}{}, ix.Close() }); err != nil {
		t.Fatal(err)
	}
	release()
	again, err := index.OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := mustGeneration(t, again); got != generation {
		t.Fatalf("generation %d after two opens of OpenRead, want %d of the last write", got, generation)
	}
}

// TestOpenReadRefusesAnotherSchema: a file of another schema version is
// refused with the text of Open, also beside a writer, and is left as it is.
func TestOpenReadRefusesAnotherSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"PRAGMA journal_mode = WAL",
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID",
		"INSERT INTO meta VALUES ('schema', 2), ('generation', 7)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	want := path + " has index schema 2, expected 1; delete it and scan again"
	// str(Path(...)) drops the empty and "." parts of the path.
	if _, err := index.OpenRead(dir + "//./index.sqlite"); err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
	release := holdWriteLock(t, path, "UPDATE meta SET value = 8 WHERE key = 'generation'")
	if _, err := within(t, "OpenRead of another schema", func() (*index.Index, error) { return index.OpenRead(path) }); err == nil || err.Error() != want {
		t.Fatalf("beside a writer: error %v, want %q", err, want)
	}
	release()
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var generation int64
	if err := db.QueryRow("SELECT value FROM meta WHERE key = 'generation'").Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 7 {
		t.Fatalf("generation %d after the refused opens, want 7", generation)
	}
}

// TestOpenReadSetsUpWhatOpenSetsUp: a missing file, an empty file and a file
// in the rollback journal get the set up of Open: the tables, the meta rows,
// WAL mode, and the generation counted up by that write.
func TestOpenReadSetsUpWhatOpenSetsUp(t *testing.T) {
	for _, c := range []struct {
		name       string
		prepare    func(t *testing.T, path string)
		generation int64
	}{
		{"missing file and parent", func(*testing.T, string) {}, 1},
		{"empty file", func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"rollback journal", func(t *testing.T, path string) {
			ix, err := index.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ix.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("PRAGMA journal_mode = DELETE"); err != nil {
				t.Fatal(err)
			}
		}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data", "index.sqlite")
			c.prepare(t, path)
			ix, err := index.OpenRead(path)
			if err != nil {
				t.Fatal(err)
			}
			defer ix.Close()
			if got := mustGeneration(t, ix); got != c.generation {
				t.Errorf("generation %d, want %d", got, c.generation)
			}
			if stats, err := ix.StoreStats(); err != nil || len(stats) != 0 {
				t.Errorf("StoreStats %v, %v; want none", stats, err)
			}
			if mode := journalMode(t, path); mode != "wal" {
				t.Errorf("journal mode %q, want wal", mode)
			}
		})
	}
}

// TestOpenWaitsForAnotherWriter pins the writer side as it was: Open sets the
// file up in a write, so it waits for the lock of another connection, up to
// the busy timeout of 30 s, and goes on once the lock is free. A write
// through an index of OpenRead waits the same way. Drop gives up after its
// short busy timeout and keeps the row.
func TestOpenWaitsForAnotherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	writer, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustReplace(t, writer, "a", slice(entry("keep.txt")))
	mustReplace(t, writer, "b", slice(entry("gone.txt")))
	generation := mustGeneration(t, writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, path)
	reader, err := within(t, "OpenRead", func() (*index.Index, error) { return index.OpenRead(path) })
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	started := time.Now()
	if err := reader.Drop("demo-releases", "gone.txt", "b"); err != nil {
		t.Fatalf("Drop beside another writer: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("Drop took %s, want about its busy timeout of 250 ms", elapsed)
	}
	if mustLookup(t, reader, "demo-releases", "gone.txt") == nil {
		t.Error("Drop removed the row while another connection held the write lock")
	}

	opened := make(chan error, 1)
	var second *index.Index
	go func() {
		ix, err := index.Open(path)
		second = ix
		opened <- err
	}()
	wrote := make(chan error, 1)
	go func() {
		_, err := reader.ReplaceStore("c", slice(entry("c.txt")))
		wrote <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("Open returned while another connection held the write lock: %v", err)
	case err := <-wrote:
		t.Fatalf("ReplaceStore returned while another connection held the write lock: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	release()
	for _, w := range []struct {
		name string
		done chan error
	}{{"Open", opened}, {"ReplaceStore", wrote}} {
		select {
		case err := <-w.done:
			if err != nil {
				t.Fatalf("%s once the lock was free: %v", w.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s still waits after the lock was released", w.name)
		}
	}
	defer second.Close()
	// The set up of Open counted the generation up, and so did ReplaceStore.
	if got := mustGeneration(t, reader); got != generation+2 {
		t.Errorf("generation %d, want %d", got, generation+2)
	}
	if err := reader.Drop("demo-releases", "gone.txt", "b"); err != nil {
		t.Fatal(err)
	}
	if mustLookup(t, reader, "demo-releases", "gone.txt") != nil {
		t.Error("Drop kept the row once the lock was free")
	}
}

func integrity(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestReadersBesideAHalfWrittenScan: ReplaceStore, stopped half way through
// the entries of its store, holds the write lock as a scan does. A reader of
// OpenRead beside it sees the rows of the last finished write and none of the
// half written ones. Readers open, read, drop and close without a lock the
// scan waits for: the scan goes on, commits at once, and a reader that stayed
// open sees every new row and no old one.
func TestReadersBesideAHalfWrittenScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	writer, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	mustReplace(t, writer, "a", slice(entry("old/one.txt"), entry("old/two.txt")))
	generation := mustGeneration(t, writer)
	const total, pause = 2000, 1000
	half, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := writer.ReplaceStore("a", func(yield func(stores.Entry) error) error {
			for i := range total {
				if i == pause {
					close(half)
					<-resume
				}
				if err := yield(entry(fmt.Sprintf("new/%04d.txt", i))); err != nil {
					return err
				}
			}
			return nil
		})
		done <- err
	}()
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	<-half

	one := int64(1)
	old := []index.Child{{Name: "old", IsDir: true}}
	seesTheLastFinishedWrite := func(name string, ix *index.Index) {
		t.Helper()
		for _, c := range []struct {
			name string
			call func() (any, error)
			want any
		}{
			{"Lookup of an old row", func() (any, error) { return ix.Lookup("demo-releases", "old/one.txt") },
				&index.Asset{Repo: "demo-releases", Path: "old/one.txt", Store: "a", Location: "loc/demo-releases/old/one.txt", Size: 1, CreatedMS: 1}},
			{"Lookup of a half written row", func() (any, error) { return ix.Lookup("demo-releases", "new/0000.txt") }, (*index.Asset)(nil)},
			{"Children of the root", func() (any, error) { return ix.Children("demo-releases", "", 10, 0) }, old},
			{"Children of old", func() (any, error) { return ix.Children("demo-releases", "old", 10, 0) },
				[]index.Child{{Name: "one.txt", Size: &one, CreatedMS: &one}, {Name: "two.txt", Size: &one, CreatedMS: &one}}},
			{"RepoStats", func() (any, error) { return ix.RepoStats() }, []index.RepoStat{{Repo: "demo-releases", Files: 2, Bytes: 2, NewestMS: 1}}},
			{"StoreStats", func() (any, error) { return ix.StoreStats() }, []index.StoreStat{{Store: "a", Files: 2, Bytes: 2}}},
			{"Generation", func() (any, error) { return ix.Generation() }, generation},
		} {
			got, err := within(t, name+": "+c.name, c.call)
			if err != nil {
				t.Errorf("%s: %s beside a half written scan: %v", name, c.name, err)
				continue
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: %s beside a half written scan: %#v, want the rows of the last finished write %#v", name, c.name, got, c.want)
			}
		}
	}

	first, err := within(t, "OpenRead", func() (*index.Index, error) { return index.OpenRead(path) })
	if err != nil {
		t.Fatal(err)
	}
	seesTheLastFinishedWrite("first reader", first)
	if err := first.Drop("demo-releases", "old/one.txt", "a"); err != nil {
		t.Fatalf("Drop beside a half written scan: %v", err)
	}
	if _, err := within(t, "Close", func() (struct{}, error) { return struct{}{}, first.Close() }); err != nil {
		t.Fatal(err)
	}
	second, err := within(t, "OpenRead", func() (*index.Index, error) { return index.OpenRead(path) })
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	seesTheLastFinishedWrite("second reader", second)

	resumed = true
	close(resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the scan beside the readers: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scan did not commit within 10 s after the readers")
	}
	if got := mustGeneration(t, second); got != generation+1 {
		t.Errorf("generation %d after the scan, want %d", got, generation+1)
	}
	if stats, err := second.StoreStats(); err != nil || !reflect.DeepEqual(stats, []index.StoreStat{{Store: "a", Files: total, Bytes: total}}) {
		t.Errorf("StoreStats after the scan %v, %v; want %d rows of the scan", stats, err, total)
	}
	if mustLookup(t, second, "demo-releases", "old/one.txt") != nil {
		t.Error("an old row is left after the scan")
	}
	for _, p := range []string{"new/0000.txt", fmt.Sprintf("new/%04d.txt", pause), fmt.Sprintf("new/%04d.txt", total-1)} {
		if mustLookup(t, second, "demo-releases", p) == nil {
			t.Errorf("%s of the scan is missing", p)
		}
	}
	if got := integrity(t, path); got != "ok" {
		t.Errorf("integrity check %q", got)
	}
}
