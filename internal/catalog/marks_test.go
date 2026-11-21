package catalog

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Ruling SR14 of leg 6-stores-b: the count of the marked blobs of a store
// is a row of store_marks (step 6); SR15: a delta moves an existing row and
// makes none.
func TestStoreMarksRow(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	if _, found, err := c.StoreMarks("demo-a"); err != nil || found {
		t.Fatalf("a row before any count: %v %v", found, err)
	}
	if err := c.AddStoreMarks(map[string]Marks{"demo-a": {Blobs: 2, Bytes: 20}}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := c.StoreMarks("demo-a"); found {
		t.Fatal("a delta made a row")
	}
	at := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	if err := c.SetStoreMarks("demo-a", Marks{Blobs: 5, Bytes: 50, Counted: at}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddStoreMarks(map[string]Marks{"demo-a": {Blobs: 2, Bytes: 20}, "demo-b": {Blobs: 1, Bytes: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddStoreMarks(map[string]Marks{"demo-a": {Blobs: -1, Bytes: -5}}); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.StoreMarks("demo-a")
	if err != nil || !found || got.Blobs != 6 || got.Bytes != 65 || !got.Counted.Equal(at) {
		t.Fatalf("%+v %v %v", got, found, err)
	}
	if _, found, _ := c.StoreMarks("demo-b"); found {
		t.Fatal("a delta made the row of demo-b")
	}
}

// Ruling SR14: a scan that commits writes the row of its store with the
// fill; a fill that is discarded keeps the old row.
func TestAFillWritesTheMarks(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	f, err := c.Begin("demo-a")
	if err != nil {
		t.Fatal(err)
	}
	f.SetMarks(Marks{Blobs: 3, Bytes: 30, Counted: at})
	if err := f.Commit(func(string) string { return "raw" }, []string{"demo-a"}); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := c.StoreMarks("demo-a"); !found || got.Blobs != 3 || got.Bytes != 30 || !got.Counted.Equal(at) {
		t.Fatalf("%+v %v", got, found)
	}
	f, err = c.Begin("demo-a")
	if err != nil {
		t.Fatal(err)
	}
	f.SetMarks(Marks{Blobs: 9, Counted: at})
	if err := f.Discard(); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := c.StoreMarks("demo-a"); got.Blobs != 3 {
		t.Fatalf("a discarded fill changed the row: %+v", got)
	}
}

// Step 6 takes a file of schema 5 forward and keeps its rows.
func TestStepSixAddsStoreMarks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	old, err := openWith(Path(dir), slices.Clip(steps[:5]))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	c := mustOpen(t, dir)
	if got := scalar[int](t, c.writer, "SELECT value FROM meta WHERE key = 'schema'"); got != 6 {
		t.Fatalf("schema %d, want 6", got)
	}
	if err := c.SetStoreMarks("demo-a", Marks{Blobs: 1, Counted: time.Now()}); err != nil {
		t.Fatal(err)
	}
}
