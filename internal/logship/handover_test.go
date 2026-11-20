package logship

import (
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// tapWith is what the tap of the manager does with the list of senders it
// read: one offer per sender whose row takes the kind of the line.
func tapWith(list []*sender, line logs.Line) {
	for _, s := range list {
		if s.mask&fileBits[line.Kind] != 0 {
			s.offer(line)
		}
	}
}

// When a row changes, a line reaches exactly one of the old and the new
// sender. Four goroutines ship lines with unique ids while the address of
// the row moves from stub A to stub B, and a tap that read the senders before
// the change offers its line once the old sender has ended. Every id
// arrives once, at A or at B: none twice, none lost, none dropped.
func TestChangedRowTakesEachLineOnce(t *testing.T) {
	a, b := newWebStub(t, nil), newWebStub(t, nil)
	clk := newFakeClock()
	var set setting
	row := map[string]any{"name": "demo", "kind": "http-json", "address": a.url(), "batch_lines": 100, "queue_lines": 1_000_000, "queue_bytes": 256 << 20}
	set.put(t, row)
	var log testLog
	m := startManager(t, clk, &set, &log)
	before := *m.senders.Load() // what a tap read before the change
	kinds := []string{"server", "requests", "audit", "tasks"}
	const most = 50_000 // lines per goroutine, so the queue of 1,000,000 lines and 256 MiB never fills
	stop := make(chan struct{})
	var wg sync.WaitGroup
	shipped := make([]int, len(kinds))
	for g, kind := range kinds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < most; n++ {
				select {
				case <-stop:
					shipped[g] = n
					return
				default:
				}
				m.Ship(testLine(kind, n))
				if n%10 == 9 {
					time.Sleep(20 * time.Microsecond)
				}
			}
			shipped[g] = most
		}()
	}
	waitFor(t, func() bool { return len(a.lines()) >= 1000 }, "1,000 lines at A")
	row["address"] = b.url()
	set.put(t, row)
	clk.waitPending(t, 5*time.Second)
	clk.Advance(5 * time.Second)
	waitFor(t, func() bool { return len(log.records(t, "log target stopped")) == 1 }, "the old sender of the row stopped")
	waitFor(t, func() bool { return len(b.lines()) >= 1000 }, "1,000 lines at B")
	late := testLine("server", 990_000_000)
	tapWith(before, late)
	close(stop)
	wg.Wait()
	m.Close()

	got := map[string]int{}
	for _, stub := range []*webStub{a, b} {
		for _, id := range idsOf(t, stub.lines()) {
			got[id]++
		}
	}
	want := idsWant([]logs.Line{late})
	for g, kind := range kinds {
		want = append(want, idsWant(testLines(kind, 0, shipped[g]))...)
	}
	missing, twice := 0, 0
	for _, id := range want {
		switch got[id] {
		case 0:
			missing++
			if missing <= 5 {
				t.Errorf("%s did not arrive", id)
			}
		case 1:
		default:
			twice++
			if twice <= 5 {
				t.Errorf("%s arrived %d times", id, got[id])
			}
		}
	}
	total := 0
	for _, n := range got {
		total += n
	}
	t.Logf("%d lines shipped, %d at A and %d at B; %d missing, %d more than once", len(want), len(a.lines()), len(b.lines()), missing, twice)
	if total != len(want) || missing != 0 || twice != 0 {
		t.Fatalf("%d lines arrived for %d shipped: %d missing, %d more than once", total, len(want), missing, twice)
	}
	stops := log.records(t, "log target stopped")
	if len(stops) != 2 {
		t.Fatalf("%d stop lines, want the old and the new sender", len(stops))
	}
	for _, rec := range stops {
		if number(t, rec, "dropped") != 0 || number(t, rec, "failed") != 0 {
			t.Errorf("stop line %v", rec)
		}
	}
}
