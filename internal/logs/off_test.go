package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// entriesOf lists dir; the test fails when it cannot be read.
func entriesOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// With Options.Dir empty the files are off: every JSON line of a mixed run,
// server records at four levels, request, audit and task lines, reaches the
// tap once, in the order it was written. Open makes no directory and opens no
// file, so the folder of the test, the working directory meanwhile, holds no
// new entry; Errors stays 0 and the check green; nothing rotates at the size
// limit or the day change, no retention runs, and the classic line is not
// made.
func TestFilesOffShipEveryLine(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	clk := newFakeClock(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC))
	var got tapped
	f := Open(Options{Settings: small(), Now: clk.now, Ship: got.ship})
	f.Start()
	log := slog.New(f.Handler())
	type written struct {
		kind  string
		level slog.Level
		msg   string
	}
	var want []written
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for round := 0; round < 3; round++ {
		for _, level := range levels {
			msg := fmt.Sprintf("round %d %s", round, level)
			log.Log(context.Background(), level, msg, "logger", "pipesmith_registry.demo", "pad", strings.Repeat("x", 600))
			want = append(want, written{kindServer, level, msg})
		}
		path := fmt.Sprintf("/repository/demo-releases/%d.jar", round)
		f.Request(demoRequest(path))
		want = append(want, written{kindRequests, slog.LevelInfo, "GET " + path + " 200"})
		if err := f.Audit(demoAudit("ok")); err != nil {
			t.Fatal(err)
		}
		want = append(want, written{kindAudit, slog.LevelInfo, "repository.create ok"})
		if err := f.Task(demoTask("step")); err != nil {
			t.Fatal(err)
		}
		want = append(want, written{kindTasks, slog.LevelInfo, "demo.scan step"})
		f.Tick()
		clk.set(clk.now().Add(time.Hour)) // the day changes after the first round, the hour after each
	}
	if check := f.Health(); !check.Healthy || check.Detail != "" {
		t.Errorf("check %+v", check)
	}
	if err := f.Close(); err != nil || f.Errors() != 0 {
		t.Fatalf("close %v, %d errors", err, f.Errors())
	}
	shipped := got.all()
	if len(shipped) != len(want) {
		t.Fatalf("%d lines shipped, %d written", len(shipped), len(want))
	}
	for i, l := range shipped {
		var fields head
		if err := json.Unmarshal(l.JSON, &fields); err != nil {
			t.Fatalf("line %d does not parse: %v: %q", i, err, l.JSON)
		}
		if w := want[i]; l.Kind != w.kind || l.Level != w.level || l.Message != w.msg || fields.Msg != w.msg {
			t.Errorf("line %d: %s %v %q, written as %s %v %q", i, l.Kind, l.Level, l.Message, w.kind, w.level, w.msg)
		}
	}
	if names := entriesOf(t, work); len(names) != 0 {
		t.Errorf("the working directory holds %v", names)
	}
	if f.classic != nil || f.maint.done != nil {
		t.Errorf("classic writer %v, maintenance started %v", f.classic != nil, f.maint.done != nil)
	}
}

// With Options.Dir empty and no tap, Open and every writer are no-ops: no
// call allocates, no writer holds a buffer, Errors stays 0, the check stays
// green and the working directory holds no new entry.
func TestFilesOffWithoutTapCostNothing(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	f := Open(Options{Settings: DefaultSettings(), Now: clock})
	f.Start()
	h := f.Handler()
	ctx := context.Background()
	request, audit, task := demoRequest("/repository/demo-releases/a.jar"), demoAudit("ok"), demoTask("end")
	debug := slog.NewRecord(clock(), slog.LevelDebug, "debug line", 0)
	warn := slog.NewRecord(clock(), slog.LevelWarn, "warn line", 0)
	writers := []struct {
		name  string
		write func()
	}{
		{"request", func() { f.Request(request) }},
		{"audit", func() { _ = f.Audit(audit) }},
		{"task", func() { _ = f.Task(task) }},
		{"server line below WARN", func() { _ = h.Handle(ctx, debug) }},
		{"server line from WARN", func() { _ = h.Handle(ctx, warn) }},
		{"tick", f.Tick},
	}
	for _, w := range writers {
		if allocs := testing.AllocsPerRun(1000, w.write); allocs != 0 {
			t.Errorf("%s: %v allocations per call", w.name, allocs)
		}
	}
	for _, lf := range f.all() {
		lf.mu.Lock()
		held := cap(lf.buf)
		lf.mu.Unlock()
		if held != 0 {
			t.Errorf("%s holds a buffer of %d bytes", lf.kind, held)
		}
	}
	if err := f.Close(); err != nil || f.Errors() != 0 {
		t.Fatalf("close %v, %d errors", err, f.Errors())
	}
	if check := f.Health(); !check.Healthy {
		t.Errorf("check %+v", check)
	}
	if names := entriesOf(t, work); len(names) != 0 {
		t.Errorf("the working directory holds %v", names)
	}
}
