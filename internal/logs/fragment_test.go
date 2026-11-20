package logs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A write to requests.log that stops half way with a full disk leaves part
// of a line (finding 7a-4). The open after retryDelay cuts the fragment
// back, so the first line after the recovery starts on a line of its own
// and every line of the file parses; only the failed line is counted.
func TestShortWriteLeavesNoFragment(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	f := Open(Options{Dir: dir, Now: clock})
	request := func(path string) Request {
		return Request{ID: "0000000000000001", Method: "GET", Path: path, Status: 200, Time: clock()}
	}
	f.Request(request("/one"))
	f.Tick()
	writeFile = func(file *os.File, p []byte) (int, error) {
		if filepath.Base(file.Name()) != "requests.log" {
			return file.Write(p)
		}
		n, _ := file.Write(p[:len(p)/2])
		return n, syscall.ENOSPC
	}
	t.Cleanup(func() { writeFile = (*os.File).Write })
	f.Request(request("/two"))
	f.Tick()
	if f.Errors() != 1 || f.Health().Healthy {
		t.Fatalf("after the short write: errors %d, %+v", f.Errors(), f.Health())
	}
	writeFile = (*os.File).Write
	mu.Lock()
	now = now.Add(retryDelay + time.Second)
	mu.Unlock()
	f.Request(request("/three"))
	f.Tick()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("requests.log does not end with a line break: %q", data)
	}
	if paths := requestPaths(t, dir); len(paths) != 2 || paths["/one"] != 1 || paths["/three"] != 1 || f.Errors() != 1 {
		t.Fatalf("lines %v, errors %d", paths, f.Errors())
	}
}

// appendText appends text to the file at path.
func appendText(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A file that a crash left with part of a line is cut back to its last
// line break when it opens; a file of whole lines stays as it is.
func TestOpenCutsALeftFragment(t *testing.T) {
	dir := t.TempDir()
	whole := `{"path":"/one"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "requests.log"), []byte(whole+`{"path":"/tw`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.log"), []byte(whole), 0o600); err != nil {
		t.Fatal(err)
	}
	f := Open(Options{Dir: dir})
	f.Request(Request{ID: "0000000000000001", Method: "GET", Path: "/three", Status: 200})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if paths := requestPaths(t, dir); len(paths) != 2 || paths["/one"] != 1 || paths["/three"] != 1 {
		t.Fatalf("lines %v", paths)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "server.log")); err != nil || string(data) != whole {
		t.Fatalf("server.log %q %v", data, err)
	}
}
