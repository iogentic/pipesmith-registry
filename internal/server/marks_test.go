package server

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// startIngestWithIDs is startIngest with the blob ids drawn from ids, in
// their order, for demo-raw under ALLOW.
func startIngestWithIDs(t *testing.T, ids ...string) *writing {
	t.Helper()
	sec := newFakeSecurity()
	snap := *sec.snap
	snap.Repositories = []model.Repository{{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: storage("ALLOW")}}
	sec.snap = &snap
	base := t.TempDir()
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a")}
	w := &writing{catalog: &fakeCatalog{}, clock: &clock{now: time.Date(2026, 11, 18, 10, 15, 30, 123_000_000, time.UTC)},
		old: storeCfgs[0].Root}
	next := 0
	newID := func() string {
		id := ids[next%len(ids)]
		next++
		return id
	}
	w.opts = appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, security: sec, writes: true, catalog: w.catalog,
		writeOptions: blobwrite.Options{Now: w.clock.Now, NewID: newID}}
	w.app = startApp(t, base, prepare(t, base, storeCfgs, w.opts), w.opts)
	return w
}

// rescan replaces the rows of the write store in the index of the server
// with what its files hold, as a scan of the store does.
func (w *writing) rescan() {
	w.t.Helper()
	s, err := stores.NewBlobStore("local-write", w.root())
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := scan.Run(s, w.srv.rp.index, nil); err != nil {
		w.t.Fatal(err)
	}
}

// replaceWithAFailedMark writes "first" and then "second" over docs/a.txt
// one millisecond later, with the delete mark of the first blob refused.
func (w *writing) replaceWithAFailedMark() (first, second *ingestResult) {
	w.t.Helper()
	return w.replaceAfter(time.Millisecond)
}

// replaceAfter is replaceWithAFailedMark with the second write step after
// the first; 0 writes both in one millisecond.
func (w *writing) replaceAfter(step time.Duration) (first, second *ingestResult) {
	w.t.Helper()
	first, err := w.ingest("demo-raw", "docs/a.txt", "first")
	if err != nil {
		w.t.Fatal(err)
	}
	w.clock.add(step)
	w.srv.writes.failMarks(func(location, _ string) error {
		if location == first.asset.Location {
			return errors.New("the disk refuses the mark")
		}
		return nil
	})
	second, err = w.ingest("demo-raw", "docs/a.txt", "second")
	if err != nil {
		w.t.Fatalf("a write that is stored fails for the mark of the blob it replaced: %v", err)
	}
	if second.replaced == nil || second.replaced.Location != first.asset.Location {
		w.t.Fatalf("replaced %+v", second.replaced)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "" {
		w.t.Fatalf("the first blob is marked: %q", props)
	}
	return first, second
}

// Risk 3 of the 4a report: the delete mark of a replaced blob fails after
// the new blob is written. The write answers as written. Two live blobs of
// the path lie in the store, and a scan from the files serves the one with
// the newer creation time, whichever of them its walk meets first. The
// writable status answers 503 while the mark waits, so that a client that
// asks it before it writes holds back; the mark is tried again at each
// request of the status and is set once the store takes it.
func TestAFailedMarkOfAReplacedBlob(t *testing.T) {
	low, high := "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-fffffffffffe"
	for _, c := range []struct {
		name string
		ids  []string
	}{{"the newer blob walked last", []string{low, high}}, {"the newer blob walked first", []string{high, low}}} {
		t.Run(c.name, func(t *testing.T) {
			w := startIngestWithIDs(t, c.ids...)
			now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
			w.srv.writes.retryClock(func() time.Time { return now }) // the writable status tries the marks at most once per second
			first, second := w.replaceWithAFailedMark()
			if counts, live := w.scan(); counts.Live != 2 || len(live) != 2 || live[0].Path != live[1].Path {
				t.Fatalf("scan %+v %+v", counts, live)
			}
			w.rescan()
			if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row == nil || row.Location != second.asset.Location ||
				row.CreatedMS != first.asset.CreatedMS+1 {
				t.Fatalf("the scan serves %+v, the newer blob is %+v", row, second.asset)
			}
			if r := w.fetch("demo-raw", "docs/a.txt"); string(r.body) != "second" {
				t.Fatalf("%d %q", r.status, r.body)
			}
			if r := w.get(writablePath); r.status != 503 {
				t.Fatalf("writable while the mark waits: %s", r)
			}
			if r := w.get(writablePath); r.status != 503 {
				t.Fatalf("writable while the mark still waits: %s", r)
			}
			w.srv.writes.failMarks(nil)
			w.srv.writes.retryClock(func() time.Time { return now.Add(retryEvery) })
			if r := w.get(writablePath); r.status != 200 {
				t.Fatalf("writable once the store takes the mark: %s", r)
			}
			if props := w.properties(first.asset.Location); props["deleted"] != "true" || props["deletedReason"] != reasonReplaced {
				t.Fatalf("the first blob after the retry: %q", props)
			}
			if counts, _ := w.scan(); counts.Live != 1 {
				t.Fatalf("scan after the retry %+v", counts)
			}
		})
	}
}

// A mark that waits is tried again at the next write of its path, and the
// list of waiting marks is bounded.
func TestAWaitingMarkGoesWithTheNextWriteOfThePath(t *testing.T) {
	w := startIngestWithIDs(t, "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003")
	first, _ := w.replaceWithAFailedMark()
	w.srv.writes.failMarks(nil)
	w.clock.add(time.Millisecond)
	if _, err := w.ingest("demo-raw", "docs/a.txt", "third"); err != nil {
		t.Fatal(err)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "true" {
		t.Fatalf("%q", props)
	}
	if counts, _ := w.scan(); counts.Live != 1 {
		t.Fatalf("%+v", counts)
	}
	if n := w.srv.writes.waiting(); n != 0 {
		t.Fatalf("%d marks wait", n)
	}
	for i := range maxWaitingMarks + 5 {
		w.srv.writes.wait("demo-raw", fmt.Sprintf("docs/%d.txt", i), fmt.Sprintf("content/x/%d.bytes", i))
	}
	if n := w.srv.writes.waiting(); n != maxWaitingMarks {
		t.Fatalf("%d marks wait, the bound is %d", n, maxWaitingMarks)
	}
}

// Risk 4 of the 4a report: erase marks the row of a path. Two rows of one
// path cannot lie in the write store: the index keeps one row per store and
// path, and a write replaces it. Two live blobs of one path can, after a
// failed mark; a delete marks both, so that no scan brings the path back.
func TestEraseMarksEveryLiveBlobOfThePath(t *testing.T) {
	w := startIngestWithIDs(t, "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002")
	first, second := w.replaceWithAFailedMark()
	rows, err := w.srv.rp.index.Rows("demo-raw", "docs/a.txt")
	if err != nil || len(rows) != 1 || rows[0].Location != second.asset.Location {
		t.Fatalf("rows of the path %+v %v", rows, err)
	}
	if _, err := w.srv.rp.index.Put("local-write", stores.Entry{Repo: "demo-raw", Path: "docs/a.txt", Location: first.asset.Location,
		Size: first.asset.Size, SHA1: first.asset.SHA1, CreatedMS: first.asset.CreatedMS}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := w.srv.rp.index.Rows("demo-raw", "docs/a.txt"); len(rows) != 1 || rows[0].Location != first.asset.Location {
		t.Fatalf("a second Put of the path: %+v", rows)
	}
	w.rescan()
	w.srv.writes.failMarks(nil)
	removed, err := w.srv.erase(w.repo("demo-raw"), "docs/a.txt")
	if err != nil || removed == nil {
		t.Fatalf("%+v %v", removed, err)
	}
	for _, blob := range []*ingestResult{first, second} {
		if props := w.properties(blob.asset.Location); props["deleted"] != "true" {
			t.Fatalf("%s after the delete: %q", blob.asset.Location, props)
		}
	}
	if counts, _ := w.scan(); counts.Live != 0 {
		t.Fatalf("%+v", counts)
	}
	w.rescan()
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row != nil {
		t.Fatalf("a scan brings the path back: %+v", row)
	}
	if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 404 {
		t.Fatalf("%d %q", r.status, r.body)
	}
}
