package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/notify"
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

// serving runs serve on port in this process until it answers
// /api/health. done gets the exit code; log is standard error once done.
func serving(t *testing.T, port int) (done chan int, log func() []string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan int, 1)
	go func() {
		code := Main([]string{"serve", "--port", fmt.Sprint(port)}, os.Stdout, w)
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
	return done, func() []string { return <-lines }
}

func stopServing(t *testing.T, done chan int) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// Under systemd: READY=1 once the port answers, WATCHDOG=1 at half of
// WATCHDOG_USEC, and STOPPING=1 as the last datagram after SIGTERM.
func TestServeTellsTheServiceManager(t *testing.T) {
	dataDir(t)
	m := newServiceManager(t)
	t.Setenv("NOTIFY_SOCKET", m.path)
	t.Setenv("WATCHDOG_USEC", "400000")
	t.Setenv("WATCHDOG_PID", "")
	port := freePort(t)
	done, _ := serving(t, port)
	if got := m.next(t, time.Second); got != notify.Ready {
		t.Fatalf("first datagram %q", got)
	}
	if got := m.next(t, 2*time.Second); got != notify.Watchdog {
		t.Fatalf("datagram %q, want the watchdog", got)
	}
	stopServing(t, done)
	var last string
	for {
		got := m.next(t, 500*time.Millisecond)
		if got == "" {
			break
		}
		last = got
	}
	if last != notify.Stopping {
		t.Fatalf("last datagram %q", last)
	}
}

// Without NOTIFY_SOCKET serve writes the log of step 3 and nothing else,
// also when the variables of the watchdog are set.
func TestServeWithoutNotifySocket(t *testing.T) {
	dataDir(t)
	t.Setenv("NOTIFY_SOCKET", "")
	t.Setenv("WATCHDOG_USEC", "200000")
	port := freePort(t)
	done, log := serving(t, port)
	time.Sleep(500 * time.Millisecond)
	stopServing(t, done)
	lines := log()
	start := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d{3} INFO pipesmith_registry\.listeners: listening on 127\.0\.0\.1 port ` +
		fmt.Sprint(port) + `$`)
	access := regexp.MustCompile(`^INFO:     127\.0\.0\.1:\d+ - "GET /api/health HTTP/1\.1" 200 OK$`)
	if len(lines) < 2 || !start.MatchString(lines[0]) {
		t.Fatalf("log %q", lines)
	}
	for _, line := range lines[1:] {
		if !access.MatchString(line) {
			t.Fatalf("line %q in log %q", line, lines)
		}
	}
}
