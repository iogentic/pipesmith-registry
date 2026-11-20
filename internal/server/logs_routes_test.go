package server

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// logsApp is a server with security on whose log routes read dir and hold
// levels that record each save.
func logsApp(t *testing.T) (a *secureApp, dir string, saves *[]map[string]string) {
	t.Helper()
	a = newSecureApp(t, newFakeSecurity())
	dir = t.TempDir()
	var mu sync.Mutex
	saves = &[]map[string]string{}
	levels := logs.NewLevels(func(m map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		*saves = append(*saves, m)
		return nil
	})
	a.srv.logging.set(&LogAccess{Dir: dir, Levels: levels})
	return a, dir, saves
}

func TestLogFileList(t *testing.T) {
	a, dir, _ := logsApp(t)
	at := time.Date(2026, 11, 20, 8, 30, 0, 0, time.UTC)
	for name, data := range map[string]string{"server.log": "s\n", "audit-2026-11-19.1.log.gz": "gz", "logs.lock": "", "notes.txt": "n"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	status(t, a.send("GET", "/api/logs"), 401)
	status(t, a.send("GET", "/api/logs", "basic", asReader), 403)
	r := a.send("GET", "/api/logs", "basic", asAdmin)
	status(t, r, 200)
	list, ok := r.json(t).([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list %s", r.body)
	}
	first := list[0].(map[string]any)
	if first["name"] != "audit-2026-11-19.1.log.gz" || fmt.Sprint(first["size"]) != "2" || first["lastModified"] != "2026-11-20T08:30:00.000Z" || len(first) != 3 {
		t.Errorf("first row %v", first)
	}
	if list[1].(map[string]any)["name"] != "server.log" {
		t.Errorf("second row %v", list[1])
	}
}

func TestLogFileTailAndDownload(t *testing.T) {
	a, dir, _ := logsApp(t)
	content := bytes.Repeat([]byte("0123456789abcdef"), 8000) // 128,000 bytes
	if err := os.WriteFile(filepath.Join(dir, "tasks.log"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{25600, 51200, 102400} {
		r := a.send("GET", "/api/logs/tasks.log?bytes="+strconv.Itoa(n), "basic", asAdmin)
		status(t, r, 200)
		if !bytes.Equal(r.body, content[len(content)-n:]) {
			t.Errorf("bytes=%d: %d bytes", n, len(r.body))
		}
		if r.header.Get("Content-Type") != "text/plain; charset=utf-8" || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("bytes=%d: %v", n, r.header)
		}
	}
	r := a.send("GET", "/api/logs/tasks.log", "basic", asAdmin)
	status(t, r, 200)
	if len(r.body) != 25600 {
		t.Errorf("without bytes: %d bytes, want 25600", len(r.body))
	}
	for _, q := range []string{"bytes=100", "bytes=x", "bytes=", "bytes=25600&bytes=51200", "download=yes"} {
		status(t, a.send("GET", "/api/logs/tasks.log?"+q, "basic", asAdmin), 400)
	}
	r = a.send("GET", "/api/logs/tasks.log?download=1", "basic", asAdmin)
	status(t, r, 200)
	if !bytes.Equal(r.body, content) || r.header.Get("Content-Disposition") != `attachment; filename="tasks.log"` {
		t.Errorf("download: %d bytes, %v", len(r.body), r.header)
	}
	status(t, a.send("GET", "/api/logs/audit.log", "basic", asAdmin), 404)
	status(t, a.send("GET", "/api/logs/tasks.log", "basic", asReader), 403)
}

// TestLogFileNamesOutsideTheList: a name with "..", a leading slash, an
// encoded slash or backslash, a control byte, or a link answers 400 and
// opens nothing.
func TestLogFileNamesOutsideTheList(t *testing.T) {
	a, dir, _ := logsApp(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "server.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/logs/server.log", "/api/logs/../secret.txt", "/api/logs/..", "/api/logs/%2E%2E",
		"/api/logs//etc/hosts", "/api/logs/%2Fetc%2Fhosts", "/api/logs/sub%2Fserver.log", "/api/logs/sub/server.log",
		"/api/logs/..%5Csecret.txt", "/api/logs/server.log%00"} {
		for _, q := range []string{"", "?download=1"} {
			r := a.send("GET", target+q, "basic", asAdmin)
			if r.status != 400 || strings.Contains(string(r.body), "secret") || strings.Contains(string(r.body), "notes") {
				t.Errorf("%s%s: %d %q", target, q, r.status, r.body)
			}
		}
	}
}

func TestLoggerLevels(t *testing.T) {
	a, _, saves := logsApp(t)
	r := a.send("GET", "/api/logging/loggers", "basic", asAdmin)
	status(t, r, 200)
	list := r.json(t).([]any)
	if len(list) != len(logs.Loggers) {
		t.Fatalf("%d loggers", len(list))
	}
	row := list[0].(map[string]any)
	if row["name"] != logs.Loggers[0] || row["level"] != "INFO" || row["default"] != "INFO" || len(row) != 3 {
		t.Errorf("row %v", row)
	}
	csrf := []string{"Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1"}
	put := func(name, body string, headers ...string) *answer {
		return do(t, a.addr(), "PUT", "/api/logging/loggers/"+name, []byte(body), append(headers, "Content-Type", "application/json")...)
	}
	status(t, put("pipesmith_registry.proxy", `{"level": "WARN"}`, csrf...), 204)
	status(t, put("pipesmith_registry.proxy", `{"level": "WARN"}`, "Cookie", "fake-session=s1"), 403)
	status(t, put("pipesmith_registry.proxy", `{"level": "WARN"}`, "basic", asReader), 403)
	if level(t, a, "pipesmith_registry.proxy") != "WARN" {
		t.Error("the level was not set")
	}
	for _, c := range []struct{ name, body, id string }{
		{"pipesmith_registry.demo", `{"level": "WARN"}`, "name"},
		{"pipesmith_registry.requests", `{"level": "WARN"}`, "name"},
		{"pipesmith_registry.server", `{"level": "LOUD"}`, "level"},
		{"pipesmith_registry.server", `{}`, "level"},
		{"pipesmith_registry.server", `not json`, "*"},
	} {
		r := put(c.name, c.body, csrf...)
		status(t, r, 400)
		errs, ok := r.json(t).([]any)
		if !ok || len(errs) != 1 || errs[0].(map[string]any)["id"] != c.id {
			t.Errorf("%s %s: %s, want id %s", c.name, c.body, r.body, c.id)
		}
	}
	if len(a.send("GET", "/api/logging/loggers", "basic", asAdmin).json(t).([]any)) != len(logs.Loggers) {
		t.Error("a refused name was added")
	}
	r = do(t, a.addr(), "DELETE", "/api/logging/loggers/pipesmith_registry.demo", nil, csrf...)
	status(t, r, 400)
	status(t, put("pipesmith_registry.catalog", `{"level": "ERROR"}`, csrf...), 204)
	status(t, do(t, a.addr(), "DELETE", "/api/logging/loggers/pipesmith_registry.proxy", nil, csrf...), 204)
	if level(t, a, "pipesmith_registry.proxy") != "INFO" || level(t, a, "pipesmith_registry.catalog") != "ERROR" {
		t.Error("the delete did not put the proxy back alone")
	}
	status(t, do(t, a.addr(), "POST", "/api/logging/reset", nil, csrf...), 204)
	if level(t, a, "pipesmith_registry.catalog") != "INFO" {
		t.Error("the reset left a level")
	}
	if len(*saves) != 4 || len((*saves)[3]) != 0 {
		t.Errorf("saves %v", *saves)
	}
}

func level(t *testing.T, a *secureApp, name string) string {
	t.Helper()
	for _, row := range a.send("GET", "/api/logging/loggers", "basic", asAdmin).json(t).([]any) {
		if m := row.(map[string]any); m["name"] == name {
			return m["level"].(string)
		}
	}
	t.Fatalf("no logger %s", name)
	return ""
}

func TestLoggerLevelsKeepTheOldLevelWhenTheSaveFails(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	a.srv.logging.set(&LogAccess{Levels: logs.NewLevels(func(map[string]string) error { return errors.New("disk full") })})
	r := do(t, a.addr(), "PUT", "/api/logging/loggers/pipesmith_registry.server", []byte(`{"level": "ERROR"}`), "basic", asAdmin)
	status(t, r, 500)
	if level(t, a, "pipesmith_registry.server") != "INFO" {
		t.Error("the level changed although the save failed")
	}
	// the files off: no file, no list entry
	status(t, a.send("GET", "/api/logs/server.log", "basic", asAdmin), 404)
	if r := a.send("GET", "/api/logs", "basic", asAdmin); r.status != 200 || string(r.body) != "[]" {
		t.Errorf("files off: %d %q", r.status, r.body)
	}
}

func TestLogRoutesWithoutAccessAnswer503(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	for _, target := range []string{"/api/logs", "/api/logs/server.log", "/api/logging/loggers"} {
		status(t, a.send("GET", target, "basic", asAdmin), 503)
	}
}
