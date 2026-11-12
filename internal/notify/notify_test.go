package notify

import (
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// env is a fixed environment for FromEnv.
type env map[string]string

func (e env) get(key string) string { return e[key] }

// manager is the socket of a service manager: it receives the datagrams.
type manager struct {
	conn *net.UnixConn
	path string
}

func listen(t *testing.T) *manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &manager{conn: conn, path: path}
}

// next is the next datagram, or "" when none comes within wait.
func (m *manager) next(t *testing.T, wait time.Duration) string {
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

func TestNothingWithoutSocket(t *testing.T) {
	for _, e := range []env{{}, {"NOTIFY_SOCKET": ""}, {"WATCHDOG_USEC": "1000000", "WATCHDOG_PID": strconv.Itoa(os.Getpid())}} {
		n := FromEnv(e.get)
		if n != nil {
			t.Fatalf("%v: a notifier %+v", e, n)
		}
		if err := n.Send(Ready); err != nil {
			t.Fatalf("%v: %v", e, err)
		}
		if interval, err := n.WatchdogInterval(); interval != 0 || err != nil {
			t.Fatalf("%v: watchdog %v, %v", e, interval, err)
		}
		n.StartWatchdog(func(err error) { t.Errorf("%v: %v", e, err) })()
	}
	// the positive twin: with a socket there is a notifier
	if FromEnv(env{"NOTIFY_SOCKET": listen(t).path}.get) == nil {
		t.Fatal("no notifier with NOTIFY_SOCKET")
	}
}

func TestSendWritesOneDatagramPerState(t *testing.T) {
	m := listen(t)
	n := FromEnv(env{"NOTIFY_SOCKET": m.path}.get)
	for _, state := range []string{Ready, Watchdog, Stopping} {
		if err := n.Send(state); err != nil {
			t.Fatal(err)
		}
		if got := m.next(t, 2*time.Second); got != state {
			t.Fatalf("datagram %q, want %q", got, state)
		}
	}
	if got := m.next(t, 100*time.Millisecond); got != "" {
		t.Fatalf("one more datagram %q", got)
	}
}

func TestSendRefusesAnotherAddress(t *testing.T) {
	for _, socket := range []string{"notify", "vsock:2:4000", filepath.Join(t.TempDir(), "none")} {
		err := FromEnv(env{"NOTIFY_SOCKET": socket}.get).Send(Ready)
		if err == nil {
			t.Fatalf("%q: no error", socket)
		}
		if !filepath.IsAbs(socket) && !strings.Contains(err.Error(), "NOTIFY_SOCKET") {
			t.Fatalf("%q: %v", socket, err)
		}
	}
}

func TestWatchdogInterval(t *testing.T) {
	own := strconv.Itoa(os.Getpid())
	for _, c := range []struct {
		usec, pid string
		want      time.Duration
		problem   bool
	}{
		{"", "", 0, false},
		{"30000000", "", 15 * time.Second, false},
		{"30000000", own, 15 * time.Second, false},
		{"30000000", strconv.Itoa(os.Getpid() + 1), 0, false},
		{"1", "", 500 * time.Nanosecond, false},
		{"9223372036854775807", "", time.Duration(math.MaxInt64 / 2), false},
		{"0", "", 0, true},
		{"-5", "", 0, true},
		{"thirty", "", 0, true},
		{"30000000", "me", 0, true},
		{"99999999999999999999", "", 0, true},
	} {
		e := env{"NOTIFY_SOCKET": "/run/notify", "WATCHDOG_USEC": c.usec}
		if c.pid != "" {
			e["WATCHDOG_PID"] = c.pid
		}
		got, err := FromEnv(e.get).WatchdogInterval()
		if got != c.want || (err != nil) != c.problem {
			t.Errorf("WATCHDOG_USEC %q, WATCHDOG_PID %q: %v, %v; want %v, problem %v", c.usec, c.pid, got, err, c.want, c.problem)
		}
		if err != nil && !strings.Contains(err.Error(), "WATCHDOG_") {
			t.Errorf("%q %q: %v", c.usec, c.pid, err)
		}
	}
}

// WATCHDOG=1 goes out at half of WATCHDOG_USEC, not at once and not at the
// whole interval, and stops with stop.
func TestWatchdogSendsAtHalfTheInterval(t *testing.T) {
	m := listen(t)
	n := FromEnv(env{"NOTIFY_SOCKET": m.path, "WATCHDOG_USEC": "600000"}.get)
	started := time.Now()
	stop := n.StartWatchdog(func(err error) { t.Error(err) })
	var at []time.Duration
	for range 4 {
		got := m.next(t, 3*time.Second)
		if got != Watchdog {
			stop()
			t.Fatalf("datagram %q after %v", got, at)
		}
		at = append(at, time.Since(started))
	}
	stop()
	// 300 ms apart: the first not before 240 ms, the fourth near 1.2 s and
	// well before the 2.4 s of the whole interval
	if at[0] < 240*time.Millisecond || at[3] < 1000*time.Millisecond || at[3] > 1900*time.Millisecond {
		t.Fatalf("sent after %v", at)
	}
	if got := m.next(t, 700*time.Millisecond); got != "" {
		t.Fatalf("%q after stop", got)
	}
}

func TestWatchdogReportsAFailedSend(t *testing.T) {
	n := FromEnv(env{"NOTIFY_SOCKET": filepath.Join(t.TempDir(), "gone"), "WATCHDOG_USEC": "100000"}.get)
	failed := make(chan error, 10)
	stop := n.StartWatchdog(func(err error) { failed <- err })
	defer stop()
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no failure reported")
	}
}
