//go:build darwin || linux

package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const serveChild = "PIPESMITH_REGISTRY_TEST_SERVE_PORT"

// TestHelperServe is the child process of
// TestServeEndsOnATermSentWhileStopped: serve on the port it is given.
func TestHelperServe(t *testing.T) {
	port := os.Getenv(serveChild)
	if port == "" {
		t.Skip("runs as a child process only")
	}
	os.Exit(Main([]string{"serve", "--port", port}, os.Stdout, os.Stderr))
}

// hostPort is a free port of 14200 to 14299, the ports of this work on the
// host, where the test also runs as a darwin binary.
func hostPort(t *testing.T) int {
	t.Helper()
	for port := 14200; port < 14300; port++ {
		if l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			l.Close()
			return port
		}
	}
	t.Fatal("no free port in 14200 to 14299")
	return 0
}

// A SIGTERM that reaches serve while SIGSTOP holds it ends serve once
// SIGCONT lets it go on, as it ends the Python server.
func TestServeEndsOnATermSentWhileStopped(t *testing.T) {
	dataDir(t)
	t.Setenv("NOTIFY_SOCKET", "")
	port := hostPort(t)
	logPath := filepath.Join(t.TempDir(), "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServe$")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", serveChild, port))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	waiting := false
	defer func() {
		if !waiting {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not listen")
		}
		time.Sleep(50 * time.Millisecond)
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	// WaitStatus.Stopped of darwin leaves SIGSTOP out; the bits are the same
	// on both systems: 0x7f in the low byte, the signal in the next.
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &status, syscall.WUNTRACED, nil); err != nil || status&0xff != 0x7f ||
		syscall.Signal(status>>8&0xff) != syscall.SIGSTOP {
		t.Fatalf("not stopped: %v, status %#x", err, uint32(status))
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waiting = true
	go func() { ended <- cmd.Wait() }()
	select {
	case err := <-ended:
		if err != nil {
			text, _ := os.ReadFile(logPath)
			t.Fatalf("serve ended with %v\n%s", err, text)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-ended
		text, _ := os.ReadFile(logPath)
		t.Fatalf("serve still ran 10 s after SIGCONT: the SIGTERM sent while it was stopped was lost\n%s", text)
	}
}
