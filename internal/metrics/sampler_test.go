package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/migrate"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// fakeProbe gives 100 ms of CPU per call and 50 MiB of memory.
type fakeProbe struct{ cpu time.Duration }

func (p *fakeProbe) read() (time.Duration, int64) {
	p.cpu += 100 * time.Millisecond
	return p.cpu, 50 << 20
}

// fileBytes is the size of a SQLite file with its WAL file.
func fileBytes(t *testing.T, path string) int64 {
	t.Helper()
	var n int64
	for _, name := range []string{path, path + "-wal"} {
		if info, err := os.Stat(name); err == nil {
			n += info.Size()
		}
	}
	return n
}

// The sampler writes one row of 1 minute every 60 s of the clock with the
// requests and bytes of that minute, the CPU time the probe reports for it
// and the memory at its end. After 48 hours the file holds 2,880 rows of 1
// minute; a further day folds the oldest day into rows of 15 minutes, and the
// sums stay as they were.
func TestSamplerKeeps48HoursOfMinutesThenQuarters(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := tasks.NewFakeClock(start)
	rec := NewRecorder(Options{Clock: clock})
	probe := &fakeProbe{}
	s := StartSampler(rec, store, clock, probe.read, nil)
	minute := func(n int) {
		for range n {
			for range 3 {
				rec.Request(logs.Request{Status: 200, RequestBytes: 10, ResponseBytes: 1000}, false)
			}
			clock.Advance(SampleEvery)
		}
	}
	minute(48 * 60)
	if n, err := store.Count(Minute); err != nil || n != 2880 {
		t.Fatalf("%d rows of 1 minute after 48 hours (%v), want 2880", n, err)
	}
	if n, _ := store.Count(Quarter); n != 0 {
		t.Fatalf("%d rows of 15 minutes after 48 hours", n)
	}
	hours, err := store.Series(start, clock.Now(), time.Hour)
	if err != nil || len(hours) != 48 {
		t.Fatalf("%d hours (%v)", len(hours), err)
	}
	for _, h := range hours {
		if h.Requests != 180 || h.BytesSent != 180_000 || h.BytesReceived != 1800 || h.CPU != 6*time.Second || h.MemoryBytes != 50<<20 {
			t.Fatalf("hour %+v", h)
		}
	}
	minute(24 * 60)
	if n, _ := store.Count(Minute); n != 2880 {
		t.Fatalf("%d rows of 1 minute after 72 hours, want 2880", n)
	}
	if n, _ := store.Count(Quarter); n != 96 {
		t.Fatalf("%d rows of 15 minutes after 72 hours, want 96", n)
	}
	hours, _ = store.Series(start, clock.Now(), time.Hour)
	if len(hours) != 72 || hours[0].Requests != 180 || hours[71].Requests != 180 || hours[0].CPU != 6*time.Second {
		t.Fatalf("%d hours, first %+v", len(hours), hours[0])
	}
	if s.Errors() != 0 {
		t.Fatalf("%d samples not written", s.Errors())
	}
	s.Stop()
	clock.Advance(time.Hour)
	if n, _ := store.Count(Minute); n != 2880 {
		t.Fatalf("a stopped sampler wrote: %d rows", n)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// Rows of 15 minutes older than 35 days go; younger ones stay.
func TestQuartersOlderThan35DaysGo(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{QuarterKeep + Quarter*time.Second, QuarterKeep - Quarter*time.Second} {
		if err := store.Add(Sample{Resolution: Quarter, At: now.Add(-age), Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Compact(now); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.Count(Quarter); n != 1 {
		t.Fatalf("%d rows of 15 minutes, want 1", n)
	}
}

// The size of metrics.sqlite, logged for docs/LOGGING.md: after 48 hours of
// samples, and with the most rows the rules keep (2,880 of 1 minute and
// 3,360 of 15 minutes).
func TestMetricsFileSize(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := tasks.NewFakeClock(start)
	rec := NewRecorder(Options{Clock: clock})
	s := StartSampler(rec, store, clock, (&fakeProbe{}).read, nil)
	for range 48 * 60 {
		for range 25 {
			rec.Request(logs.Request{Status: 200, RequestBytes: 300, ResponseBytes: 123_456}, false)
		}
		clock.Advance(SampleEvery)
	}
	s.Stop()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	after48 := fileBytes(t, Path(dir))
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 35 * 24 * 4 {
		if err := store.Add(Sample{Resolution: Quarter, At: start.Add(-time.Duration(i+1) * Quarter * time.Second),
			Requests: 375, BytesSent: 46_296_000, BytesReceived: 112_500, CPU: 90 * time.Second, MemoryBytes: 50 << 20}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	full := fileBytes(t, Path(dir))
	t.Logf("metrics.sqlite: %d bytes after 48 hours of samples, %d bytes with 2880 rows of 1 minute and 3360 of 15 minutes", after48, full)
	if after48 > 1<<20 || full > 2<<20 {
		t.Fatalf("metrics.sqlite holds %d and %d bytes", after48, full)
	}
}

// A binary that knows schema 1 refuses a file of schema 2 and names both.
func TestNewerMetricsFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	newer := append(append([]migrate.Step{}, steps...), migrate.Step{Version: 2, Apply: steps[0].Apply})
	s, err := openWith(path, newer)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := openWith(path, steps); err == nil || err.Error() != path+" has schema 2, this binary knows schema 1" {
		t.Fatalf("%v", err)
	}
}
