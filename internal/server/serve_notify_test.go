package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/listeners"
	"github.com/mirosso25/pipesmith-registry/internal/notify"
)

// serviceManager is the socket a test gives as NOTIFY_SOCKET.
type serviceManager struct {
	conn *net.UnixConn
	path string
}

func newServiceManager(t *testing.T) *serviceManager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &serviceManager{conn: conn, path: path}
}

func (m *serviceManager) notifier(watchdogUsec string) *notify.Notifier {
	env := map[string]string{"NOTIFY_SOCKET": m.path, "WATCHDOG_USEC": watchdogUsec}
	return notify.FromEnv(func(key string) string { return env[key] })
}

// next is the next datagram, or "" when none comes within wait.
func (m *serviceManager) next(t *testing.T, wait time.Duration) string {
	t.Helper()
	if err := m.conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := m.conn.Read(buf)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// READY=1 comes once every listener accepts connections, the watchdog
// follows, and STOPPING=1 is the last datagram when the server stops.
func TestServeNotifiesTheServiceManager(t *testing.T) {
	m := newServiceManager(t)
	main, docker := freePort(t), freePort(t)
	cfg := serveConfig(t, dockerDef("demo-docker", docker))
	cfg.Port = main
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, ServeOptions{Notify: m.notifier("400000")}) }()
	if got := m.next(t, 10*time.Second); got != notify.Ready {
		t.Fatalf("first datagram %q", got)
	}
	for _, port := range []int{main, docker} {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err != nil {
			t.Fatalf("port %d after READY=1: %v", port, err)
		}
		c.Close()
	}
	if got := m.next(t, 2*time.Second); got != notify.Watchdog {
		t.Fatalf("datagram %q, want the watchdog", got)
	}
	cancel()
	var last string
	for {
		got := m.next(t, time.Second)
		if got == "" {
			break
		}
		last = got
	}
	if last != notify.Stopping {
		t.Fatalf("last datagram %q", last)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// A server that does not get all its listeners sends nothing: neither a
// refusal before the bind nor a port another process holds gives READY=1.
func TestServeNotifiesNothingWithoutItsListeners(t *testing.T) {
	m := newServiceManager(t)
	holder, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	taken := holder.Addr().(*net.TCPAddr).Port
	cfg := serveConfig(t, dockerDef("demo-docker", taken))
	cfg.Port = freePort(t)
	err = Serve(context.Background(), cfg, ServeOptions{Notify: m.notifier("")})
	var listenErr *listeners.ListenError
	if !errors.As(err, &listenErr) || listenErr.Port != taken {
		t.Fatalf("err %v", err)
	}
	refused := serveConfig(t)
	refused.Port, refused.Bind = listeners.ReservedPorts()[0], unbindable
	if err := Serve(context.Background(), refused, ServeOptions{Notify: m.notifier("")}); err == nil {
		t.Fatal("no refusal")
	}
	if got := m.next(t, 300*time.Millisecond); got != "" {
		t.Fatalf("datagram %q", got)
	}
}

// A state that does not reach the service manager is a warning; the server
// serves on.
func TestServeLogsAFailedNotification(t *testing.T) {
	log := captureLog(t)
	cfg := serveConfig(t)
	cfg.Port = 0
	env := map[string]string{"NOTIFY_SOCKET": filepath.Join(t.TempDir(), "gone"), "WATCHDOG_USEC": "thirty"}
	ready := make(chan []net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, ServeOptions{Notify: notify.FromEnv(func(key string) string { return env[key] }),
			Ready: func(addrs []net.Addr) { ready <- addrs }})
	}()
	addrs := <-ready
	expect(t, raw(t, addrs[0].String(), "GET", "/api/health"), 200)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"WARN pipesmith_registry.listeners: could not send READY=1 to the service manager":                                false,
		`WARN pipesmith_registry.listeners: WATCHDOG_USEC "thirty" is not a positive number of microseconds; no watchdog`: false,
		"WARN pipesmith_registry.listeners: could not send STOPPING=1 to the service manager":                             false,
	}
	for _, line := range log.all() {
		for prefix := range want {
			if len(line) >= len(prefix) && line[:len(prefix)] == prefix {
				want[prefix] = true
			}
		}
	}
	for prefix, seen := range want {
		if !seen {
			t.Errorf("no line %q in %q", prefix, log.all())
		}
	}
}
