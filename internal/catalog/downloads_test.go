package catalog

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The writer of decision A19 and the build time of ruling CL3, at a fake
// clock: every time below is given, none is read from the machine.

var dlStart = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

// fillRaw fills the store demo-store with the raw paths given, each blob
// created at created.
func fillRaw(t *testing.T, c *Catalog, created int64, paths ...string) {
	t.Helper()
	f, err := c.Begin("demo-store")
	if err != nil {
		t.Fatal(err)
	}
	sha1 := "86f7e437faa5a7fce15d1ddcb9eaeaea377667b8"
	for i, p := range paths {
		e := stores.Entry{Repo: "demo-raw", Path: p, Size: 1, SHA1: &sha1, CreatedMS: created,
			Location: "content/vol-01/chap-01/" + string(rune('a'+i)) + ".bytes"}
		if err := f.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Commit(func(string) string { return "raw" }, []string{"demo-store"}); err != nil {
		t.Fatal(err)
	}
}

// downloaded is the stored download time of a raw path, zero for none.
func downloaded(t *testing.T, c *Catalog, path string) time.Time {
	t.Helper()
	var ms sql.NullInt64
	if err := c.readers.QueryRow("SELECT last_downloaded_ms FROM assets WHERE repo = 'demo-raw' AND path = ?", path).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

// counting is a writer over the catalog that counts its calls.
type counting struct {
	mu    sync.Mutex
	c     *Catalog
	calls int
}

func (w *counting) write(times []DownloadTime) (int, map[AssetKey]time.Time, error) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	return w.c.SetDownloaded(times, DownloadFresh)
}

func (w *counting) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// A note waits for its batch; the batch sets the column; a second download
// within 12 hours writes nothing, one after 12 hours writes the new time.
func TestADownloadSetsTheLastDownloadTime(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	fillRaw(t, c, 1, "docs/a.txt", "docs/b.txt")
	w := &counting{c: c}
	d := NewDownloadTimes(w.write, time.Hour)
	d.Note("demo-raw", "docs/a.txt", dlStart)
	if got := downloaded(t, c, "docs/a.txt"); !got.IsZero() || w.count() != 0 {
		t.Fatalf("a note was written before its batch: %v, %d writes", got, w.count())
	}
	if p := d.Pending("demo-raw"); len(p) != 1 || !p["docs/a.txt"].Equal(dlStart) {
		t.Fatalf("pending %v", p)
	}
	if n, err := d.Flush(); err != nil || n != 1 {
		t.Fatalf("flush %d %v", n, err)
	}
	if got := downloaded(t, c, "docs/a.txt"); !got.Equal(dlStart) {
		t.Fatalf("stored %v, want %v", got, dlStart)
	}
	if got := downloaded(t, c, "docs/b.txt"); !got.IsZero() {
		t.Fatalf("an asset without a download got %v", got)
	}
	calls := w.count()
	d.Note("demo-raw", "docs/a.txt", dlStart.Add(11*time.Hour+59*time.Minute))
	if n, err := d.Flush(); err != nil || n != 0 || w.count() != calls {
		t.Fatalf("a second download within 12 hours wrote: %d rows, %d writes, %v", n, w.count()-calls, err)
	}
	later := dlStart.Add(12 * time.Hour)
	d.Note("demo-raw", "docs/a.txt", later)
	if n, err := d.Flush(); err != nil || n != 1 {
		t.Fatalf("a download after 12 hours: %d %v", n, err)
	}
	if got := downloaded(t, c, "docs/a.txt"); !got.Equal(later) {
		t.Fatalf("stored %v, want %v", got, later)
	}
	// a writer that starts afresh knows no stored value; the file still
	// keeps a value younger than 12 hours
	fresh := NewDownloadTimes(w.write, time.Hour)
	fresh.Note("demo-raw", "docs/a.txt", later.Add(time.Hour))
	if n, err := fresh.Close(); err != nil || n != 0 || !downloaded(t, c, "docs/a.txt").Equal(later) {
		t.Fatalf("a restart wrote a value younger than 12 hours: %d %v", n, err)
	}
	// a path the catalog does not hold is no row
	fresh = NewDownloadTimes(w.write, time.Hour)
	fresh.Note("demo-raw", "docs/none.txt", later)
	if n, err := fresh.Close(); err != nil || n != 0 {
		t.Fatalf("an unknown path: %d %v", n, err)
	}
}

// The batch is written every DownloadBatch after its first note, without a
// call of Flush; Close writes what waits and takes no later note.
func TestTheBatchIsWrittenByItsTimerAndAtClose(t *testing.T) {
	if DownloadBatch != 10*time.Second || DownloadFresh != 12*time.Hour {
		t.Fatalf("batch %v, fresh %v", DownloadBatch, DownloadFresh)
	}
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	fillRaw(t, c, 1, "docs/a.txt", "docs/b.txt")
	w := &counting{c: c}
	d := NewDownloadTimes(w.write, 30*time.Millisecond)
	d.Note("demo-raw", "docs/a.txt", dlStart)
	deadline := time.Now().Add(5 * time.Second)
	for downloaded(t, c, "docs/a.txt").IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("the timer wrote no batch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	slow := NewDownloadTimes(w.write, time.Hour)
	slow.Note("demo-raw", "docs/b.txt", dlStart)
	if n, err := slow.Close(); err != nil || n != 1 || !downloaded(t, c, "docs/b.txt").Equal(dlStart) {
		t.Fatalf("close: %d %v", n, err)
	}
	slow.Note("demo-raw", "docs/b.txt", dlStart.Add(24*time.Hour))
	if p := slow.Pending("demo-raw"); len(p) != 0 {
		t.Fatalf("a note after close waits: %v", p)
	}
}

// A rescan keeps the last download time of every path that stays, also of
// a path whose blob changed; a path that goes takes its value with it.
func TestARescanKeepsTheLastDownloadTime(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	fillRaw(t, c, 1, "docs/a.txt", "docs/b.txt", "docs/c.txt")
	if n, _, err := c.SetDownloaded([]DownloadTime{{AssetKey{"demo-raw", "docs/a.txt"}, dlStart},
		{AssetKey{"demo-raw", "docs/b.txt"}, dlStart}, {AssetKey{"demo-raw", "docs/c.txt"}, dlStart}}, DownloadFresh); err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	fillRaw(t, c, 1, "docs/a.txt", "docs/b.txt")
	if got := downloaded(t, c, "docs/a.txt"); !got.Equal(dlStart) {
		t.Fatalf("a rescan lost the time of a path that stays: %v", got)
	}
	fillRaw(t, c, 2, "docs/a.txt")
	if got := downloaded(t, c, "docs/a.txt"); !got.Equal(dlStart) {
		t.Fatalf("a rescan lost the time of a path whose blob changed: %v", got)
	}
	var left int
	if err := c.readers.QueryRow("SELECT count(*) FROM assets WHERE repo = 'demo-raw' AND path IN ('docs/b.txt', 'docs/c.txt')").Scan(&left); err != nil || left != 0 {
		t.Fatalf("paths that went: %d %v", left, err)
	}
}

// A file built new holds the time it was built; a rescan keeps it; a file
// of schema 4 takes the time of its migration as its build time, so that a
// usage policy selects no asset of it before the criterion age passed;
// the next open keeps it.
func TestTheBuildTimeOfTheFile(t *testing.T) {
	before := buildClock
	t.Cleanup(func() { buildClock = before })
	buildClock = func() time.Time { return dlStart }
	dir := filepath.Join(t.TempDir(), "data")
	c := mustOpen(t, dir)
	if built, err := c.Built(); err != nil || !built.Equal(dlStart) {
		t.Fatalf("built %v %v", built, err)
	}
	fillRaw(t, c, 1, "docs/a.txt")
	buildClock = func() time.Time { return dlStart.Add(time.Hour) }
	again := mustOpen(t, dir)
	if built, err := again.Built(); err != nil || !built.Equal(dlStart) {
		t.Fatalf("after a rescan and a second open: %v %v", built, err)
	}
	old := Path(filepath.Join(t.TempDir(), "old"))
	v4, err := openWith(old, steps[:4])
	if err != nil {
		t.Fatal(err)
	}
	if err := v4.Close(); err != nil {
		t.Fatal(err)
	}
	buildClock = func() time.Time { return dlStart.Add(2 * time.Hour) }
	migrated := mustOpen(t, filepath.Dir(old))
	if built, err := migrated.Built(); err != nil || !built.Equal(dlStart.Add(2*time.Hour)) {
		t.Fatalf("a file of schema 4 reads %v %v, want its migration time", built, err)
	}
	buildClock = func() time.Time { return dlStart.Add(3 * time.Hour) }
	if built, err := mustOpen(t, filepath.Dir(old)).Built(); err != nil || !built.Equal(dlStart.Add(2*time.Hour)) {
		t.Fatalf("a second open of the migrated file reads %v %v", built, err)
	}
}

// A batch that is being written stays visible to Pending until its write
// ends; a failed write keeps its notes for the next batch, a newer note of
// the same asset wins; a later write stores them.
func TestABatchInFlightStaysVisibleAndAFailedWriteKeepsItsNotes(t *testing.T) {
	release := make(chan error)
	entered := make(chan struct{}, 4)
	var mu sync.Mutex
	var written [][]DownloadTime
	write := func(times []DownloadTime) (int, map[AssetKey]time.Time, error) {
		entered <- struct{}{}
		if err := <-release; err != nil {
			return 0, nil, err
		}
		mu.Lock()
		written = append(written, times)
		mu.Unlock()
		stored := map[AssetKey]time.Time{}
		for _, d := range times {
			stored[d.AssetKey] = d.At
		}
		return len(times), stored, nil
	}
	d := NewDownloadTimes(write, time.Hour)
	d.Note("demo-raw", "docs/a.txt", dlStart)
	done := make(chan error)
	go func() { _, err := d.Flush(); done <- err }()
	<-entered
	if p := d.Pending("demo-raw"); !p["docs/a.txt"].Equal(dlStart) {
		t.Fatalf("a batch in flight is not pending: %v", p)
	}
	d.Note("demo-raw", "docs/b.txt", dlStart.Add(time.Minute))
	release <- errors.New("database is locked")
	if err := <-done; err == nil {
		t.Fatal("a failed write gave no error")
	}
	p := d.Pending("demo-raw")
	if len(p) != 2 || !p["docs/a.txt"].Equal(dlStart) || !p["docs/b.txt"].Equal(dlStart.Add(time.Minute)) {
		t.Fatalf("after a failed write %v", p)
	}
	go func() { _, err := d.Flush(); done <- err }()
	<-entered
	release <- nil
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(written) != 1 || len(written[0]) != 2 || len(d.Pending("demo-raw")) != 0 {
		t.Fatalf("written %v, pending %v", written, d.Pending("demo-raw"))
	}
}

// AssetRows give the assets of a repository with the store and creation
// time of the winning blob, the component and both download times.
func TestAssetRowsOfARepository(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	fillRaw(t, c, 1_700_000_000_000, "docs/a.txt", "docs/b.txt")
	if _, err := c.writer.Exec("INSERT INTO imported_assets (repo, path, blob_ref, last_downloaded_ms, checksums)" +
		" VALUES ('demo-raw', 'docs/b.txt', 'demo-ref', 1700000000500, '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.SetDownloaded([]DownloadTime{{AssetKey{"demo-raw", "docs/a.txt"}, dlStart}}, DownloadFresh); err != nil {
		t.Fatal(err)
	}
	rows, err := c.AssetRows("demo-raw")
	if err != nil || len(rows) != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	created := time.UnixMilli(1_700_000_000_000).UTC()
	a, b := rows[0], rows[1]
	if a.Path != "docs/a.txt" || a.Store != "demo-store" || !a.Created.Equal(created) || !a.Downloaded.Equal(dlStart) ||
		!a.ImportedDownloaded.IsZero() || a.Name == "" {
		t.Fatalf("a %+v", a)
	}
	if b.Path != "docs/b.txt" || !b.Downloaded.IsZero() || !b.ImportedDownloaded.Equal(time.UnixMilli(1_700_000_000_500).UTC()) {
		t.Fatalf("b %+v", b)
	}
	if none, err := c.AssetRows("demo-none"); err != nil || len(none) != 0 {
		t.Fatalf("%+v %v", none, err)
	}
}
