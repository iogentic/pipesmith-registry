package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureDefault sends the lines of slog.Default to a buffer until the test
// ends.
func captureDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// requestPaths counts the lines in every file of requests.log in dir, the
// rotated and compressed ones included, by their path.
func requestPaths(t *testing.T, dir string) map[string]int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]int{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "requests") || strings.HasPrefix(e.Name(), "requests-classic") {
			continue
		}
		fh, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r io.Reader = fh
		if strings.HasSuffix(e.Name(), ".gz") {
			zr, err := gzip.NewReader(fh)
			if err != nil {
				t.Fatalf("%s: %v", e.Name(), err)
			}
			r = zr
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var line struct{ Path string }
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				t.Fatalf("%s: %v: %q", e.Name(), err, sc.Text())
			}
			paths[line.Path]++
		}
		_ = fh.Close()
	}
	return paths
}

// Two servers on one log directory (finding 7a-1): the first takes the lock
// and keeps every line through its rotations, the second opens no file,
// warns once on standard error with the directory and writes nothing there.
// Once the first closes, the directory is free again.
func TestOneServerPerLogDirectory(t *testing.T) {
	warned := captureDefault(t)
	dir := t.TempDir()
	s := Settings{MaxBytes: 1 << 10}
	a := Open(Options{Dir: dir, Settings: s})
	b := Open(Options{Dir: dir, Settings: s})
	for i := 0; i < 400; i++ {
		a.Request(Request{ID: "0000000000000001", Method: "GET", Path: "/a", Status: 200})
		b.Request(Request{ID: "0000000000000002", Method: "GET", Path: "/b", Status: 200})
		a.Request(Request{ID: "0000000000000001", Method: "GET", Path: "/a", Status: 200})
		if i%5 == 0 {
			a.Tick()
			b.Tick()
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	paths := requestPaths(t, dir)
	if paths["/a"] != 800 || paths["/b"] != 0 || a.Errors() != 0 || b.Errors() != 0 {
		t.Fatalf("lines %v, errors %d and %d; want 800 of /a, none of /b, no error", paths, a.Errors(), b.Errors())
	}
	text := warned.String()
	if strings.Count(text, "level=WARN") != 1 || !strings.Contains(text, dir) || !strings.Contains(text, "another server") {
		t.Fatalf("warnings %q", text)
	}
	if h := b.Health(); !h.Healthy {
		t.Fatalf("the second server: %+v", h)
	}
	c := Open(Options{Dir: dir, Settings: s})
	c.Request(Request{ID: "0000000000000003", Method: "GET", Path: "/c", Status: 200})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := requestPaths(t, dir)["/c"]; n != 1 || strings.Count(warned.String(), "level=WARN") != 1 {
		t.Fatalf("after the first closed: %d lines of /c, warnings %q", n, warned.String())
	}
}
