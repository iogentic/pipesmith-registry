package logs

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Finding 7b-1: the audit line of a command that runs beside a server waits
// as one file in audit-pending; the server moves it into audit.log and to
// its tap on its next Tick, and a server that starts takes the waiting
// lines first.

// auditActions are the actions of audit.log in dir, in their order.
func auditActions(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		var line struct{ Action string }
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, line.Action)
	}
	return out
}

func pendingFiles(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, pendingName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return entries
}

func commandEvent(action string) AuditEvent {
	return AuditEvent{Subject: "cli", Action: action, ObjectType: "store", ObjectName: "demo-raw", Result: "ok", Fields: []string{}}
}

func TestPendingLineReachesAuditLogOnTick(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	var mu sync.Mutex
	var shipped []Line
	server := Open(Options{Dir: dir, Ship: func(l Line) {
		mu.Lock()
		shipped = append(shipped, l)
		mu.Unlock()
	}})
	t.Cleanup(func() { _ = server.Close() })
	if err := AppendAudit(Options{Dir: dir}, commandEvent("store.create")); err != nil {
		t.Fatalf("append beside the server: %v", err)
	}
	if got := auditActions(t, dir); len(got) != 0 {
		t.Fatalf("audit.log before the tick: %v", got)
	}
	info, err := os.Stat(filepath.Join(dir, pendingName))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("pending folder: %v %v", info, err)
	}
	files := pendingFiles(t, dir)
	if len(files) != 1 || !pendingPattern.MatchString(files[0].Name()) {
		t.Fatalf("pending files %v", files)
	}
	if fi, err := files[0].Info(); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pending file mode: %v %v", fi, err)
	}
	server.Tick()
	if got := auditActions(t, dir); len(got) != 1 || got[0] != "store.create" {
		t.Fatalf("audit.log after the tick: %v", got)
	}
	if files := pendingFiles(t, dir); len(files) != 0 {
		t.Fatalf("pending files after the tick: %v", files)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(shipped) != 1 || shipped[0].Kind != kindAudit || !strings.Contains(string(shipped[0].JSON), `"action":"store.create"`) {
		t.Fatalf("shipped %+v", shipped)
	}
}

// Lines are taken in the order of their names, which start with the time
// of the command.
func TestPendingLinesInNameOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	server := Open(Options{Dir: dir})
	t.Cleanup(func() { _ = server.Close() })
	base := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	later := Options{Dir: dir, Now: func() time.Time { return base.Add(time.Second) }}
	earlier := Options{Dir: dir, Now: func() time.Time { return base }}
	if err := AppendAudit(later, commandEvent("store.delete")); err != nil {
		t.Fatal(err)
	}
	if err := AppendAudit(earlier, commandEvent("store.create")); err != nil {
		t.Fatal(err)
	}
	server.Tick()
	if got := strings.Join(auditActions(t, dir), " "); got != "store.create store.delete" {
		t.Fatalf("order %q", got)
	}
}

// A command that ends between its write and its rename leaves a temporary
// file: the server never takes it, so audit.log holds no half line, and it
// is removed once an hour passed. A file under a pending name that holds no
// audit line is removed and counted.
func TestPendingCrashLeavesNoHalfLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	server := Open(Options{Dir: dir, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	t.Cleanup(func() { _ = server.Close() })
	folder := filepath.Join(dir, pendingName)
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	half := filepath.Join(folder, ".tmp-123")
	if err := os.WriteFile(half, []byte(`{"time":"2026-11-20T08:00:00.000Z","level":"INFO","logger":"pipesmith_registry.audit","msg":"store.cr`), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(folder, "00000000000000000001-0000000001-000001.line")
	if err := os.WriteFile(foreign, []byte(`{"time":"2026-11-20T08:00:00.000Z","level":"INFO","logger":"pipesmith_registry","msg":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.Tick()
	if data, _ := os.ReadFile(filepath.Join(dir, "audit.log")); len(data) != 0 {
		t.Fatalf("audit.log holds %q", data)
	}
	if _, err := os.Stat(half); err != nil {
		t.Fatalf("the young temporary file is gone: %v", err)
	}
	if _, err := os.Stat(foreign); !os.IsNotExist(err) {
		t.Fatalf("the foreign line stays: %v", err)
	}
	if server.Errors() != 1 {
		t.Fatalf("errors %d", server.Errors())
	}
	clock.Add(int64(2 * time.Hour))
	server.Tick()
	if _, err := os.Stat(half); !os.IsNotExist(err) {
		t.Fatalf("the old temporary file stays: %v", err)
	}
}

// A server that starts takes the waiting lines before any line of its own;
// so does a command that finds no server.
func TestPendingTakenAtStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	first := Open(Options{Dir: dir})
	if err := AppendAudit(Options{Dir: dir}, commandEvent("store.create")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil { // stopped before its next tick
		t.Fatal(err)
	}
	if err := AppendAudit(Options{Dir: dir}, commandEvent("store.delete")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(auditActions(t, dir), " "); got != "store.create store.delete" {
		t.Fatalf("after the command without a server: %q", got)
	}
	if files := pendingFiles(t, dir); len(files) != 0 {
		t.Fatalf("pending files %v", files)
	}
}
