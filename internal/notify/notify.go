// Package notify speaks the notify protocol of systemd (sd_notify(3)) with
// the standard library: one datagram per state to the unix socket that
// NOTIFY_SOCKET names. Without NOTIFY_SOCKET nothing is sent.
package notify

import (
	"fmt"
	"math"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The states this server sends.
const (
	Ready    = "READY=1"
	Stopping = "STOPPING=1"
	Watchdog = "WATCHDOG=1"
)

// sendTimeout bounds one send, so a service manager that does not read
// cannot hold the server.
const sendTimeout = time.Second

// Notifier sends states to the service manager. A nil *Notifier sends
// nothing.
type Notifier struct {
	socket string // NOTIFY_SOCKET
	usec   string // WATCHDOG_USEC
	pid    string // WATCHDOG_PID
}

// FromEnv is the Notifier the environment asks for, read through getenv:
// nil when NOTIFY_SOCKET is empty.
func FromEnv(getenv func(string) string) *Notifier {
	socket := getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	return &Notifier{socket: socket, usec: getenv("WATCHDOG_USEC"), pid: getenv("WATCHDOG_PID")}
}

func (n *Notifier) address() (*net.UnixAddr, error) {
	switch {
	case strings.HasPrefix(n.socket, "/"):
	case strings.HasPrefix(n.socket, "@") && runtime.GOOS == "linux":
		// net maps a leading @ to the abstract namespace of Linux
	default:
		return nil, fmt.Errorf("NOTIFY_SOCKET %q is neither an absolute path nor an abstract socket name", n.socket)
	}
	return &net.UnixAddr{Name: n.socket, Net: "unixgram"}, nil
}

// Send sends one state as one datagram.
func (n *Notifier) Send(state string) error {
	if n == nil {
		return nil
	}
	addr, err := n.address()
	if err != nil {
		return err
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(sendTimeout)); err != nil {
		return err
	}
	_, err = conn.Write([]byte(state))
	return err
}

// WatchdogInterval is half of WATCHDOG_USEC, as sd_watchdog_enabled(3)
// reads it: 0 without WATCHDOG_USEC or when WATCHDOG_PID names another
// process, and an error, with 0, for a value it cannot read.
func (n *Notifier) WatchdogInterval() (time.Duration, error) {
	if n == nil || n.usec == "" {
		return 0, nil
	}
	usec, err := strconv.ParseInt(n.usec, 10, 64)
	if err != nil || usec <= 0 {
		return 0, fmt.Errorf("WATCHDOG_USEC %q is not a positive number of microseconds; no watchdog", n.usec)
	}
	if n.pid != "" {
		pid, err := strconv.Atoi(n.pid)
		if err != nil || pid <= 0 {
			return 0, fmt.Errorf("WATCHDOG_PID %q is not a process ID; no watchdog", n.pid)
		}
		if pid != os.Getpid() {
			return 0, nil
		}
	}
	if usec > math.MaxInt64/int64(time.Microsecond) {
		return math.MaxInt64 / 2, nil
	}
	return time.Duration(usec) * time.Microsecond / 2, nil
}

// StartWatchdog sends Watchdog every WatchdogInterval, the first one an
// interval after the call, until stop is called; stop returns once no send
// is under way. failed gets the error of each send that did not go out.
// Without a watchdog it sends nothing.
func (n *Notifier) StartWatchdog(failed func(error)) (stop func()) {
	interval, _ := n.WatchdogInterval()
	if interval <= 0 {
		return func() {}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
				if err := n.Send(Watchdog); err != nil && failed != nil {
					failed(err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(quit) })
		<-done
	}
}
