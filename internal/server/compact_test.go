package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
)

// The compaction of a store of this server (task 12 of the phase 6 plan,
// decision A8, rulings CL4, CL5, CL9, CL10 and CL12 of leg 6-cleanup).

// catalogBlob reports a row of the blobs table of catalog.sqlite.
func (w *writing) catalogBlob(store, repo, path string) bool {
	w.t.Helper()
	cat, err := catalog.Open(w.cfg.DataDir)
	if err != nil {
		w.t.Fatal(err)
	}
	defer cat.Close()
	_, ok, err := cat.BlobCreated(store, repo, path)
	if err != nil {
		w.t.Fatal(err)
	}
	return ok
}

// Marks of a cleanup run ten days ago: a setting of 30 days keeps them, a
// frozen server stops the run with nothing removed, a setting of 7 days
// removes them but the one whose path a write holds (CL5); the blob a write
// replaced goes, while its path stays live with its catalog row; a stale
// catalog row of a removed blob goes with it; a store of the old product
// and an unknown store are refused; no message names a path.
func TestCompactionRemovesOldMarksOfAStoreOfThisServer(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, p := range []string{"docs/a.txt", "docs/b.txt", "docs/held.txt", "docs/replaced.txt"} {
		w.put(now.Add(-40*day), "demo-raw", p, p)
	}
	w.put(now.Add(-10*day), "demo-raw", "docs/replaced.txt", "two")
	w.catalogRescan()
	a, err := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt")
	if err != nil || a == nil {
		t.Fatal(err)
	}
	w.at(now.Add(-10 * day))
	if c, err := w.srv.CleanupRun(ctx, agePolicy("raw", 30), "demo-raw"); err != nil || c.Removed != 3 {
		t.Fatalf("cleanup %+v %v", c, err)
	}
	cat, err := catalog.Open(w.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	stale := index.Asset{Repo: "demo-raw", Path: "docs/a.txt", Store: a.Store, Location: a.Location, Size: a.Size, CreatedMS: a.CreatedMS}
	if err := cat.Put(stale, "raw", catalog.Digests{}, "", ""); err != nil {
		t.Fatal(err)
	}
	cat.Close()
	w.srv.side().setClock(func() time.Time { return now })

	marked, err := w.srv.MarkedBlobs(ctx, "local-write", 2)
	wantBytes := int64(len("docs/a.txt") + len("docs/b.txt") + len("docs/held.txt") + len("docs/replaced.txt"))
	if err != nil || marked.Count != 4 || marked.Bytes != wantBytes || len(marked.Blobs) != 2 || !marked.Truncated {
		t.Fatalf("marked %+v %v", marked, err)
	}
	if b := marked.Blobs[0]; b.Repository != "demo-raw" || !strings.HasPrefix(b.Path, "docs/") || b.Reason == "" ||
		!b.MarkedAt.Equal(now.Add(-10*day)) {
		t.Fatalf("a marked blob %+v", b)
	}
	if c, err := w.srv.CompactStore(ctx, "local-write", 30*day); err != nil || c != (CompactCounts{Kept: 4}) {
		t.Fatalf("a setting of 30 days %+v %v", c, err)
	}
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	if c, err := w.srv.CompactStore(ctx, "local-write", 7*day); err == nil || err.Error() != "stopped: the server froze" || c.Removed != 0 {
		t.Fatalf("a frozen run %+v %v", c, err)
	}
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	unlock := w.srv.writes.locks.lock("demo-raw", "docs/held.txt")
	c, err := w.srv.CompactStore(ctx, "local-write", 7*day)
	unlock()
	if err != nil || c != (CompactCounts{Removed: 3, Skipped: 1, Bytes: wantBytes - int64(len("docs/held.txt"))}) {
		t.Fatalf("a setting of 7 days %+v %v", c, err)
	}
	if counts, live := w.scan(); counts.Deleted != 1 || counts.Copies != 1 || len(live) != 1 || live[0].Path != "docs/replaced.txt" {
		t.Fatalf("scan after %+v %v", counts, live)
	}
	if w.catalogBlob(a.Store, "demo-raw", "docs/a.txt") {
		t.Fatal("the catalog row of a removed blob stays")
	}
	if !w.catalogBlob(a.Store, "demo-raw", "docs/replaced.txt") || !slices.Equal(w.livePaths(), []string{"demo-raw/docs/replaced.txt"}) {
		t.Fatalf("the live path of a replaced blob is gone: %v", w.livePaths())
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/replaced.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "two" {
		t.Fatalf("the live path answers %d", r.status)
	}
	if c, err := w.srv.CompactStore(ctx, "local-write", 7*day); err != nil || c != (CompactCounts{Removed: 1, Bytes: int64(len("docs/held.txt"))}) {
		t.Fatalf("a second run %+v %v", c, err)
	}
	if marked, err := w.srv.MarkedBlobs(ctx, "local-write", 10); err != nil || marked.Count != 0 || len(marked.Blobs) != 0 {
		t.Fatalf("marked after %+v %v", marked, err)
	}
	for store, want := range map[string]string{"store-a": "the store of the task is not a store of this server",
		"no-such-store": "the store of the task does not exist"} {
		if _, err := w.srv.CompactStore(ctx, store, 7*day); err == nil || err.Error() != want {
			t.Errorf("%s: %v", store, err)
		}
		if _, err := w.srv.MarkedBlobs(ctx, store, 10); err == nil || err.Error() != want {
			t.Errorf("marked of %s: %v", store, err)
		}
	}
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.srv.CompactStore(stopped, "local-write", 7*day); err != context.Canceled {
		t.Fatalf("a stopped run: %v", err)
	}
}
