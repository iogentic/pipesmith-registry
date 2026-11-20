package server

import (
	"errors"
	"testing"
	"time"
)

// Waiting marks (plan 0.2.0, Write Path): a delete mark that failed waits in
// the memory of the process. A restart between the failed mark and the next
// write of the path must not leave the replaced blob live: the start finds
// it in the write store, and the write after it ends with one live blob.
func TestAWaitingMarkOutlivesARestart(t *testing.T) {
	w := startIngestWithIDs(t, lowID, highID, "00000000-0000-4000-8000-000000000003")
	first, second := w.replaceWithAFailedMark()
	if n := w.srv.writes.waiting(); n != 1 {
		t.Fatalf("%d marks wait before the restart", n)
	}
	w.restart(true)
	w.clock.add(time.Millisecond)
	third, err := w.ingest("demo-raw", "docs/a.txt", "third")
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range []*ingestResult{first, second} {
		if props := w.properties(blob.asset.Location); props["deleted"] != "true" || props["deletedReason"] != reasonReplaced {
			t.Fatalf("%s after the write: %q", blob.asset.Location, props)
		}
	}
	if counts, live := w.scan(); counts.Live != 1 || live[0].Location != third.asset.Location {
		t.Fatalf("the write store after the write: %+v %+v", counts, live)
	}
	if n := w.srv.writes.waiting(); n != 0 {
		t.Fatalf("%d marks wait", n)
	}
}

// A mark that fails at the start too waits again: the writable status
// answers 503, and the next write of the path sets the mark.
func TestAMarkThatFailsAtTheStartWaitsForTheNextWrite(t *testing.T) {
	w := startIngestWithIDs(t, lowID, highID, "00000000-0000-4000-8000-000000000003")
	first, second := w.replaceWithAFailedMark()
	refused := func(location, _ string) error {
		if location == first.asset.Location {
			return errors.New("the disk refuses the mark")
		}
		return nil
	}
	w.opts.marksFault = refused
	w.restart(true)
	if n := w.srv.writes.waiting(); n != 1 {
		t.Fatalf("%d marks wait after the start, 1 is the blob whose mark failed", n)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "" {
		t.Fatalf("the refused mark is set: %q", props)
	}
	if r := w.get(writablePath); r.status != 503 {
		t.Fatalf("writable while the mark waits: %s", r)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row == nil || row.Location != second.asset.Location {
		t.Fatalf("the start serves %+v", row)
	}
	w.srv.writes.failMarks(nil)
	w.clock.add(time.Millisecond)
	if _, err := w.ingest("demo-raw", "docs/a.txt", "third"); err != nil {
		t.Fatal(err)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "true" || props["deletedReason"] != reasonReplaced {
		t.Fatalf("the first blob after the write: %q", props)
	}
	if n := w.srv.writes.waiting(); n != 0 {
		t.Fatalf("%d marks wait", n)
	}
}
