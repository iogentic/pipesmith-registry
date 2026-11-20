package logship

import (
	"slices"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// A queue of 10 lines that gets 25 lines while its sender waits for the
// flush interval keeps the first 10 and drops the other 15 (L7); the 10
// arrive in their order once the sender runs. A tap that blocks instead of
// dropping fails by the deadline of shipAll.
func TestFullQueueDropsTheNewLine(t *testing.T) {
	stub := newWebStub(t, nil)
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "queue_lines": 10,
		"batch_lines": 500, "flush_interval_ms": 60000})
	var log testLog
	m := startManager(t, clk, &set, &log)
	lines := testLines("server", 0, 25)
	shipAll(t, m, lines)
	if st := statsOf(t, m, "demo"); st.Queued != 10 || st.Dropped != 15 || st.Sent != 0 {
		t.Fatalf("queued %d, dropped %d, sent %d; want 10, 15, 0", st.Queued, st.Dropped, st.Sent)
	}
	clk.waitPending(t, time.Minute)
	if n := len(stub.requests()); n != 0 {
		t.Fatalf("%d requests before the flush interval passed", n)
	}
	clk.Advance(time.Minute)
	waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 10 }, "10 lines sent")
	if got, want := idsOf(t, stub.lines()), idsWant(lines[:10]); !slices.Equal(got, want) {
		t.Fatalf("the stub got %v, want %v", got, want)
	}
}

// The byte bound drops as well: a queue of 64 KiB takes as many lines of
// 10 kB as fit and drops the rest.
func TestFullQueueDropsByBytes(t *testing.T) {
	stub := newWebStub(t, nil)
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "queue_bytes": 65536,
		"batch_lines": 500, "flush_interval_ms": 60000})
	var log testLog
	m := startManager(t, clk, &set, &log)
	lines := make([]logs.Line, 25)
	for i := range lines {
		lines[i] = paddedLine("requests", i, 10_000)
	}
	fit := 65536 / len(lines[0].JSON)
	if fit < 2 || fit >= len(lines) {
		t.Fatalf("%d lines of %d bytes fit", fit, len(lines[0].JSON))
	}
	shipAll(t, m, lines)
	if st := statsOf(t, m, "demo"); st.Queued != int64(fit) || st.Dropped != int64(len(lines)-fit) {
		t.Fatalf("queued %d, dropped %d; want %d, %d", st.Queued, st.Dropped, fit, len(lines)-fit)
	}
	clk.waitPending(t, time.Minute)
	clk.Advance(time.Minute)
	waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == int64(fit) }, "the lines that fit sent")
	if got, want := idsOf(t, stub.lines()), idsWant(lines[:fit]); !slices.Equal(got, want) {
		t.Fatalf("the stub got %v, want %v", got, want)
	}
}
