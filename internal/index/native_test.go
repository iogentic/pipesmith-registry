package index_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
)

// entry is the entry helper of tests/test_index.py: repo demo-releases,
// size 1, no SHA1, no content type, created_ms 1, location loc/<repo>/<path>.
func entry(path string) stores.Entry {
	return stores.Entry{Repo: "demo-releases", Path: path, Size: 1, CreatedMS: 1, Location: "loc/demo-releases/" + path}
}

func slice(entries ...stores.Entry) func(yield func(stores.Entry) error) error {
	return func(yield func(stores.Entry) error) error {
		for _, e := range entries {
			if err := yield(e); err != nil {
				return err
			}
		}
		return nil
	}
}

func openIndex(t *testing.T) (*index.Index, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix, path
}

func mustReplace(t *testing.T, ix *index.Index, store string, entries func(yield func(stores.Entry) error) error) int64 {
	t.Helper()
	written, err := ix.ReplaceStore(store, entries)
	if err != nil {
		t.Fatal(err)
	}
	return written
}

func mustLookup(t *testing.T, ix *index.Index, repo, path string) *index.Asset {
	t.Helper()
	a, err := ix.Lookup(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustGeneration(t *testing.T, ix *index.Index) int64 {
	t.Helper()
	g, err := ix.Generation()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestReplaceAcceptsAFunc(t *testing.T) {
	ix, _ := openIndex(t)
	rows := func(yield func(stores.Entry) error) error {
		for i := range 100 {
			if err := yield(entry(fmt.Sprintf("f%03d.txt", i))); err != nil {
				return err
			}
		}
		return nil
	}
	if got := mustReplace(t, ix, "a", rows); got != 100 {
		t.Fatalf("written %d, want 100", got)
	}
	if mustLookup(t, ix, "demo-releases", "f099.txt") == nil {
		t.Fatal("f099.txt missing")
	}
}

func TestReplaceFailureKeepsOldRows(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "a", slice(entry("old.txt")))
	broken := errors.New("the disk went away")
	_, err := ix.ReplaceStore("a", func(yield func(stores.Entry) error) error {
		if err := yield(entry("new.txt")); err != nil {
			return err
		}
		return broken
	})
	if !errors.Is(err, broken) {
		t.Fatalf("error %v, want the error of the entries", err)
	}
	if mustLookup(t, ix, "demo-releases", "old.txt") == nil {
		t.Fatal("old.txt gone after a failed replace")
	}
	if mustLookup(t, ix, "demo-releases", "new.txt") != nil {
		t.Fatal("new.txt written by a failed replace")
	}
	if got := mustReplace(t, ix, "a", slice(entry("new.txt"))); got != 1 {
		t.Fatalf("written %d, want 1", got)
	}
}

func TestReplacePanicRollsBack(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "a", slice(entry("old.txt")))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no panic")
			}
		}()
		ix.ReplaceStore("a", func(yield func(stores.Entry) error) error {
			yield(entry("new.txt"))
			panic("entries broke")
		})
	}()
	if mustLookup(t, ix, "demo-releases", "old.txt") == nil || mustLookup(t, ix, "demo-releases", "new.txt") != nil {
		t.Fatal("a panicking replace changed the rows")
	}
	if got := mustReplace(t, ix, "a", slice(entry("new.txt"))); got != 1 {
		t.Fatalf("written %d after the panic, want 1", got)
	}
}

func TestReopenKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	first, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := entry("one.txt")
	e.Size = 7
	mustReplace(t, first, "a", slice(e))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := mustLookup(t, second, "demo-releases", "one.txt"); got == nil || got.Size != 7 {
		t.Fatalf("after reopen: %+v", got)
	}
}

func TestOpenMakesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "deeper", "index.sqlite")
	ix, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("no index file at %s: %v", path, err)
	}
	if ix.Path() != path {
		t.Fatalf("Path %q, want %q", ix.Path(), path)
	}
}

func TestOpenRefusesAnotherSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID",
		"INSERT INTO meta VALUES ('schema', 2), ('generation', 7)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	// str(Path(...)) drops the empty and "." parts of the path.
	_, err = index.Open(dir + "//./index.sqlite")
	want := path + " has index schema 2, expected 1; delete it and scan again"
	if err == nil || err.Error() != want {
		t.Fatalf("error %v, want %q", err, want)
	}
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
		t.Fatalf("generation %d after the refused open, want 7", generation)
	}
}

func TestClosed(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "a", slice(entry("one.txt")))
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	checks := map[string]error{}
	_, checks["Lookup"] = ix.Lookup("demo-releases", "one.txt")
	_, checks["Children"] = ix.Children("demo-releases", "", 10, 0)
	_, checks["IsDir"] = ix.IsDir("demo-releases", "")
	_, checks["RepoStats"] = ix.RepoStats()
	_, checks["StoreStats"] = ix.StoreStats()
	_, checks["RepoNames"] = ix.RepoNames()
	_, checks["Hints"] = ix.Hints()
	_, checks["Generation"] = ix.Generation()
	_, checks["ReplaceStore"] = ix.ReplaceStore("a", slice())
	checks["RemoveStore"] = ix.RemoveStore("a")
	checks["SetHints"] = ix.SetHints("a", map[string]string{"demo-releases": "raw"})
	checks["Drop"] = ix.Drop("demo-releases", "one.txt", "a")
	for name, err := range checks {
		if !errors.Is(err, index.ErrClosed) {
			t.Errorf("%s after close: %v, want ErrClosed", name, err)
		}
	}
	if index.ErrClosed.Error() != "the index is closed" {
		t.Errorf("ErrClosed says %q", index.ErrClosed)
	}
}

func TestLargeDirectory(t *testing.T) {
	ix, _ := openIndex(t)
	rows := func(yield func(stores.Entry) error) error {
		for i := range 50_000 {
			e := entry(fmt.Sprintf("big/f%05d.txt", i))
			e.Size, e.CreatedMS = int64(i), int64(i)
			if err := yield(e); err != nil {
				return err
			}
		}
		return nil
	}
	mustReplace(t, ix, "a", rows)
	started := time.Now()
	page, err := ix.Children("demo-releases", "big", 100, 0)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 100 || page[0].Name != "f00000.txt" || page[99].Name != "f00099.txt" {
		t.Fatalf("first page: %d rows, %+v ... %+v", len(page), page[0], page[len(page)-1])
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("first page took %s", elapsed)
	}
}

func TestDefaultLimit(t *testing.T) {
	if index.DefaultLimit != 10_000 {
		t.Fatalf("DefaultLimit %d, the default of children in Python is 10000", index.DefaultLimit)
	}
	if index.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion %d", index.SchemaVersion)
	}
}
