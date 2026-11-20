package server

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A body that keeps arriving a byte at a time, each well within the chunk
// timeout, is given up after the total body timeout with 408 (N4); nothing
// of it stays.
func TestRawPutOfATricklingBody(t *testing.T) {
	bodyTimeout.Store(int64(500 * time.Millisecond))
	t.Cleanup(func() { bodyTimeout.Store(0) })
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	conn := rawRequest(t, w.addr(), "/repository/demo-raw/docs/trickle.bin", 1<<20, []byte("first bytes"))
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := conn.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	started := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer after %v: %v", time.Since(started), err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(body), "did not arrive in full") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Fatalf("the answer came after %v, before the body timeout", elapsed)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/trickle.bin"); row != nil {
		t.Fatalf("row %+v", row)
	}
}
