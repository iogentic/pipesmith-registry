package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// The prompt of --ask-ldap-password against a pseudo terminal (review 6,
// finding 10).

// openPTY opens a pseudo terminal pair; the test is skipped where the
// system has none.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatal(errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

func echoOn(t *testing.T, tty *os.File) bool {
	t.Helper()
	var attrs syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&attrs))); errno != 0 {
		t.Fatal(errno)
	}
	return attrs.Lflag&syscall.ECHO != 0
}

// lockedBuffer collects what the terminal shows on the master side.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// drain reads the master side until the slave side is closed.
func drain(master *os.File) (*lockedBuffer, chan struct{}) {
	shown, done := &lockedBuffer{}, make(chan struct{})
	go func() {
		_, _ = io.Copy(shown, master)
		close(done)
	}()
	return shown, done
}

// signalWriter reports its first write: the prompt is on the screen.
type signalWriter struct {
	once  sync.Once
	first chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.first) })
	return len(p), nil
}

func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s within 10 s", what)
	}
}

// Keys typed before the prompt are not part of the password, the answer is
// not echoed, and echo is on again afterwards.
func TestPromptDropsTypeAhead(t *testing.T) {
	master, slave := openPTY(t)
	shown, drained := drain(master)
	if _, err := master.Write([]byte("early")); err != nil {
		t.Fatal(err)
	}
	prompt := &signalWriter{first: make(chan struct{})}
	type result struct {
		answer string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := askSecret(slave, prompt)("bind password: ")
		done <- result{answer, err}
	}()
	within(t, prompt.first, "prompt")
	if _, err := master.Write([]byte("s3cret-pw\n")); err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no answer within 10 s")
	}
	if got.err != nil || got.answer != "s3cret-pw" {
		t.Fatalf("answer %q, error %v; keys typed before the prompt must not count", got.answer, got.err)
	}
	if !echoOn(t, slave) {
		t.Fatal("echo is still off after the answer")
	}
	_ = slave.Close()
	within(t, drained, "end of the terminal output")
	if out := shown.String(); strings.Contains(out, "s3cret") || !strings.Contains(out, "early") {
		t.Fatalf("terminal showed %q", out)
	}
}

const promptChild = "PSR_TEST_PROMPT_CHILD"

// TestHelperPrompt is the child process of
// TestPromptRestoresTheTerminalWhenInterrupted: it asks on its standard
// input, the pseudo terminal of the parent.
func TestHelperPrompt(t *testing.T) {
	if os.Getenv(promptChild) != "1" {
		t.Skip("runs as a child process only")
	}
	answer, err := askSecret(os.Stdin, os.Stderr)("bind password: ")
	fmt.Printf("answer %q, error %v\n", answer, err)
	os.Exit(0)
}

// Ctrl-C or SIGTERM during the prompt restore the terminal and end the
// process by that signal.
func TestPromptRestoresTheTerminalWhenInterrupted(t *testing.T) {
	for _, c := range []struct {
		name string
		sig  syscall.Signal
	}{{"ctrl-c", syscall.SIGINT}, {"sigterm", syscall.SIGTERM}} {
		t.Run(c.name, func(t *testing.T) {
			master, slave := openPTY(t)
			_, _ = drain(master)
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPrompt$")
			cmd.Env = append(os.Environ(), promptChild+"=1")
			cmd.Stdin = slave
			// the pseudo terminal is the controlling terminal of the child,
			// so Ctrl-C reaches it as SIGINT through the line discipline
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			prompted := make(chan struct{})
			go func() {
				r := bufio.NewReader(stderr)
				var seen []byte
				for {
					b, err := r.ReadByte()
					if err != nil {
						return
					}
					seen = append(seen, b)
					if bytes.HasSuffix(seen, []byte("bind password: ")) {
						close(prompted)
						_, _ = io.Copy(io.Discard, r)
						return
					}
				}
			}()
			within(t, prompted, "prompt")
			if echoOn(t, slave) {
				t.Fatal("echo is on during the prompt")
			}
			if c.sig == syscall.SIGINT {
				_, err = master.Write([]byte{0x03})
			} else {
				err = cmd.Process.Signal(c.sig)
			}
			if err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			select {
			case err = <-waited:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("the prompt did not end within 10 s")
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the child ended with %v, stdout %q", err, stdout.String())
			}
			if ws := exit.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != c.sig {
				t.Errorf("the child did not end by %v: signaled %v by %v, exit status %d", c.sig, ws.Signaled(), ws.Signal(), ws.ExitStatus())
			}
			if !echoOn(t, slave) {
				t.Errorf("echo is still off after %s during the prompt", c.name)
			}
		})
	}
}
