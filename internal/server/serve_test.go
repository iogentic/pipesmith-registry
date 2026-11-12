package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/listeners"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// records keeps the log records of the default logger during a test.
type records struct {
	mu    sync.Mutex
	lines []string
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }

func (r *records) Handle(_ context.Context, rec slog.Record) error {
	logger := ""
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			logger = a.Value.String()
		}
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf("%s %s: %s", rec.Level, logger, rec.Message))
	return nil
}

func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

func (r *records) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func captureLog(t *testing.T) *records {
	t.Helper()
	r := &records{}
	previous := slog.Default()
	slog.SetDefault(slog.New(r))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return r
}

// freePort is a port the system handed out for port 0, free again.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func canBind(port int) bool {
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func serveConfig(t *testing.T, defs ...*pyjson.Object) *config.Config {
	t.Helper()
	base := t.TempDir()
	return prepare(t, base, nil, appOptions{defs: defs, noScan: true})
}

// unbindable is an address of TEST-NET-1: a bind there fails, so a refusal
// that came after a bind would show as a bind error, and no test ever binds
// the reserved port.
const unbindable = "192.0.2.1"

func TestServeRefusesBeforeAnyBind(t *testing.T) {
	cfg := serveConfig(t)
	cfg.Port = listeners.ReservedPorts()[0]
	cfg.Bind = unbindable
	err := Serve(context.Background(), cfg, ServeOptions{Ready: func([]net.Addr) { t.Error("ready") }})
	if err == nil || err.Error() != fmt.Sprintf("port %d is reserved on this machine", cfg.Port) || !pyjson.IsValueError(err) {
		t.Fatalf("err %v", err)
	}
	main := freePort(t)
	for _, c := range []struct {
		port int
		text string
	}{
		{80, "Docker port 80 of demo-docker is outside 1024 to 65535"},
		{main, fmt.Sprintf("Docker port %d of demo-docker is the main port of the server", main)},
		{listeners.ReservedPorts()[0], fmt.Sprintf("Docker port %d of demo-docker is reserved on this machine", listeners.ReservedPorts()[0])},
	} {
		cfg := serveConfig(t, dockerDef("demo-docker", c.port))
		cfg.Port, cfg.Bind = main, unbindable
		err := Serve(context.Background(), cfg, ServeOptions{Ready: func([]net.Addr) { t.Error("ready") }})
		if err == nil || err.Error() != c.text {
			t.Fatalf("port %d: err %v", c.port, err)
		}
		if !canBind(main) {
			t.Fatalf("port %d: the main port was bound", c.port)
		}
	}
}

func TestServeOpensAllOrNone(t *testing.T) {
	holder, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	taken := holder.Addr().(*net.TCPAddr).Port
	first, main := freePort(t), freePort(t)
	cfg := serveConfig(t, dockerDef("demo-docker-a", first), dockerDef("demo-docker-b", taken))
	cfg.Port = main
	err = Serve(context.Background(), cfg, ServeOptions{})
	holder.Close()
	var listenErr *listeners.ListenError
	if !errors.As(err, &listenErr) || listenErr.Port != taken || !strings.Contains(err.Error(), fmt.Sprint(taken)) {
		t.Fatalf("err %v", err)
	}
	if !canBind(first) || !canBind(main) {
		t.Fatal("a port stayed bound")
	}
}

func TestServeAnswersLogsAndStops(t *testing.T) {
	log := captureLog(t)
	docker := freePort(t)
	cfg := serveConfig(t, dockerDef("demo-docker", docker))
	cfg.Port = 0
	access := &syncBuffer{}
	ready := make(chan []net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, ServeOptions{AccessLog: access, Ready: func(addrs []net.Addr) { ready <- addrs }})
	}()
	var addrs []net.Addr
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("serve ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	if len(addrs) != 2 || addrs[1].(*net.TCPAddr).Port != docker {
		t.Fatalf("addresses %v", addrs)
	}
	mainAddr := addrs[0].String()
	main := addrs[0].(*net.TCPAddr).Port
	expect(t, raw(t, mainAddr, "GET", "/api/health"), 200)
	expect(t, raw(t, addrs[1].String(), "GET", "/v2/"), 200)
	expect(t, raw(t, addrs[1].String(), "GET", "/api/health"), 404)
	want := []string{
		fmt.Sprintf("INFO pipesmith_registry.listeners: listening on 127.0.0.1 port 0"),
		fmt.Sprintf("INFO pipesmith_registry.listeners: Docker repository demo-docker listening on 127.0.0.1 port %d", docker),
	}
	got := log.all()
	if len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("log %q, want %q", got, want)
	}
	if !strings.Contains(access.String(), `"GET /api/health HTTP/1.1" 200 OK`) {
		t.Fatalf("access %q", access.String())
	}
	// an idle keep alive connection and a request that never finishes its head
	idle, err := net.Dial("tcp", mainAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	fmt.Fprintf(idle, "GET /api/health HTTP/1.1\r\nHost: %s\r\n\r\n", mainAddr)
	buf := make([]byte, 4096)
	if _, err := idle.Read(buf); err != nil {
		t.Fatal(err)
	}
	partial, err := net.Dial("tcp", mainAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	fmt.Fprintf(partial, "GET /api/health HTTP/1.1\r\nHost: ")
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("serve did not stop")
	}
	if took := time.Since(started); took > 5500*time.Millisecond {
		t.Fatalf("stop took %v", took)
	}
	if !canBind(main) || !canBind(docker) {
		t.Fatal("a port stayed bound after the stop")
	}
}

func TestHTTPServerLimits(t *testing.T) {
	s := newHTTPServer(nil)
	if s.ReadHeaderTimeout != 10*time.Second || s.IdleTimeout != 5*time.Second || s.MaxHeaderBytes != 16*1024 ||
		!s.DisableGeneralOptionsHandler || s.ErrorLog == nil {
		t.Fatalf("server %+v", s)
	}
	if shutdownGrace != 5*time.Second {
		t.Fatalf("grace %v", shutdownGrace)
	}
}
