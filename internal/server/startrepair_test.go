package server

import (
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// Repair at start (plan 0.2.0, Write Path): a kill between the row of a
// write and the delete mark of the blob it replaced leaves two live blobs of
// one path in the write store. The next start with writes on serves the blob
// the scan of the write store keeps and gives the other its mark, before it
// takes the first write, with no POST /api/scan.
func TestAStartRepairsTwoLiveBlobsOfAPath(t *testing.T) {
	for _, c := range walkOrders {
		for _, step := range []time.Duration{time.Millisecond, 0} {
			t.Run(c.name+" "+step.String(), func(t *testing.T) {
				w := startIngestWithIDs(t, c.ids...)
				first, second := w.replaceAfter(step)
				if counts, _ := w.scan(); counts.Live != 2 {
					t.Fatalf("before the start: %+v", counts)
				}
				w.restart(true)
				w.expectServed(second, first, reasonReplaced)
				if n := w.srv.writes.waiting(); n != 0 {
					t.Fatalf("%d marks wait after the start", n)
				}
			})
		}
	}
	// The row of the path names the older blob: the start serves the blob
	// the arbiter of the scan keeps, the one written last.
	t.Run("the row names the older blob", func(t *testing.T) {
		w := startIngestWithIDs(t, lowID, highID)
		first, second := w.replaceWithAFailedMark()
		if _, err := w.srv.rp.index.Put("local-write", stores.Entry{Repo: "demo-raw", Path: "docs/a.txt", Location: first.asset.Location,
			Size: first.asset.Size, SHA1: first.asset.SHA1, CreatedMS: first.asset.CreatedMS}); err != nil {
			t.Fatal(err)
		}
		w.restart(true)
		w.expectServed(second, first, reasonReplaced)
	})
}

// With writes off the write store is read only: a start repairs nothing.
func TestAStartWithWritesOffMarksNothing(t *testing.T) {
	w := startIngestWithIDs(t, lowID, highID)
	first, _ := w.replaceWithAFailedMark()
	w.restart(false)
	if props := w.properties(first.asset.Location); props["deleted"] != "" {
		t.Fatalf("a start with writes off wrote a mark: %q", props)
	}
	if counts, _ := w.scan(); counts.Live != 2 {
		t.Fatalf("%+v", counts)
	}
}
