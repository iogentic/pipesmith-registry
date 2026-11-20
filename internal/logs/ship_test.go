package logs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// tapped collects the lines a tap gets.
type tapped struct {
	mu    sync.Mutex
	lines []Line
}

func (c *tapped) ship(l Line) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
}

func (c *tapped) all() []Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Line(nil), c.lines...)
}

// Every JSON line of the four files reaches the tap once, in the order it was
// made, with the kind and the fields of its head; JSON is the line of the file
// without its line break. The classic line is not shipped.
func TestShipGetsEveryJSONLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	var got tapped
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock, Ship: got.ship})
	log := slog.New(f.Handler())
	log.Debug("debug line", "logger", "pipesmith_registry.server", "path", "/a")
	log.Info("info line", "count", 3)
	log.Warn("warn \"line\"\n\x00", "logger", "pipesmith_registry.listeners")
	log.Error("error line", "logger", "pipesmith_registry.security")
	f.Request(demoRequest("/repository/demo-releases/a.jar"))
	if err := f.Audit(demoAudit("ok")); err != nil {
		t.Fatal(err)
	}
	if err := f.Task(demoTask("start")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if classic := lines(t, dir, "requests-classic.log"); len(classic) != 1 {
		t.Fatalf("requests-classic.log %q", classic)
	}
	var want []string
	kinds := []string{kindServer, kindServer, kindServer, kindServer, kindRequests, kindAudit, kindTasks}
	for _, kind := range []string{kindServer, kindRequests, kindAudit, kindTasks} {
		want = append(want, lines(t, dir, kind+".log")...)
	}
	shipped := got.all()
	if len(shipped) != len(want) || len(want) != len(kinds) {
		t.Fatalf("%d lines shipped, %d lines in the files, want %d", len(shipped), len(want), len(kinds))
	}
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError, slog.LevelInfo, slog.LevelInfo, slog.LevelInfo}
	for i, l := range shipped {
		if string(l.JSON) != want[i] || bytes.IndexByte(l.JSON, '\n') >= 0 {
			t.Fatalf("line %d: JSON %q, the file holds %q", i, l.JSON, want[i])
		}
		var fields head
		if err := json.Unmarshal(l.JSON, &fields); err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(timeLayout, fields.Time)
		if err != nil {
			t.Fatal(err)
		}
		if l.Kind != kinds[i] || !l.Time.Equal(at) || l.Level != levels[i] || l.Logger != fields.Logger || l.Message != fields.Msg {
			t.Fatalf("line %d: %+v, the line holds %+v", i, l, fields)
		}
	}
	if shipped[2].Message != "warn \"line\"\n\x00" || shipped[4].Logger != loggerRequests || !shipped[4].Time.Equal(clock().Truncate(time.Millisecond)) {
		t.Fatalf("message %q, logger %q, time %v", shipped[2].Message, shipped[4].Logger, shipped[4].Time)
	}
	if data := shipped[0].JSON; cap(data) != len(data) {
		t.Fatalf("JSON has room for %d more bytes: an append of the tap would write into the line", cap(data)-len(data))
	}
}

// The tap runs outside the locks of the writers: a tap that flushes every file
// does not wait for itself, on the path of lines that wait (a request, a task,
// a debug line) and on the path of lines written at once (audit, WARN).
func TestShipRunsOutsideTheWriterLocks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	var f *Files
	calls := 0
	f = Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock, Ship: func(Line) {
		calls++
		f.Tick()
	}})
	defer func() { _ = f.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		log := slog.New(f.Handler())
		log.Debug("debug line")
		log.Warn("warn line")
		f.Request(demoRequest("/repository/demo-releases/a.jar"))
		_ = f.Audit(demoAudit("ok"))
		_ = f.Task(demoTask("end"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a tap that flushes the files waits for the lock of a writer")
	}
	if calls != 5 {
		t.Fatalf("the tap ran %d times, want 5", calls)
	}
}

// Without a tap the call that would ship a line allocates nothing.
func TestShipWithoutTapCostsNothing(t *testing.T) {
	f := open(t, filepath.Join(t.TempDir(), "logs"))
	line := []byte(`{"time":"2026-11-19T10:00:00.123Z","level":"INFO","logger":"pipesmith_registry","msg":"m"}` + "\n")
	if allocs := testing.AllocsPerRun(100, func() { f.ship(kindServer, line) }); allocs != 0 {
		t.Fatalf("%v allocations per line without a tap", allocs)
	}
}
