package logs

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// compressions is the seam of a test: the first compression of each rotated
// file fails as on a full disk, a later one compresses.
type compressions struct {
	mu    sync.Mutex
	calls map[string]int // by path, the compressions asked for
}

// failOnce sets the seam of f; call it after Open, before the first line.
func failOnce(f *Files) *compressions {
	c := &compressions{calls: map[string]int{}}
	f.maint.compressor = func(path string) error {
		c.mu.Lock()
		c.calls[path]++
		first := c.calls[path] == 1
		c.mu.Unlock()
		if first {
			return &fs.PathError{Op: "write", Path: path + ".gz.tmp", Err: syscall.ENOSPC}
		}
		return compress(path)
	}
	return c
}

func (c *compressions) of(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[path]
}

// rotateOnce writes two lines of about 700 bytes to server.log of Files with
// the limits of small(), so the file rotates once, to server-<day>.1.log, and
// waits for the maintenance. It returns the path of the rotated file.
func rotateOnce(t *testing.T, f *Files, dir string) string {
	t.Helper()
	log := slog.New(f.Handler())
	log.Warn("first", "pad", strings.Repeat("a", 600))
	log.Warn("second", "pad", strings.Repeat("b", 600))
	settle(f)
	return filepath.Join(dir, "server-2026-11-19.1.log")
}

// A compression that fails turns the check red: the detail is the name of
// the rotated file and the error of the system, Errors stays 0, the .log
// stays and no .gz name exists for it, also after a Tick in the same hour.
// The hourly run of the maintenance tries the file again: one hour after
// Open, Tick compresses it, gzip -t passes, the .log is gone and the check
// is green.
func TestFailedCompressionIsRetriedHourly(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: small(), Now: clk.now})
	t.Cleanup(func() { _ = f.Close() })
	seam := failOnce(f)
	path := rotateOnce(t, f, dir)
	name := filepath.Base(path)
	rotated := lines(t, dir, name)
	if seam.of(path) != 1 || len(rotated) != 1 || !strings.Contains(rotated[0], `"msg":"first"`) {
		t.Fatalf("%d compressions of %s, which holds %q", seam.of(path), name, rotated)
	}
	detail := name + ": write " + path + ".gz.tmp: " + syscall.ENOSPC.Error()
	unhealthy := func(when string) {
		t.Helper()
		if check := f.Health(); check.Healthy || check.Detail != detail {
			t.Fatalf("%s: %+v, want the detail %q", when, check, detail)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		for _, gz := range []string{path + ".gz", path + ".gz.tmp"} {
			if _, err := os.Lstat(gz); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: %s is there: %v", when, filepath.Base(gz), err)
			}
		}
		if f.Errors() != 0 {
			t.Fatalf("%s: errors %d", when, f.Errors())
		}
	}
	unhealthy("after the failed compression")
	clk.set(time.Date(2026, 11, 19, 10, 59, 59, 999_999_999, time.UTC))
	f.Tick()
	settle(f)
	unhealthy("after a Tick in the same hour")
	clk.set(time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC))
	f.Tick()
	settle(f)
	if got := rotatedOf(t, dir, "server"); !slices.Equal(got, []string{name + ".gz"}) {
		t.Fatalf("rotated after the hourly run: %v", got)
	}
	gzipTest(t, path+".gz")
	if got := splitLines(unzipped(t, path+".gz")); !slices.Equal(got, rotated) {
		t.Fatalf("%s.gz holds %q, want %q", name, got, rotated)
	}
	for _, p := range []string{path, path + ".gz.tmp"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s after the hourly run: %v", filepath.Base(p), err)
		}
	}
	if check := f.Health(); !check.Healthy || check.Detail != "" || seam.of(path) != 2 || f.Errors() != 0 {
		t.Fatalf("after the hourly run: %+v, %d compressions, %d errors", check, seam.of(path), f.Errors())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A rotated file whose compression failed and that is gone by the hourly
// run, removed by hand or by the retention, leaves nothing to compress: the
// hourly run tries it once more, and its part of the detail clears.
func TestFailedCompressionOfARemovedFileClears(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: small(), Now: clk.now})
	t.Cleanup(func() { _ = f.Close() })
	seam := failOnce(f)
	path := rotateOnce(t, f, dir)
	if check := f.Health(); check.Healthy || !strings.HasPrefix(check.Detail, filepath.Base(path)+": ") {
		t.Fatalf("after the failed compression: %+v", check)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clk.set(time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC))
	f.Tick()
	settle(f)
	if check := f.Health(); !check.Healthy || check.Detail != "" || seam.of(path) != 2 {
		t.Fatalf("after the hourly run: %+v, %d compressions", check, seam.of(path))
	}
	if names := namesOf(t, dir); !slices.Equal(names, []string{"audit.log", "logs.lock", "requests-classic.log", "requests.log", "server.log", "tasks.log"}) {
		t.Fatalf("the log directory: %v", names)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
