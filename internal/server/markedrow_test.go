package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// countingWalk counts the walks of the marked count and gives the real one.
type countingWalk struct{ calls atomic.Int64 }

func (c *countingWalk) walk(ctx context.Context, name, root string) (markedResult, error) {
	c.calls.Add(1)
	return walkMarked(ctx, name, root)
}

// markedRow reads the row of store_marks of a store from catalog.sqlite.
func (w *writing) markedRow(store string) (catalog.Marks, bool) {
	w.t.Helper()
	cat, err := catalog.Open(w.cfg.DataDir)
	if err != nil {
		w.t.Fatal(err)
	}
	defer cat.Close()
	m, found, err := cat.StoreMarks(store)
	if err != nil {
		w.t.Fatal(err)
	}
	return m, found
}

// dropMarkedRow removes the row of store_marks of a store, as a catalog of
// 0.2.0 has none.
func (w *writing) dropMarkedRow(store string) {
	w.t.Helper()
	abs, err := filepath.Abs(catalog.Path(w.cfg.DataDir))
	if err != nil {
		w.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath())
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM store_marks WHERE store = ?", store); err != nil {
		w.t.Fatal(err)
	}
}

// deletedAnswer is GET /api/blob-stores/{store}/deleted.
func (w *writing) deletedAnswer(store string) map[string]any {
	w.t.Helper()
	r := do(w.t, w.addr(), "GET", "/api/blob-stores/"+store+"/deleted", nil, "basic", asAdmin)
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); r.status != 200 || err != nil {
		w.t.Fatalf("%d %q %v", r.status, r.body, err)
	}
	return m
}

// recount walks the marked blobs of the store at root.
func recount(t *testing.T, root string) (int64, int64) {
	t.Helper()
	r, err := walkMarked(context.Background(), "recount", root)
	if err != nil {
		t.Fatal(err)
	}
	return r.Count, r.Bytes
}

// Rulings SR14 and SR15 of leg 6-stores-b: the scan writes the row; a delete
// mark of a replace and of a delete add one blob and its bytes each, a
// compaction takes them off again, and the row equals a recount after each.
// This server has no undelete.
func TestTheMarkedRowMovesByDeltas(t *testing.T) {
	w := startIngest(t, nil)
	root := createdStoreRepo(t, w, "demo-team", "demo-t", true)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-20*day), "demo-t", "docs/a.txt", "one")
	if _, err := w.srv.Rescan(ctx, "demo-team"); err != nil {
		t.Fatal(err)
	}
	check := func(label string, blobs, bytes int64) {
		t.Helper()
		w.srv.marked.flush()
		row, found := w.markedRow("demo-team")
		count, size := recount(t, root)
		if !found || row.Blobs != blobs || row.Bytes != bytes || count != blobs || size != bytes {
			t.Fatalf("%s: row %+v %v, recount %d %d, want %d %d", label, row, found, count, size, blobs, bytes)
		}
		got := w.deletedAnswer("demo-team")
		if got["blobCount"] != float64(blobs) || got["bytes"] != float64(bytes) || got["counting"] != false || got["countedAt"] == nil {
			t.Fatalf("%s: answer %v", label, got)
		}
	}
	check("after the scan", 0, 0)
	w.put(now.Add(-10*day), "demo-t", "docs/a.txt", "three") // marks "one" as replaced
	w.put(now.Add(-10*day), "demo-t", "docs/b.txt", "seven b")
	if _, err := w.srv.erase(w.repo("demo-t"), "docs/b.txt"); err != nil {
		t.Fatal(err)
	}
	check("after a replace and a delete", 2, int64(len("one")+len("seven b")))
	w.srv.side().setClock(func() time.Time { return now })
	w.at(now)
	if c, err := w.srv.CompactStore(ctx, "demo-team", 7*day); err != nil || c.Removed != 2 {
		t.Fatalf("compaction %+v %v", c, err)
	}
	check("after the compaction", 0, 0)
}

// Ruling SR16: a store without a row is walked once and its count written;
// a second start answers from the row and walks nothing.
func TestASecondStartDoesNotWalk(t *testing.T) {
	gone := testkit.NewBlobSpec("demo-raw", "old/gone.txt")
	gone.Content, gone.Deleted = []byte("old bytes"), true
	w := startIngest(t, nil, gone)
	if _, err := w.srv.Rescan(context.Background(), "local-write"); err != nil { // makes catalog.sqlite
		t.Fatal(err)
	}
	w.dropMarkedRow("store-a") // the scan of the set up counted it: a store of an index of 0.2.0 has no row
	first := &countingWalk{}
	w.srv.marked.walk = first.walk
	if got := w.deletedAnswer("store-a"); got["blobCount"] != float64(1) || got["bytes"] != float64(len("old bytes")) {
		t.Fatalf("first answer %v", got)
	}
	if n := first.calls.Load(); n != 1 {
		t.Fatalf("%d walks, want 1", n)
	}
	if row, found := w.markedRow("store-a"); !found || row.Blobs != 1 {
		t.Fatalf("the walk wrote no row: %+v %v", row, found)
	}
	w.restart(true)
	second := &countingWalk{}
	w.srv.marked.walk = second.walk
	if got := w.deletedAnswer("store-a"); got["blobCount"] != float64(1) || got["counting"] != false {
		t.Fatalf("after the start %v", got)
	}
	if n := second.calls.Load(); n != 0 {
		t.Fatalf("the second start walked %d times", n)
	}
}

// Ruling SR16 with the order of 2026-11-21: marked-counts.json of the data
// directory gives the row of a store of the old product without a row, when
// its root is the root of the store and nothing was unreadable; else the
// store is walked. A store of this server takes no seed.
func TestASeedFileGivesTheFirstCount(t *testing.T) {
	cases := []struct {
		label     string
		entry     func(root string) map[string]any
		store     string
		walks     int64
		wantBlobs float64
	}{
		{"seed", func(root string) map[string]any {
			return map[string]any{"blobs": 41, "bytes": 4100, "files": 50, "unreadable": 0, "seconds": 1.5, "countedAt": "2026-11-21T06:00:00Z", "root": root}
		}, "store-a", 0, 41},
		{"another root", func(root string) map[string]any {
			return map[string]any{"blobs": 41, "bytes": 4100, "files": 50, "unreadable": 0, "seconds": 1.5, "countedAt": "2026-11-21T06:00:00Z", "root": root + "-other"}
		}, "store-a", 1, 1},
		{"unreadable files", func(root string) map[string]any {
			return map[string]any{"blobs": 41, "bytes": 4100, "files": 50, "unreadable": 2, "seconds": 1.5, "countedAt": "2026-11-21T06:00:00Z", "root": root}
		}, "store-a", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			gone := testkit.NewBlobSpec("demo-raw", "old/gone.txt")
			gone.Content, gone.Deleted = []byte("old bytes"), true
			w := startIngest(t, nil, gone)
			if _, err := w.srv.Rescan(context.Background(), "local-write"); err != nil {
				t.Fatal(err)
			}
			w.dropMarkedRow("store-a")
			w.dropMarkedRow("local-write")
			seed := map[string]any{c.store: c.entry(w.old), "local-write": c.entry(w.root())}
			data, _ := json.Marshal(seed)
			if err := os.WriteFile(filepath.Join(w.cfg.DataDir, "marked-counts.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			walks := &countingWalk{}
			w.srv.marked.walk = walks.walk
			got := w.deletedAnswer(c.store)
			if got["blobCount"] != c.wantBlobs || walks.calls.Load() != c.walks {
				t.Fatalf("answer %v after %d walks", got, walks.calls.Load())
			}
			if c.walks == 0 && !strings.HasPrefix(got["countedAt"].(string), "2026-11-21T06:00:00") {
				t.Fatalf("countedAt %v", got["countedAt"])
			}
			before := walks.calls.Load()
			if own := w.deletedAnswer("local-write"); own["blobCount"] == float64(41) || walks.calls.Load() != before+1 {
				t.Fatalf("local-write took the seed: %v", own)
			}
			w.restart(true)
			again := &countingWalk{}
			w.srv.marked.walk = again.walk
			if got := w.deletedAnswer(c.store); got["blobCount"] != c.wantBlobs || again.calls.Load() != 0 {
				t.Fatalf("after the start %v, %d walks", got, again.calls.Load())
			}
		})
	}
}

// Review P3: the compaction takes off the bytes the count holds for a
// blob, the size of its properties, also when its bytes file is gone.
func TestACompactionWithoutTheBytesFileTakesOffItsCount(t *testing.T) {
	w := startIngest(t, nil)
	root := createdStoreRepo(t, w, "demo-team", "demo-t", true)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-20*day), "demo-t", "docs/a.txt", "five!")
	if _, err := w.srv.Rescan(ctx, "demo-team"); err != nil {
		t.Fatal(err)
	}
	row, err := w.srv.rp.index.Lookup("demo-t", "docs/a.txt")
	if err != nil || row == nil {
		t.Fatal(err)
	}
	w.at(now.Add(-10 * day))
	if _, err := w.srv.erase(w.repo("demo-t"), "docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	w.srv.marked.flush()
	if m, _ := w.markedRow("demo-team"); m.Blobs != 1 || m.Bytes != 5 {
		t.Fatalf("after the mark %+v", m)
	}
	if err := os.Remove(filepath.Join(root, row.Location)); err != nil {
		t.Fatal(err)
	}
	w.srv.side().setClock(func() time.Time { return now })
	w.at(now)
	if c, err := w.srv.CompactStore(ctx, "demo-team", 7*day); err != nil || c.Removed != 1 {
		t.Fatalf("compaction %+v %v", c, err)
	}
	w.srv.marked.flush()
	if m, _ := w.markedRow("demo-team"); m.Blobs != 0 || m.Bytes != 0 {
		t.Fatalf("after the compaction %+v", m)
	}
}

// Review P5, P6: the close of the server writes the deltas that wait, so
// that the next start reads them from the row.
func TestTheCloseWritesTheDeltas(t *testing.T) {
	w := startIngest(t, nil)
	createdStoreRepo(t, w, "demo-team", "demo-t", true)
	ctx := context.Background()
	w.put(time.Now(), "demo-t", "docs/a.txt", "four")
	if _, err := w.srv.Rescan(ctx, "demo-team"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.erase(w.repo("demo-t"), "docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	w.restart(true)
	if m, found := w.markedRow("demo-team"); !found || m.Blobs != 1 || m.Bytes != 4 {
		t.Fatalf("after the restart %+v %v", m, found)
	}
}

// Gate finding of the contract walk: a delta is in the row once the change
// that made it returned, so that no later request sees catalog.sqlite
// change.
func TestADeltaIsWrittenAtOnce(t *testing.T) {
	w := startIngest(t, nil)
	createdStoreRepo(t, w, "demo-team", "demo-t", true)
	w.put(time.Now(), "demo-t", "docs/a.txt", "four")
	if _, err := w.srv.Rescan(context.Background(), "demo-team"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.erase(w.repo("demo-t"), "docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	if m, found := w.markedRow("demo-team"); !found || m.Blobs != 1 || m.Bytes != 4 {
		t.Fatalf("right after the delete %+v %v", m, found)
	}
}
