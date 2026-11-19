package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
)

const bigPath = "demo-raw/big.bin"

// bigFile is 64 MiB of random bytes served from a directory store.
func bigFile(t *testing.T) (*config.Config, []byte) {
	t.Helper()
	data := make([]byte, 64<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	cfg := prepare(t, base, []config.StoreConfig{treeStore(t, base, "tree", map[string][]byte{bigPath: data})}, appOptions{})
	cfg.Port = 0
	return cfg, data
}

// serving runs Serve on cfg until stop is called; stop returns how long
// Serve took to return after the stop began.
type serving struct {
	addr string
	stop func() time.Duration
}

func startServing(t *testing.T, cfg *config.Config) *serving {
	t.Helper()
	ready := make(chan []net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, ServeOptions{Ready: func(addrs []net.Addr) { ready <- addrs }}) }()
	var addr string
	select {
	case addrs := <-ready:
		addr = addrs[0].String()
	case err := <-done:
		t.Fatalf("serve ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	stopped := false
	s := &serving{addr: addr}
	s.stop = func() time.Duration {
		if stopped {
			return 0
		}
		stopped = true
		started := time.Now()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("serve did not stop within 60 s")
		}
		return time.Since(started)
	}
	t.Cleanup(func() { s.stop() })
	return s
}

// download sends a GET of the big file on a connection with a small receive
// buffer and returns the connection and the answer with its body unread.
func download(t *testing.T, addr string) (net.Conn, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.(*net.TCPConn).SetReadBuffer(64 << 10); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /repository/%s HTTP/1.1\r\nHost: %s\r\n\r\n", bigPath, addr)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 64<<10), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 64<<20 {
		t.Fatalf("answer %d, length %d", resp.StatusCode, resp.ContentLength)
	}
	return conn, resp
}

func TestDrainTime(t *testing.T) {
	for text, want := range map[string]time.Duration{"": shutdownGrace, "1": time.Second, "300": 300 * time.Second,
		"3600": time.Hour} {
		t.Setenv(DrainVariable, text)
		if got, err := drainTime(); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"0", "3601", "-1", "+5", "1.5", "5s", " 5", "5 ", "05", "abc", "99999999999999999999"} {
		t.Setenv(DrainVariable, text)
		if _, err := drainTime(); err == nil || !strings.Contains(err.Error(), DrainVariable) {
			t.Errorf("%q: %v", text, err)
		}
	}
}

// A bad value refuses the start before a bind.
func TestDrainBadValueRefusesTheStart(t *testing.T) {
	t.Setenv(DrainVariable, "0")
	cfg := serveConfig(t)
	cfg.Port, cfg.Bind = freePort(t), unbindable
	err := Serve(context.Background(), cfg, ServeOptions{Ready: func([]net.Addr) { t.Error("ready") }})
	if err == nil || !strings.HasPrefix(err.Error(), DrainVariable+` is "0"; give whole seconds from 1 to 3600`) {
		t.Fatalf("err %v", err)
	}
}

// After the stop began a new connection is refused, while an open answer
// still runs.
func TestDrainRefusesANewConnection(t *testing.T) {
	t.Setenv(DrainVariable, "4")
	cfg, _ := bigFile(t)
	s := startServing(t, cfg)
	download(t, s.addr)
	took := make(chan time.Duration, 1)
	go func() { took <- s.stop() }()
	time.Sleep(300 * time.Millisecond)
	if conn, err := net.DialTimeout("tcp", s.addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("a new connection was accepted after the stop began")
	}
	if d := <-took; d < 3*time.Second {
		t.Fatalf("serve returned after %v, before the drain time", d)
	}
}

// An answer of 64 MiB read slower than the default grace and faster than
// the drain ends complete.
func TestDrainLetsAnOpenAnswerEnd(t *testing.T) {
	t.Setenv(DrainVariable, "30")
	cfg, data := bigFile(t)
	s := startServing(t, cfg)
	_, resp := download(t, s.addr)
	took := make(chan time.Duration, 1)
	go func() { took <- s.stop() }()
	// 64 MiB at 8 MiB/s: about 8 s
	const chunk, perChunk = 256 << 10, 32 * time.Millisecond
	sum := sha256.New()
	started := time.Now()
	var n int64
	for {
		m, err := io.CopyN(sum, resp.Body, chunk)
		n += m
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("after %d bytes: %v", n, err)
		}
		time.Sleep(perChunk)
	}
	read := time.Since(started)
	want := sha256.Sum256(data)
	if n != int64(len(data)) || !bytes.Equal(sum.Sum(nil), want[:]) {
		t.Fatalf("read %d bytes, want %d, or another SHA-256", n, len(data))
	}
	if read < 6*time.Second {
		t.Fatalf("the read took %v, not slower than the default grace", read)
	}
	if d := <-took; d < shutdownGrace || d > 30*time.Second {
		t.Fatalf("serve returned after %v", d)
	}
}

// stalled stops a server whose one client stops reading and returns how
// long the stop took.
func stalled(t *testing.T) time.Duration {
	t.Helper()
	cfg, _ := bigFile(t)
	s := startServing(t, cfg)
	download(t, s.addr)
	return s.stop()
}

func TestDrainEndsAtTheDrainTime(t *testing.T) {
	t.Setenv(DrainVariable, "3")
	if d := stalled(t); d < 3*time.Second || d > 4500*time.Millisecond {
		t.Fatalf("stop took %v, want 3 s", d)
	}
}

func TestDrainDefaultIsTheGrace(t *testing.T) {
	t.Setenv(DrainVariable, "")
	if d := stalled(t); d < shutdownGrace || d > shutdownGrace+1500*time.Millisecond {
		t.Fatalf("stop took %v, want %v", d, shutdownGrace)
	}
}
