package logs

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var activeNames = []string{"server.log", "requests.log", "audit.log", "tasks.log", "requests-classic.log"}

// A log directory below a regular file cannot be made, which a mode bit
// would not show when the tests run as root. Open returns usable Files,
// every line counts as an error, the audit writer returns the failure, and
// the check is red: it names each file and the error of the system, never a
// line. The open is tried again at most every 5 s of the clock: once the
// directory can be made and 5 s passed, the next line lands in its file and
// the check is green.
func TestOpenFailureIsRetried(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("a regular file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(file, "logs")
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clk.now})
	t.Cleanup(func() { _ = f.Close() })
	check := f.Health()
	if check.Name != "log directory" || check.Healthy || !strings.Contains(check.Detail, "not a directory") {
		t.Fatalf("after Open: %+v", check)
	}
	for _, name := range activeNames {
		if !strings.Contains(check.Detail, name+": ") {
			t.Errorf("the detail does not name %s: %q", name, check.Detail)
		}
	}
	const canary = "canary-of-a-line-7f3a"
	log := slog.New(f.Handler())
	log.Warn(canary)
	f.Request(demoRequest("/" + canary))
	audit := demoAudit("ok")
	audit.ObjectName = canary
	if err := f.Audit(audit); err == nil {
		t.Fatal("the audit writer did not report the failure")
	}
	if err := f.Task(demoTask("start")); err != nil {
		t.Fatal(err)
	}
	f.Tick()
	if got := f.Errors(); got != 5 {
		t.Fatalf("errors %d, want 5: a warning, two request lines, an audit and a task line", got)
	}
	if check := f.Health(); check.Healthy || strings.Contains(check.Detail, canary) {
		t.Fatalf("after the lines: %+v", check)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o750); err != nil {
		t.Fatal(err)
	}
	clk.set(time.Date(2026, 11, 19, 10, 0, 4, 999_999_999, time.UTC))
	log.Warn("too early")
	f.Tick()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) || f.Errors() != 6 || f.Health().Healthy {
		t.Fatalf("tried again before 5 s: %v, %d errors, %+v", err, f.Errors(), f.Health())
	}
	clk.set(time.Date(2026, 11, 19, 10, 0, 5, 0, time.UTC))
	log.Warn("on time")
	if got := lines(t, dir, "server.log"); len(got) != 1 || !strings.Contains(got[0], "on time") {
		t.Fatalf("server.log after 5 s: %q", got)
	}
	f.Request(demoRequest("/after"))
	f.Tick()
	if check := f.Health(); !check.Healthy || check.Detail != "" {
		t.Fatalf("after the next success: %+v", check)
	}
	if err := f.Audit(demoAudit("denied")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"requests.log": "/after", "requests-classic.log": "/after", "audit.log": "denied"} {
		if got := lines(t, dir, name); len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%s: %q", name, got)
		}
	}
	if f.Errors() != 6 {
		t.Fatalf("errors %d, want 6", f.Errors())
	}
}

// A write that fails, here to a file closed under the writer: its lines
// count as errors, Request and Tick return as always, the audit writer
// returns the error, and the check is red and names the file and the error
// of the system but no line. 5 s of the clock later the file is opened again
// and the next line lands.
func TestWriteFailureIsRetried(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clk.now})
	t.Cleanup(func() { _ = f.Close() })
	if check := f.Health(); !check.Healthy || check.Name != "log directory" {
		t.Fatalf("after Open: %+v", check)
	}
	for _, lf := range []*logFile{f.requests, f.audit} {
		lf.disk.Lock()
		_ = lf.f.Close()
		lf.disk.Unlock()
	}
	const canary = "canary-of-a-failed-write-91c2"
	f.Request(demoRequest("/" + canary))
	f.Tick()
	if f.Errors() != 1 {
		t.Fatalf("errors %d, want 1 for the line of requests.log", f.Errors())
	}
	audit := demoAudit("ok")
	audit.ObjectName = canary
	if err := f.Audit(audit); err == nil {
		t.Fatal("the audit writer did not report the failure")
	}
	check := f.Health()
	if check.Healthy || !strings.Contains(check.Detail, "requests.log: ") || !strings.Contains(check.Detail, "audit.log: ") ||
		!strings.Contains(check.Detail, os.ErrClosed.Error()) || strings.Contains(check.Detail, canary) {
		t.Fatalf("after the failed writes: %+v", check)
	}
	slog.New(f.Handler()).Warn("server.log still works")
	if got := lines(t, dir, "server.log"); len(got) != 1 {
		t.Fatalf("server.log: %q", got)
	}
	clk.set(time.Date(2026, 11, 19, 10, 0, 5, 0, time.UTC))
	f.Request(demoRequest("/after"))
	f.Tick()
	if err := f.Audit(demoAudit("failed")); err != nil {
		t.Fatal(err)
	}
	if check := f.Health(); !check.Healthy {
		t.Fatalf("after the next success: %+v", check)
	}
	for name, want := range map[string]string{"requests.log": "/after", "audit.log": `"failed"`} {
		if got := lines(t, dir, name); len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := lines(t, dir, "requests-classic.log"); len(got) != 2 {
		t.Errorf("requests-classic.log: %q", got)
	}
	if f.Errors() != 2 {
		t.Fatalf("errors %d, want 2", f.Errors())
	}
}
