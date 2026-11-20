package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// servingWith runs serve with args on a free port in this process until it
// answers /api/health. done gets the exit code; log is standard error once
// done.
func servingWith(t *testing.T, args ...string) (done chan int, log func() []string, port int) {
	t.Helper()
	port = freePort(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan int, 1)
	go func() {
		code := Main(append([]string{"serve", "--port", fmt.Sprint(port)}, args...), os.Stdout, w)
		w.Close()
		done <- code
	}()
	lines := make(chan []string, 1)
	go func() {
		var all []string
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			all = append(all, scanner.Text())
		}
		lines <- all
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return done, func() []string { return <-lines }, port
}

// jsonLines reads a log file of one JSON object per line; a missing file
// fails the test.
func jsonLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("%s: %v: %q", path, err, line)
		}
		out = append(out, v)
	}
	return out
}

var logFileNames = []string{"audit.log", "logs.lock", "requests-classic.log", "requests.log", "server.log", "tasks.log"}

// namesIn lists a directory; a missing one gives nil.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func noLogDirVariable(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvLogDir, "")
}

// serve with the defaults writes server.log with the listening line and
// requests.log with one line per request into <data>/logs.
func TestServeWritesLogFiles(t *testing.T) {
	dir := dataDir(t)
	noLogDirVariable(t)
	done, _, port := servingWith(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	sent := map[string]int{"/api/health?demo=one": 200, "/repository/demo-none/a.txt": 404, "/api/health/": 307}
	for path, status := range sent {
		resp, _ := callNoRedirect(t, base+path)
		if resp.StatusCode != status {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	stopServing(t, done)
	logs := filepath.Join(dir, "logs")
	if names := namesIn(t, logs); !slices.Equal(names, logFileNames) {
		t.Fatalf("log directory %v", names)
	}
	server := jsonLines(t, filepath.Join(logs, "server.log"))
	if len(server) == 0 || server[0]["logger"] != "pipesmith_registry.listeners" || server[0]["level"] != "INFO" ||
		server[0]["msg"] != fmt.Sprintf("listening on 127.0.0.1 port %d", port) {
		t.Fatalf("server.log %v", server)
	}
	requests := jsonLines(t, filepath.Join(logs, "requests.log"))
	if len(requests) != len(sent)+1 {
		t.Fatalf("%d request lines for %d requests and the probe: %v", len(requests), len(sent), requests)
	}
	seen := map[string]int{}
	for _, line := range requests {
		target := line["path"].(string)
		if q := line["query"].(string); q != "" {
			target += "?" + q
		}
		seen[target]++
		if status, ok := sent[target]; ok && line["status"] != float64(status) {
			t.Errorf("%s: status %v", target, line["status"])
		}
		if line["logger"] != "pipesmith_registry.requests" || line["client"] != "127.0.0.1" || line["port"] != float64(port) {
			t.Errorf("request line %v", line)
		}
	}
	for path := range sent {
		if seen[path] != 1 {
			t.Errorf("%s: %d lines", path, seen[path])
		}
	}
	if seen["/api/health"] != 1 {
		t.Errorf("the probe: %d lines", seen["/api/health"])
	}
}

func callNoRedirect(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp, nil
}

// --log-dir beats the variable, the variable beats config.json, and off
// writes no file and makes no directory.
func TestServeLogDirOrder(t *testing.T) {
	dir := dataDir(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	cwd, _ = os.Getwd()
	variable := filepath.Join(t.TempDir(), "variable-logs")
	fromFile := filepath.Join(t.TempDir(), "file-logs")
	cfg := config.New(dir)
	cfg.LogDir = fromFile
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	serveOnce := func(args ...string) {
		t.Helper()
		done, _, _ := servingWith(t, args...)
		stopServing(t, done)
	}
	t.Setenv(config.EnvLogDir, "")
	serveOnce()
	if names := namesIn(t, fromFile); !slices.Equal(names, logFileNames) {
		t.Fatalf("config.json: %v", names)
	}
	t.Setenv(config.EnvLogDir, variable)
	serveOnce()
	if names := namesIn(t, variable); !slices.Equal(names, logFileNames) {
		t.Fatalf("variable: %v", names)
	}
	serveOnce("--log-dir", "option-logs")
	if names := namesIn(t, filepath.Join(cwd, "option-logs")); !slices.Equal(names, logFileNames) {
		t.Fatalf("relative option: %v", names)
	}
	for _, d := range []string{fromFile, variable} {
		if n := len(jsonLines(t, filepath.Join(d, "server.log"))); n != 1 {
			t.Fatalf("%s holds %d listening lines, want the one of its own run", d, n)
		}
	}
	off := filepath.Join(t.TempDir(), "off-variable")
	t.Setenv(config.EnvLogDir, off)
	serveOnce("--log-dir", "off")
	t.Setenv(config.EnvLogDir, "off")
	serveOnce()
	if names := namesIn(t, off); names != nil {
		t.Fatalf("off: %v", names)
	}
	if names := namesIn(t, filepath.Join(dir, "logs")); names != nil {
		t.Fatalf("off: the default directory %v", names)
	}
	if n := len(jsonLines(t, filepath.Join(fromFile, "server.log"))); n != 1 {
		t.Fatalf("off wrote into config.json's directory: %d lines", n)
	}
}

// A value of the setting logs that is not a positive number keeps its
// default and writes one warning to server.log and none to standard error.
func TestServeWarnsOfABadLogsSetting(t *testing.T) {
	dir := importedFixture(t)
	noLogDirVariable(t)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(logs.SettingKey, map[string]any{"max_bytes": -1, "classic": false}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	done, log, _ := servingWith(t)
	stopServing(t, done)
	var warnings []map[string]any
	for _, l := range jsonLines(t, filepath.Join(dir, "logs", "server.log")) {
		if l["logger"] == "pipesmith_registry.logs" {
			warnings = append(warnings, l)
		}
	}
	if len(warnings) != 1 || warnings[0]["level"] != "WARN" || !strings.Contains(warnings[0]["msg"].(string), "max_bytes") {
		t.Fatalf("warnings in server.log: %v", warnings)
	}
	for _, line := range log() {
		if strings.Contains(line, "max_bytes") || strings.Contains(line, "setting logs") {
			t.Fatalf("on standard error: %q", line)
		}
	}
}

// A line of the security logger at run time reaches server.log: the LDAP
// connection of the fixture points at a port where nothing listens, and a
// sign in of a user the local realm does not know asks it.
func TestSecurityLineReachesServerLog(t *testing.T) {
	dir := importedFixture(t)
	noLogDirVariable(t)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil || len(snap.LDAP) != 1 {
		t.Fatalf("%v %v", snap, err)
	}
	snap.LDAP[0].Host, snap.LDAP[0].Port = "127.0.0.1", freePort(t)
	if err := st.Write([]state.Kind{state.KindLDAP}, snap); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), fixtureLDAPPass)
	done, log, port := servingWith(t)
	body := strings.NewReader(`{"username":"demo-directory-user","password":"example-ldap-pw-1"}`)
	resp, _ := call(t, "POST", fmt.Sprintf("http://127.0.0.1:%d/api/session", port), body, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != 401 {
		t.Fatalf("sign in: %d", resp.StatusCode)
	}
	stopServing(t, done)
	const line = "directory did not answer: connection=1 of=1"
	stderr := log()
	if !slices.ContainsFunc(stderr, func(s string) bool { return strings.HasSuffix(s, " WARNING pipesmith_registry.security: "+line) }) {
		t.Fatalf("no warning of the security logger on standard error: %q", stderr)
	}
	if !slices.ContainsFunc(stderr, func(s string) bool {
		return strings.HasSuffix(s, " INFO pipesmith_registry.security: security on: 4 local users, 1 of 1 LDAP connections, anonymous access off")
	}) {
		t.Fatalf("no security summary on standard error: %q", stderr)
	}
	var found bool
	for _, l := range jsonLines(t, filepath.Join(dir, "logs", "server.log")) {
		if l["logger"] == "pipesmith_registry.security" && l["msg"] == line && l["level"] == "WARN" {
			found = true
		}
		if l["msg"] == "security on: 4 local users, 1 of 1 LDAP connections, anonymous access off" {
			t.Errorf("a line from before the files opened: %v", l)
		}
	}
	if !found {
		t.Fatalf("server.log holds no line of the security logger")
	}
	if text, _ := os.ReadFile(filepath.Join(dir, "logs", "server.log")); strings.Contains(string(text), "example-ldap-pw-1") ||
		strings.Contains(string(text), fixtureLDAPPass) {
		t.Fatal("a password is in server.log")
	}
}

var (
	stderrTime   = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d{3} `)
	stderrPort   = regexp.MustCompile(`port \d+$`)
	stderrClient = regexp.MustCompile(`^INFO:     127\.0\.0\.1:\d+ `)
)

// Standard error is byte equal with the files on and off, once the time,
// the port and the client port are set aside.
func TestStderrSameWithFilesOnAndOff(t *testing.T) {
	dataDir(t)
	noLogDirVariable(t)
	run := func(args ...string) string {
		t.Helper()
		done, log, port := servingWith(t, args...)
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		for _, path := range []string{"/api/health?demo=1", "/repository/demo-none/a.txt", "/api/health/", "/api/status"} {
			callNoRedirect(t, base+path)
		}
		call(t, "DELETE", base+"/api/health", nil, nil)
		stopServing(t, done)
		var out []string
		for _, line := range log() {
			line = stderrTime.ReplaceAllString(line, "<time> ")
			line = stderrPort.ReplaceAllString(line, "port <port>")
			out = append(out, stderrClient.ReplaceAllString(line, "INFO:     127.0.0.1:<port> "))
		}
		return strings.Join(out, "\n")
	}
	on := run()
	off := run("--log-dir", "off")
	if on != off || !strings.Contains(on, `"DELETE /api/health HTTP/1.1" 405 Method Not Allowed`) {
		t.Fatalf("files on:\n%s\nfiles off:\n%s", on, off)
	}
}
