package logship

import (
	"runtime"
	"testing"
	"time"
)

// The watcher of a tcp or tls link ends with the link: when the receiver
// ended the connection after 10 messages and the next batch dialed again,
// and when the manager closes the second connection (finding 7a-5).
func TestStreamLinksLeaveNoGoroutine(t *testing.T) {
	for _, scheme := range []string{"tcp", "tls"} {
		t.Run(scheme, func(t *testing.T) {
			stub := newSyslogStub(t, scheme, nil)
			stub.setCloseAfter(10)
			before := runtime.NumGoroutine()
			row := map[string]any{"name": "demo", "kind": "syslog", "address": stub.address(), "batch_lines": 5}
			if scheme == "tls" {
				row["ca_file"] = stub.caFile
			}
			var set setting
			set.put(t, row)
			var log testLog
			m := newManager(options(&set, &log), newFakeClock())
			shipAll(t, m, testLines("server", 0, 10))
			waitFor(t, func() bool { return len(stub.messages()) == 10 }, "the first 10 messages")
			waitFor(t, func() bool { _, closed := stub.connections(); return closed == 1 }, "the end of the first connection")
			shipAll(t, m, testLines("server", 10, 5))
			waitFor(t, func() bool { return len(stub.messages()) == 15 }, "15 messages")
			if accepted, _ := stub.connections(); accepted != 2 {
				t.Fatalf("%d connections, want 2", accepted)
			}
			if runtime.NumGoroutine() <= before {
				t.Fatal("no goroutine of the sender runs")
			}
			m.Close()
			goroutinesBack(t, before)
		})
	}
}

// A sender that a reload stops, for a removed row and for a changed one,
// leaves no goroutine: neither its own nor the watcher of its link.
func TestReloadLeavesNoGoroutine(t *testing.T) {
	a, b, c := newSyslogStub(t, "tcp", nil), newSyslogStub(t, "tcp", nil), newSyslogStub(t, "tcp", nil)
	clk := newFakeClock()
	var set setting
	rowB := map[string]any{"name": "b", "kind": "syslog", "address": b.address(), "batch_lines": 1}
	set.put(t, rowB)
	var log testLog
	m := startManager(t, clk, &set, &log)
	m.Ship(testLine("server", 1))
	waitFor(t, func() bool { return len(b.messages()) == 1 }, "the first line at b")
	before := runtime.NumGoroutine()
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
	rowA := map[string]any{"name": "a", "kind": "syslog", "address": a.address(), "batch_lines": 1}
	set.put(t, rowA, rowB)
	reload("a,b")
	m.Ship(testLine("server", 2))
	waitFor(t, func() bool { return len(a.messages()) == 1 && len(b.messages()) == 2 }, "the second line at a and b")
	if runtime.NumGoroutine() <= before {
		t.Fatal("no goroutine of the sender of a runs")
	}
	rowA["address"] = c.address()
	set.put(t, rowA, rowB)
	clk.waitPending(t, 5*time.Second)
	clk.Advance(5 * time.Second)
	waitFor(t, func() bool { return stops("a") == 1 }, "the old a stopped")
	m.Ship(testLine("server", 3))
	waitFor(t, func() bool { return len(c.messages()) == 1 && len(b.messages()) == 3 }, "the third line at c and b")
	set.put(t, rowB)
	reload("b")
	waitFor(t, func() bool { return stops("a") == 2 }, "a stopped")
	goroutinesBack(t, before)
}
