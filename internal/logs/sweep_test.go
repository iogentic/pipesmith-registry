package logs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The hourly run of the maintenance compresses every uncompressed rotated
// file of the names of this package in the directory, not only those whose
// compression failed in this process: a server-<day>.<n>.log put into the
// directory while the files are open stays through a Tick of the same hour,
// and the Tick after the clock moved one hour compresses it; gzip -t passes
// and it holds its lines. Files with a foreign name and the active files stay
// as they are.
func TestHourlyRunCompressesLeftovers(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clk.now})
	t.Cleanup(func() { _ = f.Close() })
	settle(f)
	left := filepath.Join(dir, "server-2026-11-18.1.log")
	lines := []byte(`{"time":"2026-11-18T23:59:59.000Z","level":"INFO","logger":"pipesmith_registry","msg":"left over"}` + "\n")
	if err := os.WriteFile(left, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := map[string][]byte{}
	for _, name := range []string{"notes.log", "demo-2026-11-18.1.log", "server-2026-11-18.log", "server-2026-13-40.1.log", "server-2026-11-18.1.log.bak"} {
		foreign[name] = []byte("not ours: " + name + "\n")
		if err := os.WriteFile(filepath.Join(dir, name), foreign[name], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clk.set(time.Date(2026, 11, 19, 10, 59, 59, 999_999_999, time.UTC))
	f.Tick()
	settle(f)
	if _, err := os.Lstat(left); err != nil {
		t.Fatalf("the rotated file went in the hour of Open: %v", err)
	}
	clk.set(time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC))
	f.Tick()
	settle(f)
	if _, err := os.Lstat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s after the hourly run: %v", filepath.Base(left), err)
	}
	want := []string{"audit.log", "demo-2026-11-18.1.log", "logs.lock", "notes.log", "requests-classic.log", "requests.log", "server-2026-11-18.1.log.bak",
		"server-2026-11-18.1.log.gz", "server-2026-11-18.log", "server-2026-13-40.1.log", "server.log", "tasks.log"}
	if got := namesOf(t, dir); !slices.Equal(got, want) {
		t.Fatalf("the log directory after the hourly run: %v, want %v", got, want)
	}
	gzipTest(t, left+".gz")
	if got := unzipped(t, left+".gz"); !bytes.Equal(got, lines) {
		t.Fatalf("the .gz holds %q, want %q", got, lines)
	}
	for name, want := range foreign {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	for _, name := range []string{"server.log", "requests.log", "audit.log", "tasks.log", "requests-classic.log"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the active file %s: %v", name, err)
		}
	}
	if check := f.Health(); !check.Healthy || f.Errors() != 0 {
		t.Errorf("check %+v, %d errors", check, f.Errors())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
