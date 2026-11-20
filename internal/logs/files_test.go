package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// clock is the fake clock of a test.
var clock = func() time.Time { return time.Date(2026, 11, 19, 10, 0, 0, 123_456_789, time.UTC) }

func open(t *testing.T, dir string) *Files {
	t.Helper()
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// lines reads the lines of one file of dir; a missing file has none.
func lines(t *testing.T, dir, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("%s does not end with a line break", name)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// keysOf reads one line as a JSON object and returns its keys in their order
// and its values.
func keysOf(t *testing.T, line string) ([]string, map[string]any) {
	t.Helper()
	for i := 0; i < len(line); i++ {
		if line[i] < 0x20 {
			t.Fatalf("control byte %#x in %q", line[i], line)
		}
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %q", line)
	}
	var keys []string
	values := map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("%v: %q", err, line)
		}
		key := tok.(string)
		var value any
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("%v: %q", err, line)
		}
		keys = append(keys, key)
		values[key] = value
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		t.Fatalf("object not closed: %q", line)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("more after the object: %q", line)
	}
	return keys, values
}

var (
	lineTime   = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	common     = []string{"time", "level", "logger", "msg"}
	requestOf  = []string{"request_id", "client", "subject", "realm", "method", "path", "query", "protocol", "status", "request_bytes", "response_bytes", "duration_ms", "user_agent", "repository", "format", "port"}
	auditOf    = []string{"request_id", "subject", "realm", "client", "action", "object_type", "object_name", "result", "fields"}
	taskOf     = []string{"run_id", "task_id", "task_type", "event", "result", "counts", "duration_ms"}
	levelNames = map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
)

// checkLine checks the common fields of a line and that its other keys are
// fields, in this order; it returns the values.
func checkLine(t *testing.T, line, logger string, fields []string) map[string]any {
	t.Helper()
	keys, values := keysOf(t, line)
	if !reflect.DeepEqual(keys, append(append([]string(nil), common...), fields...)) {
		t.Fatalf("keys %v, want %v and %v: %q", keys, common, fields, line)
	}
	if s, _ := values["time"].(string); !lineTime.MatchString(s) {
		t.Errorf("time %v", values["time"])
	}
	if s, _ := values["level"].(string); !levelNames[s] {
		t.Errorf("level %v", values["level"])
	}
	if values["logger"] != logger {
		t.Errorf("logger %v, want %s", values["logger"], logger)
	}
	return values
}

func demoRequest(path string) Request {
	return Request{ID: "0123456789abcdef", Time: clock(), Client: "192.0.2.7", Subject: "demo-reader", Realm: "demo-realm",
		Method: "GET", Path: path, Query: "a=1", Protocol: "HTTP/1.1", Status: 200, ContentLength: "3", RequestBytes: 3,
		ResponseBytes: 2048, Duration: 1234567 * time.Nanosecond, UserAgent: "demo-agent/1.0", Repository: "demo-releases",
		Format: "maven2", Port: 14100}
}

func demoAudit(result string) AuditEvent {
	return AuditEvent{RequestID: "0123456789abcdef", Subject: "demo-admin", Realm: "demo-realm", Client: "192.0.2.8",
		Action: "repository.create", ObjectType: "repository", ObjectName: "demo-raw", Result: result, Fields: []string{"name", "format"}}
}

func demoTask(event string) TaskEvent {
	return TaskEvent{RunID: "run-1", TaskID: "task-1", TaskType: "demo.scan", Event: event, Result: "ok",
		Counts: map[string]int64{"files": 12, "bytes": 4096}, Duration: 2500 * time.Microsecond}
}

// A mixed run: server records at four levels, request, audit and task lines.
// Every line of every file parses, holds the common fields in their order
// and exactly the fields of its file.
func TestMixedRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	f := open(t, dir)
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(f.Handler())
	log.Debug("debug line", "logger", "pipesmith_registry.server", "path", "/a")
	log.Info("info line", "count", 3)
	log.Warn("warn line", "logger", "pipesmith_registry.listeners", "msg", "inner", "time", "t", "level", "l", "port", 14100)
	log.With("logger", "pipesmith_registry.security", "source", "demo").Error("error line", "group", slog.GroupValue(slog.Int("n", 1)))
	f.Request(demoRequest("/repository/demo-releases/a.jar"))
	f.Request(demoRequest("/repository/demo-releases/b.jar"))
	for _, result := range []string{"ok", "denied", "failed"} {
		if err := f.Audit(demoAudit(result)); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []string{"start", "step", "end"} {
		if err := f.Task(demoTask(event)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	server := lines(t, dir, "server.log")
	if len(server) != 4 {
		t.Fatalf("server.log %q", server)
	}
	want := []struct {
		level, logger, msg string
		fields             []string
	}{
		{"DEBUG", "pipesmith_registry.server", "debug line", []string{"path"}},
		{"INFO", "pipesmith_registry", "info line", []string{"count"}},
		{"WARN", "pipesmith_registry.listeners", "warn line", []string{"attr.msg", "attr.time", "attr.level", "port"}},
		{"ERROR", "pipesmith_registry.security", "error line", []string{"source", "group"}},
	}
	for i, w := range want {
		values := checkLine(t, server[i], w.logger, w.fields)
		if values["level"] != w.level || values["msg"] != w.msg {
			t.Errorf("line %d: %q", i, server[i])
		}
	}
	if _, values := keysOf(t, server[2]); values["attr.msg"] != "inner" || values["port"] != json.Number("14100") {
		t.Errorf("attributes %q", server[2])
	}
	if !strings.Contains(server[3], `"group":{"n":1}`) {
		t.Errorf("group %q", server[3])
	}
	requests := lines(t, dir, "requests.log")
	if len(requests) != 2 {
		t.Fatalf("requests.log %q", requests)
	}
	for _, line := range requests {
		values := checkLine(t, line, "pipesmith_registry.requests", requestOf)
		if values["time"] != "2026-11-19T10:00:00.123Z" || values["duration_ms"] != json.Number("1.234") ||
			values["status"] != json.Number("200") || values["level"] != "INFO" {
			t.Errorf("request line %q", line)
		}
	}
	audit := lines(t, dir, "audit.log")
	if len(audit) != 3 {
		t.Fatalf("audit.log %q", audit)
	}
	for i, line := range audit {
		values := checkLine(t, line, "pipesmith_registry.audit", auditOf)
		if values["result"] != []string{"ok", "denied", "failed"}[i] || !reflect.DeepEqual(values["fields"], []any{"name", "format"}) {
			t.Errorf("audit line %q", line)
		}
	}
	tasks := lines(t, dir, "tasks.log")
	if len(tasks) != 3 {
		t.Fatalf("tasks.log %q", tasks)
	}
	for i, line := range tasks {
		values := checkLine(t, line, "pipesmith_registry.tasks", taskOf)
		if values["event"] != []string{"start", "step", "end"}[i] || values["duration_ms"] != json.Number("2.5") ||
			!reflect.DeepEqual(values["counts"], map[string]any{"bytes": json.Number("4096"), "files": json.Number("12")}) {
			t.Errorf("task line %q", line)
		}
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "not ours\n" {
		t.Fatalf("the foreign file changed: %q %v", data, err)
	}
	if f.Errors() != 0 {
		t.Fatalf("errors %d", f.Errors())
	}
}

// A value with a quote, a line break, a NUL byte and non ASCII text comes
// back unchanged from every file, and no control byte reaches a file raw.
func TestValuesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := open(t, dir)
	hostile := "a \"quote\" \\ a line\r\nbreak, a nul \x00, a tab\t and text \u00e4\u00f6\u00fc \u65e5\u672c <&>"
	slog.New(f.Handler()).Warn(hostile, "value", hostile)
	r := demoRequest(hostile)
	r.UserAgent, r.Query = hostile, hostile
	f.Request(r)
	a := demoAudit("ok")
	a.ObjectName, a.Fields = hostile, []string{hostile}
	if err := f.Audit(a); err != nil {
		t.Fatal(err)
	}
	task := demoTask("step")
	task.TaskID, task.Counts = hostile, map[string]int64{hostile: 1}
	if err := f.Task(task); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for name, keys := range map[string][]string{"server.log": {"msg", "value"}, "requests.log": {"path", "query", "user_agent"},
		"audit.log": {"object_name"}, "tasks.log": {"task_id"}} {
		got := lines(t, dir, name)
		if len(got) != 1 {
			t.Fatalf("%s: %q", name, got)
		}
		_, values := keysOf(t, got[0])
		for _, key := range keys {
			if values[key] != hostile {
				t.Errorf("%s %s: %q", name, key, values[key])
			}
		}
	}
	_, audit := keysOf(t, lines(t, dir, "audit.log")[0])
	_, task2 := keysOf(t, lines(t, dir, "tasks.log")[0])
	if !reflect.DeepEqual(audit["fields"], []any{hostile}) || !reflect.DeepEqual(task2["counts"], map[string]any{hostile: json.Number("1")}) {
		t.Errorf("list and object: %v %v", audit["fields"], task2["counts"])
	}
}

// A WARN line is in the file before Handle returns, an INFO line and a
// request line only after Tick; an audit line is there at once.
func TestWarnWrittenAtOnce(t *testing.T) {
	dir := t.TempDir()
	f := open(t, dir)
	log := slog.New(f.Handler())
	log.Info("info waits")
	f.Request(demoRequest("/waits"))
	if got := lines(t, dir, "server.log"); len(got) != 0 {
		t.Fatalf("info before the flush: %q", got)
	}
	log.Warn("warn now")
	got := lines(t, dir, "server.log")
	if len(got) != 2 || !strings.Contains(got[0], "info waits") || !strings.Contains(got[1], "warn now") {
		t.Fatalf("after the warning: %q", got)
	}
	log.Info("info again")
	log.Error("error now")
	if got := lines(t, dir, "server.log"); len(got) != 4 || !strings.Contains(got[3], "error now") {
		t.Fatalf("after the error: %q", got)
	}
	if got := lines(t, dir, "requests.log"); len(got) != 0 {
		t.Fatalf("request line before the flush: %q", got)
	}
	if err := f.Audit(demoAudit("denied")); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, dir, "audit.log"); len(got) != 1 {
		t.Fatalf("audit line: %q", got)
	}
	log.Info("info last")
	f.Tick()
	if got := lines(t, dir, "server.log"); len(got) != 5 || !strings.Contains(got[4], "info last") {
		t.Fatalf("after Tick: %q", got)
	}
	if got := lines(t, dir, "requests.log"); len(got) != 1 {
		t.Fatalf("requests after Tick: %q", got)
	}
}

// A request line never waits for the disk: Request returns while another
// flush holds the file.
func TestRequestDoesNotWaitForTheDisk(t *testing.T) {
	f := open(t, t.TempDir())
	f.requests.disk.Lock()
	done := make(chan struct{})
	go func() {
		f.Request(demoRequest("/busy"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Request waited for the disk")
	}
	f.requests.disk.Unlock()
}

// Close writes what waits; a second Open of the same directory appends.
func TestCloseFlushesAndOpenAppends(t *testing.T) {
	dir := t.TempDir()
	first := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	slog.New(first.Handler()).Info("first run")
	first.Request(demoRequest("/first"))
	if err := first.Task(demoTask("start")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server.log", "requests.log", "tasks.log"} {
		if got := lines(t, dir, name); len(got) != 1 {
			t.Fatalf("%s after Close: %q", name, got)
		}
	}
	second := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	slog.New(second.Handler()).Info("second run")
	second.Request(demoRequest("/second"))
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	server := lines(t, dir, "server.log")
	requests := lines(t, dir, "requests.log")
	if len(server) != 2 || !strings.Contains(server[0], "first run") || !strings.Contains(server[1], "second run") ||
		len(requests) != 2 || !strings.Contains(requests[0], "/first") || !strings.Contains(requests[1], "/second") {
		t.Fatalf("server.log %q, requests.log %q", server, requests)
	}
	// a request after Close reaches neither requests.log nor requests-classic.log; both lines are counted
	second.Request(demoRequest("/late"))
	if second.Errors() != 2 || len(lines(t, dir, "requests.log")) != 2 {
		t.Fatalf("after Close: %d errors", second.Errors())
	}
}

// The directory has mode 0750, every file 0600, and a link in the place of a
// file is not followed.
func TestModesAndNoLink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "logs")
	f := open(t, dir)
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("directory %v %v", info, err)
	}
	for _, name := range []string{"server.log", "requests.log", "audit.log", "tasks.log"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	_ = f.Close()
	linked := filepath.Join(base, "linked")
	target := filepath.Join(base, "target.txt")
	if err := os.MkdirAll(linked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked, "server.log")); err != nil {
		t.Fatal(err)
	}
	g := open(t, linked)
	slog.New(g.Handler()).Warn("must not follow the link")
	if data, _ := os.ReadFile(target); string(data) != "untouched\n" {
		t.Fatalf("the link was followed: %q", data)
	}
	if g.Errors() != 1 {
		t.Fatalf("errors %d, want 1", g.Errors())
	}
}

// An audit result and a task event outside their three words are refused:
// nothing is written and the line counts as an error.
func TestRefusedWords(t *testing.T) {
	dir := t.TempDir()
	f := open(t, dir)
	if err := f.Audit(demoAudit("maybe")); err == nil {
		t.Fatal("audit result maybe was written")
	}
	if err := f.Task(demoTask("begin")); err == nil {
		t.Fatal("task event begin was written")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := append(lines(t, dir, "audit.log"), lines(t, dir, "tasks.log")...); len(got) != 0 {
		t.Fatalf("written: %q", got)
	}
	if f.Errors() != 2 {
		t.Fatalf("errors %d, want 2", f.Errors())
	}
}

// When more than 8 MiB wait for one file the new line is dropped and
// counted; after the flush lines are taken again.
func TestWaitingBytesAreBounded(t *testing.T) {
	dir := t.TempDir()
	f := open(t, dir)
	r := demoRequest("/big")
	r.UserAgent = strings.Repeat("u", 64<<10)
	const sent = 8000 // lines of about 1.4 KiB with the user agent cut at 1 KiB (cap.go)
	for range sent {
		f.Request(r)
	}
	dropped := f.Errors()
	f.Tick()
	written := uint64(len(lines(t, dir, "requests.log")) + len(lines(t, dir, "requests-classic.log"))) // a request has a line in each
	info, err := os.Stat(filepath.Join(dir, "requests.log"))
	if err != nil || dropped == 0 || written+dropped != 2*sent || info.Size() > 8<<20 {
		t.Fatalf("%d written, %d dropped, %v bytes", written, dropped, info.Size())
	}
	f.Request(r)
	f.Tick()
	if got := uint64(len(lines(t, dir, "requests.log")) + len(lines(t, dir, "requests-classic.log"))); got != written+2 || f.Errors() != dropped {
		t.Fatalf("after the flush: %d lines, %d errors", got, f.Errors())
	}
}
