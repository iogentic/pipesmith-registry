package logship

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Close with a healthy target sends everything queued before it returns,
// logs the stop line with the counters, and leaves no goroutine behind.
func TestCloseSendsWhatIsQueued(t *testing.T) {
	stub := newWebStub(t, nil)
	before := runtime.NumGoroutine()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 40,
		"flush_interval_ms": 60000})
	var log testLog
	m := newManager(options(&set, &log), realClock{})
	lines := testLines("audit", 0, 100)
	shipAll(t, m, lines)
	start := time.Now()
	m.Close()
	took := time.Since(start)
	if got, want := idsOf(t, stub.lines()), idsWant(lines); !slices.Equal(got, want) {
		t.Fatalf("the stub got %d lines, want the %d queued in order", len(got), len(want))
	}
	if took > 2*time.Second {
		t.Fatalf("Close took %v", took)
	}
	stop := log.stopLine(t, "demo")
	if number(t, stop, "queued") != 100 || number(t, stop, "sent") != 100 || number(t, stop, "failed") != 0 ||
		number(t, stop, "attempts") != 3 || stop["level"] != "INFO" {
		t.Fatalf("stop line %v", stop)
	}
	goroutinesBack(t, before)
}

// Close with a target that never answers returns after 2 s plus a margin,
// counts the batch in flight as failed and leaves no goroutine behind.
func TestCloseGivesUpAfterTwoSeconds(t *testing.T) {
	stub := newSilentStub(t)
	before := runtime.NumGoroutine()
	var set setting
	set.put(t, map[string]any{"name": "silent", "kind": "http-json", "address": "http://" + stub.addr(),
		"batch_lines": 10, "timeout_ms": 5000})
	var log testLog
	m := newManager(options(&set, &log), realClock{})
	shipAll(t, m, testLines("server", 0, 10))
	waitFor(t, func() bool { return stub.count() == 1 }, "the attempt")
	start := time.Now()
	m.Close()
	if took := time.Since(start); took < 1900*time.Millisecond || took > 3*time.Second {
		t.Fatalf("Close took %v, want 2 s plus at most 1 s", took)
	}
	stop := log.stopLine(t, "silent")
	if number(t, stop, "failed") != 10 || number(t, stop, "sent") != 0 || number(t, stop, "attempts") != 1 {
		t.Fatalf("stop line %v", stop)
	}
	m.Ship(testLine("server", 99))
	goroutinesBack(t, before)
}

// The manager reads the setting every 5 s of its clock: a new row starts a
// sender, a changed row or a changed secret restarts it, a removed row stops
// it with its stop line.
func TestReloadFollowsTheSetting(t *testing.T) {
	a, b, c := newWebStub(t, nil), newWebStub(t, nil), newWebStub(t, nil)
	clk := newFakeClock()
	var set setting
	rowA := map[string]any{"name": "a", "kind": "http-json", "address": a.url(), "batch_lines": 1}
	rowB := map[string]any{"name": "b", "kind": "http-json", "address": b.url(), "batch_lines": 1, "secret": "log-target-b"}
	set.put(t, rowA)
	var log testLog
	m := startManager(t, clk, &set, &log)
	reload := func(names string) {
		t.Helper()
		clk.waitPending(t, 5*time.Second)
		clk.Advance(5 * time.Second)
		waitFor(t, func() bool { return namesOf(m) == names }, "the targets "+names)
	}
	stops := func(name string) int {
		n := 0
		for _, rec := range log.records(t, "log target stopped") {
			if rec["target"] == name {
				n++
			}
		}
		return n
	}
	m.Ship(testLine("server", 1))
	waitFor(t, func() bool { return len(a.lines()) == 1 }, "a line at a")

	set.putSecret("log-target-b", "token-one")
	set.put(t, rowA, rowB)
	reload("a,b")
	m.Ship(testLine("server", 2))
	waitFor(t, func() bool { return len(a.lines()) == 2 && len(b.lines()) == 1 }, "the line at a and b")

	rowA["address"] = c.url()
	set.put(t, rowA, rowB)
	clk.waitPending(t, 5*time.Second)
	clk.Advance(5 * time.Second)
	waitFor(t, func() bool { return stops("a") == 1 }, "the old a stopped")
	m.Ship(testLine("server", 3))
	waitFor(t, func() bool { return len(c.lines()) == 1 && len(b.lines()) == 2 }, "the line at c and b")
	if n := len(a.lines()); n != 2 {
		t.Fatalf("the old address of a got %d lines", n)
	}

	set.putSecret("log-target-b", "token-two")
	clk.waitPending(t, 5*time.Second)
	clk.Advance(5 * time.Second)
	waitFor(t, func() bool { return stops("b") == 1 }, "b restarted for its new secret")
	if st := statsOf(t, m, "b"); st.Queued != 0 {
		t.Fatalf("the new sender of b counts %+v", st)
	}
	m.Ship(testLine("server", 4))
	waitFor(t, func() bool { return len(b.lines()) == 3 }, "the line at b with the new secret")
	var auths []string
	for _, r := range b.requests() {
		auths = append(auths, r.header.Get("Authorization"))
	}
	if !slices.Equal(auths, []string{"Bearer token-one", "Bearer token-one", "Bearer token-two"}) {
		t.Fatalf("b got the secrets %q", auths)
	}

	set.put(t, rowB)
	reload("b")
	waitFor(t, func() bool { return stops("a") == 2 }, "a stopped")
	if n := stops("b"); n != 1 {
		t.Fatalf("b stopped %d times", n)
	}
}

// A row the manager cannot validate, and a row whose secret file cannot be
// read, are left out with one warning each, however often the setting is
// read; the other targets run.
func TestBadRowsAreSkippedWithOneWarning(t *testing.T) {
	stub := newWebStub(t, nil)
	clk := newFakeClock()
	var set setting
	set.put(t,
		map[string]any{"name": "good", "kind": "http-json", "address": stub.url()},
		map[string]any{"name": "colour", "kind": "http-json", "address": stub.url(), "colour": "red"},
		map[string]any{"name": "locked", "kind": "http-json", "address": stub.url(), "secret": "log-target-locked"})
	var log testLog
	m := startManager(t, clk, &set, &log)
	for i := 0; i < 3; i++ {
		clk.waitPending(t, 5*time.Second)
		clk.Advance(5 * time.Second)
	}
	clk.waitPending(t, 5*time.Second)
	if names := namesOf(m); names != "good" {
		t.Fatalf("running %q", names)
	}
	skipped := log.records(t, "log target skipped")
	if len(skipped) != 1 || !strings.Contains(skipped[0]["error"].(string), "colour") {
		t.Fatalf("warnings %v", skipped)
	}
	left := log.records(t, "log target left out")
	if len(left) != 1 || left[0]["target"] != "locked" || !strings.Contains(left[0]["error"].(string), "secrets/log-target-locked") {
		t.Fatalf("warnings %v", left)
	}
	set.putSecret("log-target-locked", "token")
	clk.Advance(5 * time.Second)
	waitFor(t, func() bool { return namesOf(m) == "good,locked" }, "the target with its secret")
}
