package server

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The open point of leg 4d: a delete mark that waits lives in the memory of
// the process. A kill between the row of a write and the mark of the blob it
// replaced leaves two live blobs of one path in the write store, and the
// next start knows nothing of the mark. A scan of the write store serves the
// blob written last and gives every older live blob of the path its mark.

const (
	lowID  = "00000000-0000-4000-8000-000000000001"
	highID = "ffffffff-ffff-4fff-bfff-fffffffffffe"
)

// walkOrders give the first blob of a path the id the walk meets first, and
// then the one it meets last.
var walkOrders = []struct {
	name string
	ids  []string
}{{"the older blob walked first", []string{lowID, highID}}, {"the newer blob walked first", []string{highID, lowID}}}

// restart stops the server, so that every mark that waits is gone as after a
// kill, and starts a new one on the same data directory, with writes on or
// off.
func (w *writing) restart(writes bool) {
	w.t.Helper()
	w.ts.Close()
	for _, ts := range w.docker {
		ts.Close()
	}
	if err := w.srv.Close(); err != nil {
		w.t.Fatal(err)
	}
	opts := w.opts
	opts.writes = writes
	w.app = startApp(w.t, w.base, w.cfg, opts)
}

// scanWrites is a scan of the write store through the job of the server, as
// POST /api/scan starts it, and waits for its end.
func (w *writing) scanWrites() {
	w.t.Helper()
	st, err := w.srv.rp.sync()
	if err != nil {
		w.t.Fatal(err)
	}
	store := st.stores["local-write"]
	if store == nil {
		w.t.Fatal("the write store is not attached")
	}
	if !w.srv.job.start(store) {
		w.t.Fatal("the scan did not start")
	}
	deadline := time.Now().Add(30 * time.Second)
	for running(w.srv.job.snapshot()) {
		if time.Now().After(deadline) {
			w.t.Fatal("the scan did not end")
		}
		time.Sleep(time.Millisecond)
	}
	last, _ := w.srv.job.snapshot().Get("last")
	outcome, _ := last.(*pyjson.Object).Get("local-write")
	if e, _ := outcome.(*pyjson.Object).Get("error"); e != nil {
		w.t.Fatalf("the scan of the write store failed: %v", e)
	}
}

// expectServed checks that the path of served is served from it, that older
// carries the delete mark with reason, and that served is the one live blob
// of the write store.
func (w *writing) expectServed(served, older *ingestResult, reason string) {
	w.t.Helper()
	row, err := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt")
	if err != nil || row == nil || row.Location != served.asset.Location {
		w.t.Fatalf("the scan serves %+v, the blob written last is %s", row, served.asset.Location)
	}
	if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 200 || string(r.body) != "second" {
		w.t.Fatalf("%d %q", r.status, r.body)
	}
	if props := w.properties(older.asset.Location); props["deleted"] != "true" || props["deletedReason"] != reason {
		w.t.Fatalf("the older blob after the scan: %q", props)
	}
	if counts, live := w.scan(); counts.Live != 1 || live[0].Location != served.asset.Location {
		w.t.Fatalf("the write store after the scan: %+v %+v", counts, live)
	}
}

// dropLogLine removes the line of a blob from the reconciliation log, as a
// kill between the rename of its properties and the line leaves it.
func (w *writing) dropLogLine(location string) {
	w.t.Helper()
	id := strings.TrimSuffix(path.Base(location), stores.BytesSuffix)
	dir := filepath.Join(w.root(), "reconciliation")
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.t.Fatal(err)
	}
	dropped := 0
	for _, e := range entries {
		name := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(name)
		if err != nil {
			w.t.Fatal(err)
		}
		var kept []string
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if strings.Contains(line, ","+id+",") {
				dropped++
				continue
			}
			kept = append(kept, line)
		}
		if err := os.WriteFile(name, []byte(strings.Join(kept, "")), 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
	if dropped != 1 {
		w.t.Fatalf("%d log lines of blob %s", dropped, id)
	}
}

// A kill between the row of the second write and the mark of the first: the
// next start and every scan after it serve the newer blob and mark the older
// one.
func TestAScanMarksTheBlobAKillLeftLive(t *testing.T) {
	for _, c := range walkOrders {
		t.Run(c.name, func(t *testing.T) {
			w := startIngestWithIDs(t, c.ids...)
			first, second := w.replaceWithAFailedMark()
			w.restart(true)
			if n := w.srv.writes.waiting(); n != 0 {
				t.Fatalf("%d marks wait after the start", n)
			}
			w.scanWrites()
			w.expectServed(second, first, reasonReplaced)
		})
	}
}

// The same with both writes in one millisecond (Review Focus 3 of the
// execution plan): both blobs carry one creationTime, which does not tell
// which was written last.
func TestAScanServesTheLaterOfTwoBlobsOfOneMillisecond(t *testing.T) {
	for _, c := range walkOrders {
		t.Run(c.name, func(t *testing.T) {
			w := startIngestWithIDs(t, c.ids...)
			first, second := w.replaceAfter(0)
			if first.asset.CreatedMS != second.asset.CreatedMS {
				t.Fatalf("creation times %d and %d", first.asset.CreatedMS, second.asset.CreatedMS)
			}
			w.restart(true)
			w.scanWrites()
			w.expectServed(second, first, reasonReplaced)
		})
	}
}

// A kill that came after the rename of the properties of the second blob and
// before its line in the log: the blob without a line is the later one. With
// writes off the write store is read only, so the scan serves the right blob
// and marks nothing; the next scan with writes on sets the mark.
func TestABlobWithoutItsLogLineIsTheLaterOne(t *testing.T) {
	w := startIngestWithIDs(t, lowID, highID)
	first, second := w.replaceAfter(0)
	w.dropLogLine(second.asset.Location)
	w.restart(false)
	w.scanWrites()
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row == nil || row.Location != second.asset.Location {
		t.Fatalf("with writes off the scan serves %+v", row)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "" {
		t.Fatalf("a scan with writes off wrote a mark: %q", props)
	}
	w.restart(true)
	w.scanWrites()
	w.expectServed(second, first, reasonReplaced)
}

// forgetWaiting drops every mark that waits, as a kill did before a start
// repaired the write store (repairAtStart), while the server runs on.
func (w *writing) forgetWaiting() {
	w.srv.writes.marks.mu.Lock()
	defer w.srv.writes.marks.mu.Unlock()
	w.srv.writes.marks.waiting, w.srv.writes.marks.count = nil, 0
}

// A delete marks the blob of the row; the older live blob is the one whose
// mark was lost. A scan of the write store must not bring the deleted path
// back. A start repairs such a blob (TestAStartRepairsTwoLiveBlobsOfAPath),
// so the loss of the mark is made inside the process.
func TestAScanKeepsAPathDeletedAfterTheStart(t *testing.T) {
	for _, step := range []time.Duration{time.Millisecond, 0} {
		t.Run(step.String(), func(t *testing.T) {
			w := startIngestWithIDs(t, lowID, highID)
			first, second := w.replaceAfter(step)
			w.forgetWaiting()
			w.srv.writes.failMarks(nil)
			removed, err := w.srv.erase(w.repo("demo-raw"), "docs/a.txt")
			if err != nil || removed == nil || removed.Location != second.asset.Location {
				t.Fatalf("%+v %v", removed, err)
			}
			w.scanWrites()
			if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row != nil {
				t.Fatalf("the scan brings the deleted path back: %+v", row)
			}
			if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 404 {
				t.Fatalf("%d %q", r.status, r.body)
			}
			if props := w.properties(first.asset.Location); props["deleted"] != "true" || props["deletedReason"] != reasonDeleted {
				t.Fatalf("the older blob after the scan: %q", props)
			}
			if counts, _ := w.scan(); counts.Live != 0 {
				t.Fatalf("%+v", counts)
			}
		})
	}
}

// The scan of the write store orders the blobs of a path by their
// creationTime, so a clock that steps back must not put a new blob behind an
// older blob of its path in the write store (finding S5 of review 4): a new
// blob passes the newest blob of its path there, the blob of its row and a
// blob that a delete of the path marked.
func TestAClockThatStepsBackKeepsTheOrderOfAPath(t *testing.T) {
	t.Run("a put after a delete", func(t *testing.T) {
		w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
		first, err := w.ingest("demo-raw", "docs/a.txt", "first")
		if err != nil {
			t.Fatal(err)
		}
		w.clock.add(-5 * time.Second)
		w.clock.add(2 * time.Second) // the delete comes 2 s after the put, on a clock 5 s back
		if removed, err := w.srv.erase(w.repo("demo-raw"), "docs/a.txt"); err != nil || removed == nil {
			t.Fatalf("%+v %v", removed, err)
		}
		second, err := w.ingest("demo-raw", "docs/a.txt", "second")
		if err != nil {
			t.Fatal(err)
		}
		if second.asset.CreatedMS <= first.asset.CreatedMS {
			t.Errorf("the second blob has creationTime %d, the deleted one %d", second.asset.CreatedMS, first.asset.CreatedMS)
		}
		w.scanWrites()
		if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 200 || string(r.body) != "second" {
			t.Fatalf("after the scan: %d %q", r.status, r.body)
		}
		if props := w.properties(second.asset.Location); props["deleted"] != "" {
			t.Fatalf("the scan marked the last put: %q", props)
		}
	})
	t.Run("a put over a blob whose mark failed", func(t *testing.T) {
		w := startIngestWithIDs(t, lowID, highID)
		first, second := w.replaceAfter(-5 * time.Second)
		if second.asset.CreatedMS <= first.asset.CreatedMS {
			t.Errorf("the second blob has creationTime %d, the first %d", second.asset.CreatedMS, first.asset.CreatedMS)
		}
		w.restart(true)
		w.scanWrites()
		w.expectServed(second, first, reasonReplaced)
	})
}
