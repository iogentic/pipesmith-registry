package server

import (
	"testing"
	"time"
)

// Clock after restart (plan 0.2.0, Write Path findings): the creation time
// of a blob a delete of its path marked must hold over a restart. A clock
// that steps back after the restart must not sort the next write of the
// path behind the delete, or the scan of the write store drops the path.
func TestAClockThatStepsBackAfterARestartKeepsTheOrderOfAPath(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	first, err := w.ingest("demo-raw", "docs/a.txt", "first")
	if err != nil {
		t.Fatal(err)
	}
	w.clock.add(time.Second)
	if removed, err := w.srv.erase(w.repo("demo-raw"), "docs/a.txt"); err != nil || removed == nil {
		t.Fatalf("%+v %v", removed, err)
	}
	w.restart(true)
	w.clock.add(-5 * time.Second)
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
	if props := w.properties(first.asset.Location); props["deleted"] != "true" || props["deletedReason"] != reasonDeleted {
		t.Fatalf("the deleted blob: %q", props)
	}
}
