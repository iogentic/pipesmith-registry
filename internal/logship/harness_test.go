package logship

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The helpers of every test of this package: the setting and the secret
// files of a test, the log of the manager, lines with an id, and two stub
// receivers that know no protocol (an HTTP server that answers by a script,
// a TCP listener that never answers).

// testVersion is the version the manager of a test sends in User-Agent.
const testVersion = "9.9.9-test"

// demoStart is the time of the first line of a test.
var demoStart = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// testLine is one JSON line of kind in the layout of internal/logs, with the
// field line_id <kind>-<n>.
func testLine(kind string, n int) logs.Line { return paddedLine(kind, n, 0) }

// paddedLine is testLine with a field pad of pad bytes.
func paddedLine(kind string, n, pad int) logs.Line {
	at := demoStart.Add(time.Duration(n) * time.Millisecond)
	id := fmt.Sprintf("%s-%04d", kind, n)
	data := fmt.Sprintf(`{"time":%q,"level":"INFO","logger":"pipesmith_registry.demo","msg":%q,"line_id":%q`,
		at.Format("2006-01-02T15:04:05.000Z07:00"), "line "+id, id)
	if pad > 0 {
		data += `,"pad":"` + strings.Repeat("x", pad) + `"`
	}
	data += "}"
	return logs.Line{Kind: kind, Time: at, Level: slog.LevelInfo, Logger: "pipesmith_registry.demo", Message: "line " + id,
		JSON: []byte(data)}
}

// testLines is n lines of kind, numbered from first.
func testLines(kind string, first, n int) []logs.Line {
	out := make([]logs.Line, n)
	for i := range out {
		out[i] = testLine(kind, first+i)
	}
	return out
}

// idOf is the line_id of a JSON line.
func idOf(line []byte) (string, error) {
	var v struct {
		LineID string `json:"line_id"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return "", err
	}
	return v.LineID, nil
}

// idsOf is the line ids of lines, in their order.
func idsOf(t *testing.T, lines []string) []string {
	t.Helper()
	var out []string
	for _, l := range lines {
		id, err := idOf([]byte(l))
		if err != nil {
			t.Fatalf("a line that is not JSON: %q", l)
		}
		out = append(out, id)
	}
	return out
}

// idsWant is the line ids of lines.
func idsWant(lines []logs.Line) []string {
	var out []string
	for _, l := range lines {
		id, _ := idOf(l.JSON)
		out = append(out, id)
	}
	return out
}

// setting is the setting log_targets of a test and its secret files.
type setting struct {
	mu      sync.Mutex
	value   []byte
	secrets map[string]string
}

// put stores rows as the value of the setting.
func (s *setting) put(t *testing.T, rows ...map[string]any) {
	t.Helper()
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = data
}

func (s *setting) putSecret(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = map[string]string{}
	}
	s.secrets[name] = value
}

func (s *setting) rows() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, nil
}

func (s *setting) secret(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.secrets[name]
	if !ok {
		return "", &fs.PathError{Op: "open", Path: "secrets/" + name, Err: fs.ErrNotExist}
	}
	return value, nil
}

// testLog holds what a manager logs, one JSON object per line.
type testLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *testLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *testLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// records is the lines of the log with the message msg.
func (l *testLog) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.text()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line that is not JSON: %q", line)
		}
		if rec["logger"] != Logger {
			t.Fatalf("a log line of the logger %v: %q", rec["logger"], line)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// stopLine is the one line "log target stopped" of target.
func (l *testLog) stopLine(t *testing.T, target string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, rec := range l.records(t, "log target stopped") {
		if rec["target"] == target {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d lines log target stopped for %s in %s", len(found), target, l.text())
	}
	return found[0]
}

// number is a count of a log line.
func number(t *testing.T, rec map[string]any, key string) int64 {
	t.Helper()
	v, ok := rec[key].(float64)
	if !ok {
		t.Fatalf("no count %s in %v", key, rec)
	}
	return int64(v)
}

// options are the options of the manager of a test.
func options(set *setting, log io.Writer) Options {
	return Options{Rows: set.rows, Secret: set.secret, Logger: slog.New(slog.NewJSONHandler(log, nil)),
		Version: testVersion, Host: "demo-host"}
}

// startManager starts a manager on clk that Cleanup closes.
func startManager(t *testing.T, clk clock, set *setting, log io.Writer) *Manager {
	t.Helper()
	m := newManager(options(set, log), clk)
	t.Cleanup(m.Close)
	return m
}

// statsOf is the counters of the running target name.
func statsOf(t *testing.T, m *Manager, name string) Stats {
	t.Helper()
	for _, st := range m.Stats() {
		if st.Target == name {
			return st
		}
	}
	t.Fatalf("no running target %s in %+v", name, m.Stats())
	return Stats{}
}

// namesOf is the names of the running targets, in their order.
func namesOf(m *Manager) string {
	var names []string
	for _, st := range m.Stats() {
		names = append(names, st.Target)
	}
	return strings.Join(names, ",")
}

// waitFor polls cond in real time for up to 10 s.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// shipAll hands lines to the tap from one goroutine; a tap that does not
// return within 5 s fails the test instead of hanging it.
func shipAll(t *testing.T, m *Manager, lines []logs.Line) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, l := range lines {
			m.Ship(l)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tap did not return within 5 s")
	}
}

// goroutinesBack waits until no more goroutines run than before and dumps
// the stacks when they do not come back.
func goroutinesBack(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines, %d before:\n%s", runtime.NumGoroutine(), before, buf[:n])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// webRequest is one request a webStub got.
type webRequest struct {
	method, path, query string
	header              http.Header
	body                []byte
}

// webStub is an HTTP receiver that keeps every request and answers each by
// the function of the test; n counts the requests from 1, and a nil answer
// is 200.
type webStub struct {
	srv    *httptest.Server
	answer func(n int, w http.ResponseWriter, r *http.Request)
	mu     sync.Mutex
	got    []webRequest
}

func newWebStub(t *testing.T, answer func(n int, w http.ResponseWriter, r *http.Request)) *webStub {
	t.Helper()
	s := &webStub{answer: answer}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *webStub) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "body not read", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.got = append(s.got, webRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
	n := len(s.got)
	s.mu.Unlock()
	if s.answer != nil {
		s.answer(n, w, r)
	}
}

func (s *webStub) url() string { return s.srv.URL }

func (s *webStub) requests() []webRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]webRequest(nil), s.got...)
}

// lines is every line of every body, in order.
func (s *webStub) lines() []string {
	var out []string
	for _, r := range s.requests() {
		for _, l := range strings.Split(strings.TrimSuffix(string(r.body), "\n"), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

// statuses answers request n with codes[n-1]; the last code repeats.
func statuses(codes ...int) func(int, http.ResponseWriter, *http.Request) {
	return func(n int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(codes[min(n, len(codes))-1])
	}
}

// silentStub accepts TCP connections, reads what comes and never answers.
type silentStub struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []*silentConn
	wg    sync.WaitGroup
}

type silentConn struct {
	conn           net.Conn
	opened, closed time.Time
}

func newSilentStub(t *testing.T) *silentStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &silentStub{ln: ln}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.close)
	return s
}

func (s *silentStub) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &silentConn{conn: conn, opened: time.Now()}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_, _ = io.Copy(io.Discard, conn) // until the client closes
			s.mu.Lock()
			c.closed = time.Now()
			s.mu.Unlock()
		}()
	}
}

func (s *silentStub) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *silentStub) addr() string { return s.ln.Addr().String() }

// count is the number of connections accepted.
func (s *silentStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// spans is how long each connection the client closed was open.
func (s *silentStub) spans() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []time.Duration
	for _, c := range s.conns {
		if !c.closed.IsZero() {
			out = append(out, c.closed.Sub(c.opened))
		}
	}
	return out
}
