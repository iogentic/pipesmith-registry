package logs

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// settle starts the work on the directory that waits and returns once it
// ended: compression and retention.
func settle(f *Files) {
	f.maint.kick()
	f.maint.wait()
}

// putSized writes a file of size bytes into dir.
func putSized(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("z", size)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// namesOf lists dir.
func namesOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A rotated file of day D goes when the clock is at or past D plus one day
// plus MaxAge, of every kind, also of requests-classic while the classic
// file is off. Retention runs at Open and in Tick once the hour of the clock
// changes; the active files stay.
func TestRetentionByAge(t *testing.T) {
	dir := t.TempDir()
	settings := DefaultSettings()
	settings.Classic = false
	// expires is the instant at which a file of the day goes
	expires := func(day string) time.Time {
		d, err := time.Parse(dayLayout, day)
		if err != nil {
			t.Fatal(err)
		}
		return d.Add(24 * time.Hour).Add(settings.MaxAge)
	}
	const boundary = "2026-08-20" // goes at 2026-11-19T00:00:00Z
	if got := expires(boundary); !got.Equal(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the boundary day expires at %v", got)
	}
	old := []string{"requests-2026-07-01.1.log.gz", "server-2026-08-19.3.log.gz", "audit-2026-08-01.1.log.gz",
		"tasks-2026-06-30.2.log.gz", "requests-classic-2026-08-19.1.log.gz"}
	for _, name := range old {
		putSized(t, dir, name, 10)
	}
	putSized(t, dir, "requests-"+boundary+".1.log.gz", 10)
	young := []string{"requests-2026-08-21.1.log.gz", "server-2026-11-18.1.log.gz", "audit-2026-11-19.1.log.gz",
		"requests-classic-2026-08-21.1.log.gz"}
	for _, name := range young {
		putSized(t, dir, name, 10)
	}
	foreign := []string{"notes.txt", "requests-2026-07-01.1.log.gz.bak", "requests-2026-07-01.01.log.gz", "requests-2026-07-32.1.log.gz"}
	for _, name := range foreign {
		putSized(t, dir, name, 10)
	}
	clk := newFakeClock(time.Date(2026, 11, 18, 23, 59, 59, 999_999_999, time.UTC))
	f := Open(Options{Dir: dir, Settings: settings, Now: clk.now})
	settle(f)
	names := namesOf(t, dir)
	for _, name := range old {
		if slices.Contains(names, name) {
			t.Errorf("%s is past its age and still there", name)
		}
	}
	for _, name := range append(append([]string{"requests-" + boundary + ".1.log.gz"}, young...), foreign...) {
		if !slices.Contains(names, name) {
			t.Errorf("%s went one nanosecond before its time", name)
		}
	}
	clk.set(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))
	f.Tick()
	settle(f)
	names = namesOf(t, dir)
	if slices.Contains(names, "requests-"+boundary+".1.log.gz") {
		t.Errorf("the file of %s is still there at %v", boundary, clk.now())
	}
	for _, name := range append(append(young, foreign...), "server.log", "requests.log", "audit.log", "tasks.log") {
		if !slices.Contains(names, name) {
			t.Errorf("%s went", name)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// When the rotated files of one kind together are larger than MaxTotalBytes
// the oldest go, by day and then by number, until the kind fits. A file of
// another kind does not count, the active file is never removed, also when
// it alone is larger than the cap, and names outside the pattern stay.
func TestRetentionBySize(t *testing.T) {
	dir := t.TempDir()
	settings := DefaultSettings()
	settings.MaxTotalBytes = 250
	for _, name := range []string{"requests-2026-11-16.1.log.gz", "requests-2026-11-17.1.log.gz", "requests-2026-11-17.2.log.gz",
		"requests-2026-11-17.10.log.gz", "requests-2026-11-18.1.log.gz", "server-2026-11-16.1.log.gz"} {
		putSized(t, dir, name, 100)
	}
	putSized(t, dir, "requests-classic-2026-11-16.1.log.gz", 200)
	putSized(t, dir, "requests.log", 999)
	appendText(t, filepath.Join(dir, "requests.log"), "\n") // whole lines: an open cuts a fragment back (7a-4)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "requests.log"), time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	foreign := []string{"notes.txt", "requests-latest.log.gz", "requests-2026-11-15.1.log.gz.bak", "requests-2026-11-15.007.log.gz"}
	for _, name := range foreign {
		putSized(t, dir, name, 500)
	}
	f := Open(Options{Dir: dir, Settings: settings, Now: newFakeClock(now).now})
	settle(f)
	if got := rotatedOf(t, dir, "requests"); !slices.Equal(got, []string{"requests-2026-11-17.10.log.gz", "requests-2026-11-18.1.log.gz"}) {
		t.Errorf("requests: %v", got)
	}
	if got := rotatedOf(t, dir, "requests-classic"); !slices.Equal(got, []string{"requests-classic-2026-11-16.1.log.gz"}) {
		t.Errorf("requests-classic: %v", got)
	}
	if got := rotatedOf(t, dir, "server"); !slices.Equal(got, []string{"server-2026-11-16.1.log.gz"}) {
		t.Errorf("server: %v", got)
	}
	if info, err := os.Stat(filepath.Join(dir, "requests.log")); err != nil || info.Size() != 1000 {
		t.Errorf("the active file: %v %v", info, err)
	}
	names := namesOf(t, dir)
	for _, name := range foreign {
		if !slices.Contains(names, name) {
			t.Errorf("%s went", name)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Retention runs after each rotation: a file past its age that came after
// Open stays through a Tick of the same hour and goes with the next
// rotation.
func TestRetentionAfterRotation(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: small(), Now: clk.now})
	settle(f)
	const late = "server-2026-01-01.1.log.gz"
	putSized(t, dir, late, 10)
	clk.set(time.Date(2026, 11, 19, 10, 59, 0, 0, time.UTC))
	f.Tick()
	settle(f)
	if !slices.Contains(namesOf(t, dir), late) {
		t.Fatalf("%s went without a rotation in the hour of Open", late)
	}
	log := slog.New(f.Handler())
	for i := range 20 {
		log.Warn("line", "n", i, "pad", strings.Repeat("x", 100))
	}
	settle(f)
	names := namesOf(t, dir)
	if slices.Contains(names, late) {
		t.Errorf("%s is still there after a rotation", late)
	}
	if got := rotatedOf(t, dir, "server"); len(got) < 2 {
		t.Errorf("rotated: %v", got)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
