package logship

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// A target that accepts the connection and never answers costs the sender
// one timeout per attempt, the tap returns at once all the while, and after
// 3 attempts the batch counts as failed.
func TestSilentTargetCostsAtMostItsTimeout(t *testing.T) {
	stub := newSilentStub(t)
	clk := newFakeClock()
	const timeout = 500 * time.Millisecond
	var set setting
	set.put(t, map[string]any{"name": "silent", "kind": "http-json", "address": "http://" + stub.addr() + "/ingest",
		"batch_lines": 5, "queue_lines": 5, "timeout_ms": 500})
	var log testLog
	m := startManager(t, clk, &set, &log)
	shipAll(t, m, testLines("server", 0, 5))
	waitFor(t, func() bool { return stub.count() == 1 }, "the first attempt")
	var slowest time.Duration
	for i := 0; i < 200; i++ {
		start := time.Now()
		m.Ship(testLine("server", 100+i))
		slowest = max(slowest, time.Since(start))
	}
	if slowest > timeout/2 {
		t.Fatalf("one call of the tap took %v while an attempt waited", slowest)
	}
	clk.waitPending(t, 500*time.Millisecond)
	clk.Advance(500 * time.Millisecond)
	waitFor(t, func() bool { return stub.count() == 2 }, "the second attempt")
	clk.waitPending(t, 2*time.Second)
	clk.Advance(2 * time.Second)
	waitFor(t, func() bool { return statsOf(t, m, "silent").Failed >= 5 }, "the batch given up")
	if st := statsOf(t, m, "silent"); st.Failed != 5 || st.Sent != 0 || st.Errors < 3 {
		t.Fatalf("after 3 attempts: %+v", st)
	}
	waitFor(t, func() bool { return len(stub.spans()) >= 3 }, "the stub to see 3 connections closed")
	spans := stub.spans()
	for i, span := range spans[:3] {
		if span < timeout-100*time.Millisecond || span > timeout+750*time.Millisecond {
			t.Fatalf("attempt %d held the connection %v, the timeout is %v", i+1, span, timeout)
		}
	}
	m.Close()
	stop := log.stopLine(t, "silent")
	if number(t, stop, "queued") != 10 || number(t, stop, "failed") != 10 || number(t, stop, "sent") != 0 ||
		number(t, stop, "dropped") != 195 || number(t, stop, "attempts") < 4 {
		t.Fatalf("stop line %v", stop)
	}
}

// A 503 and then a 200 send the batch once more and count it as sent.
func TestRetryAfterAServiceUnavailable(t *testing.T) {
	stub := newWebStub(t, statuses(503, 200))
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 3})
	var log testLog
	m := startManager(t, clk, &set, &log)
	shipAll(t, m, testLines("server", 0, 3))
	clk.waitPending(t, 500*time.Millisecond)
	clk.Advance(500 * time.Millisecond)
	waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 3 }, "the batch sent")
	st := statsOf(t, m, "demo")
	got := stub.requests()
	if st.Attempts != 2 || st.Errors != 1 || st.Failed != 0 || len(got) != 2 || !bytes.Equal(got[0].body, got[1].body) {
		t.Fatalf("%+v after %d requests", st, len(got))
	}
}

// A 400 drops the batch at once, without a pause or a second attempt.
func TestBadRequestDropsAtOnce(t *testing.T) {
	stub := newWebStub(t, statuses(400))
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 3})
	var log testLog
	m := startManager(t, clk, &set, &log)
	shipAll(t, m, testLines("server", 0, 3))
	waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 3 }, "the batch dropped")
	if st := statsOf(t, m, "demo"); st.Attempts != 1 || st.Errors != 1 || st.Sent != 0 {
		t.Fatalf("%+v", st)
	}
	if p := clk.pending(); slices.Contains(p, 500*time.Millisecond) || slices.Contains(p, 2*time.Second) {
		t.Fatalf("a pause after a final answer: %v", p)
	}
	if n := len(stub.requests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

// Retry-After: 1 makes the pause 1 s of the clock, in place of 500 ms.
func TestRetryAfterIsHonoured(t *testing.T) {
	stub := newWebStub(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
	var log testLog
	m := startManager(t, clk, &set, &log)
	shipAll(t, m, testLines("server", 0, 1))
	clk.waitPending(t, time.Second)
	if slices.Contains(clk.pending(), 500*time.Millisecond) {
		t.Fatalf("the pause of 500 ms is still asked for: %v", clk.pending())
	}
	clk.Advance(999 * time.Millisecond)
	clk.waitPending(t, time.Millisecond)
	if n := len(stub.requests()); n != 1 {
		t.Fatalf("%d requests before Retry-After passed", n)
	}
	clk.Advance(time.Millisecond)
	waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent after Retry-After")
}

// A Retry-After above 30 s waits 30 s; an HTTP date waits until that date.
func TestRetryAfterIsCapped(t *testing.T) {
	clk := newFakeClock()
	stub := newWebStub(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		switch n {
		case 1:
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.Header().Set("Retry-After", clk.Now().Add(3*time.Second).Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
	var log testLog
	m := startManager(t, clk, &set, &log)
	shipAll(t, m, testLines("server", 0, 1))
	clk.waitPending(t, 30*time.Second)
	clk.Advance(30 * time.Second)
	waitFor(t, func() bool { return len(stub.requests()) == 2 }, "the second attempt")
	clk.waitPending(t, 3*time.Second)
	clk.Advance(3 * time.Second)
	waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent after the date")
}

// A redirect is a failed attempt: it is not followed, and the stub it points
// to sees no request.
func TestRedirectIsAFailure(t *testing.T) {
	other := newWebStub(t, nil)
	stub := newWebStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.url()+"/elsewhere", http.StatusFound)
	})
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
	var log testLog
	m := startManager(t, clk, &set, &log)
	shipAll(t, m, testLines("server", 0, 1))
	waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line dropped")
	if st := statsOf(t, m, "demo"); st.Attempts != 1 || st.Sent != 0 {
		t.Fatalf("%+v", st)
	}
	if n := len(other.requests()); n != 0 {
		t.Fatalf("the redirect was followed: %d requests", n)
	}
	waitFor(t, func() bool { return len(log.records(t, "log target failing")) > 0 }, "the warning")
	warns := log.records(t, "log target failing")
	if len(warns) != 1 || warns[0]["error"] != "status 302" || warns[0]["target"] != "demo" {
		t.Fatalf("warnings %v", warns)
	}
}

// An answer body of 10 MiB is read up to the bound of 64 KiB: the stub
// cannot write the whole body, and the batch counts as sent.
func TestAnswerBodyIsBounded(t *testing.T) {
	var written atomic.Int64
	ended := make(chan error, 1)
	stub := newWebStub(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		var err error
		for total := 0; total < 10<<20 && err == nil; total += len(chunk) {
			var n int
			n, err = w.Write(chunk)
			written.Add(int64(n))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		ended <- err
	})
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
	var log testLog
	m := startManager(t, clk, &set, &log)
	shipAll(t, m, testLines("server", 0, 1))
	waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent")
	select {
	case err := <-ended:
		if err == nil || written.Load() >= 10<<20 {
			t.Fatalf("the stub wrote %d bytes of 10 MiB, error %v", written.Load(), err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stub still writes its answer")
	}
}

// The lines of the logger pipesmith_registry.logship are never queued, also
// when the manager logs through the log files whose tap feeds it: a target
// that refuses every line warns into server.log and gets no line of its own
// warnings.
func TestOwnLinesAreNeverQueued(t *testing.T) {
	stub := newWebStub(t, statuses(400))
	clk := newFakeClock()
	dir := filepath.Join(t.TempDir(), "logs")
	var shipTo atomic.Pointer[Manager]
	files := logs.Open(logs.Options{Dir: dir, Settings: logs.DefaultSettings(), Ship: func(l logs.Line) {
		if m := shipTo.Load(); m != nil {
			m.Ship(l)
		}
	}})
	t.Cleanup(func() { _ = files.Close() })
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
	m := newManager(Options{Rows: set.rows, Secret: set.secret, Logger: slog.New(files.Handler()), Version: testVersion,
		Host: "demo-host"}, clk)
	t.Cleanup(m.Close)
	shipTo.Store(m)
	own := logs.Line{Kind: "server", Logger: Logger, Message: "log target failing", JSON: []byte(`{"logger":"pipesmith_registry.logship"}`)}
	m.Ship(own)
	if st := statsOf(t, m, "demo"); st.Queued != 0 {
		t.Fatalf("a line of the own logger was queued: %+v", st)
	}
	for i := 0; i < 3; i++ {
		m.Ship(testLine("server", i))
		waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == int64(i+1) }, "the line refused")
		clk.Advance(time.Minute)
	}
	if st := statsOf(t, m, "demo"); st.Queued != 3 {
		t.Fatalf("queued %d, want the 3 lines of the test", st.Queued)
	}
	m.Close()
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	for _, line := range stub.lines() {
		if strings.Contains(line, Logger) {
			t.Fatalf("the target got a line of its own logger: %s", line)
		}
	}
	server, err := os.ReadFile(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(server), `"logger":"pipesmith_registry.logship","msg":"log target failing"`); n != 3 {
		t.Fatalf("%d warnings in server.log:\n%s", n, server)
	}
}

// A failing target logs at most one WARN per minute of the clock, each with
// the counts since the last one.
func TestWarnAtMostOncePerMinute(t *testing.T) {
	stub := newWebStub(t, statuses(400))
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
	var log testLog
	m := startManager(t, clk, &set, &log)
	for i := 0; i < 18; i++ { // a failure every 10 s, from 0 s to 170 s
		m.Ship(testLine("server", i))
		waitFor(t, func() bool { return statsOf(t, m, "demo").Errors == int64(i+1) }, "the failure")
		clk.Advance(10 * time.Second)
	}
	warns := log.records(t, "log target failing")
	var errs, failed []int64
	for _, w := range warns {
		errs, failed = append(errs, number(t, w, "errors")), append(failed, number(t, w, "failed"))
	}
	if want := []int64{1, 6, 6}; !slices.Equal(errs, want) || !slices.Equal(failed, want) {
		t.Fatalf("warnings with errors %v and failed %v, want %v each", errs, failed, want)
	}
	for _, w := range warns {
		if w["level"] != "WARN" || w["target"] != "demo" || w["error"] != "status 400" {
			t.Fatalf("warning %v", w)
		}
	}
}

// A target for requests only gets no server, audit or task line.
func TestTargetTakesOnlyItsFiles(t *testing.T) {
	stub := newWebStub(t, nil)
	clk := newFakeClock()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1,
		"files": []string{"requests"}})
	var log testLog
	m := startManager(t, clk, &set, &log)
	for _, kind := range []string{"server", "requests", "audit", "tasks"} {
		m.Ship(testLine(kind, 1))
	}
	waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the request line sent")
	if st := statsOf(t, m, "demo"); st.Queued != 1 {
		t.Fatalf("%+v", st)
	}
	if got := idsOf(t, stub.lines()); !slices.Equal(got, []string{"requests-0001"}) {
		t.Fatalf("the stub got %v", got)
	}
}
