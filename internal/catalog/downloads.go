package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// The last download time of an asset (decision A19 of the phase 6 plan)
// and the build time of the file (ruling CL3 of leg 6-cleanup). A download
// sets assets.last_downloaded_ms through DownloadTimes: the notes of the
// read path wait in memory and are written every DownloadBatch, and a note
// is skipped while the stored value is younger than DownloadFresh. A rescan
// keeps the column for every path that stays (merger.place).

const (
	// DownloadBatch is how long a note waits before its batch is written.
	DownloadBatch = 10 * time.Second
	// DownloadFresh is the age below which a stored download time is kept.
	DownloadFresh = 12 * time.Hour

	// builtStep is the schema step that holds the table facts.
	builtStep = 5
	// maxKnown bounds the stored times DownloadTimes keeps in memory.
	maxKnown = 1 << 16
)

// buildClock is the clock of the build time of a new file; tests set it.
var buildClock = time.Now

// Built is the time the file was built new, in UTC; zero for a file built
// before schema step 5, which holds no such time.
func (c *Catalog) Built() (time.Time, error) {
	var ms int64
	err := c.read(func(tx *sql.Tx) error {
		return tx.QueryRowContext(bg, "SELECT value FROM facts WHERE key = 'built_ms'").Scan(&ms)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(ms).UTC(), nil
}

// AssetKey is an asset by repository and path.
type AssetKey struct{ Repo, Path string }

// DownloadTime is the time of a download of an asset.
type DownloadTime struct {
	AssetKey
	At time.Time
}

// SetDownloaded writes the download times of assets in one transaction: a
// time is written where the asset has none or one older than fresh before
// it, and skipped for an asset the file does not hold. written counts the
// rows written; stored is the value of each held asset after the write.
func (c *Catalog) SetDownloaded(times []DownloadTime, fresh time.Duration) (written int, stored map[AssetKey]time.Time, err error) {
	stored = map[AssetKey]time.Time{}
	if len(times) == 0 {
		return 0, stored, nil
	}
	err = c.write(func(tx *sql.Tx) error {
		for _, t := range times {
			var held sql.NullInt64
			err := tx.QueryRowContext(bg, "SELECT last_downloaded_ms FROM assets WHERE repo = ? AND path = ?", t.Repo, t.Path).Scan(&held)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			at := t.At.UnixMilli()
			if held.Valid && at-held.Int64 < fresh.Milliseconds() {
				stored[t.AssetKey] = time.UnixMilli(held.Int64).UTC()
				continue
			}
			if _, err := tx.ExecContext(bg, "UPDATE assets SET last_downloaded_ms = ? WHERE repo = ? AND path = ?", at, t.Repo, t.Path); err != nil {
				return err
			}
			written++
			stored[t.AssetKey] = time.UnixMilli(at).UTC()
		}
		return nil
	})
	if err != nil {
		return 0, map[AssetKey]time.Time{}, err
	}
	return written, stored, nil
}

// DownloadWriter writes a batch of download times; *Catalog.SetDownloaded
// with DownloadFresh is one.
type DownloadWriter func(times []DownloadTime) (written int, stored map[AssetKey]time.Time, err error)

// DownloadTimes batches the download notes of the read path (decision A19).
// It is safe for use by several goroutines.
type DownloadTimes struct {
	write DownloadWriter
	every time.Duration

	flushMu sync.Mutex // one batch write at a time
	mu      sync.Mutex
	pending map[AssetKey]time.Time
	writing map[AssetKey]time.Time // the batch being written, nil between batches
	known   map[AssetKey]time.Time // stored values this process wrote or read
	timer   *time.Timer
	closed  bool
}

// NewDownloadTimes writes its batches through write, each every after its
// first note; every 0 takes DownloadBatch.
func NewDownloadTimes(write DownloadWriter, every time.Duration) *DownloadTimes {
	if every <= 0 {
		every = DownloadBatch
	}
	return &DownloadTimes{write: write, every: every, pending: map[AssetKey]time.Time{}, known: map[AssetKey]time.Time{}}
}

// Note keeps a download of an asset at a time for the next batch. A note
// is skipped while the stored value it knows is younger than DownloadFresh,
// and after Close.
func (d *DownloadTimes) Note(repo, path string, at time.Time) {
	key := AssetKey{repo, path}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if stored, ok := d.known[key]; ok && at.Sub(stored) < DownloadFresh {
		return
	}
	if held, ok := d.pending[key]; ok && !at.After(held) {
		return
	}
	d.pending[key] = at
	if d.timer == nil {
		d.timer = time.AfterFunc(d.every, d.flushLater)
	}
}

// Pending are the download times of a repository that wait for their
// batch, by path.
func (d *DownloadTimes) Pending(repo string) map[string]time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]time.Time{}
	for _, notes := range []map[AssetKey]time.Time{d.writing, d.pending} {
		for key, at := range notes {
			if key.Repo == repo && at.After(out[key.Path]) {
				out[key.Path] = at
			}
		}
	}
	return out
}

// Flush writes the notes that wait and returns the rows written. The batch
// stays visible to Pending until its write ends; a failed write gives its
// notes back for the next batch, so that a use of an asset is not lost.
func (d *DownloadTimes) Flush() (int, error) {
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	d.mu.Lock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if len(d.pending) == 0 {
		d.mu.Unlock()
		return 0, nil
	}
	batch := make([]DownloadTime, 0, len(d.pending))
	for key, at := range d.pending {
		batch = append(batch, DownloadTime{AssetKey: key, At: at})
	}
	d.writing, d.pending = d.pending, map[AssetKey]time.Time{}
	d.mu.Unlock()
	written, stored, err := d.write(batch)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writing = nil
	if err != nil {
		for _, note := range batch {
			if held, ok := d.pending[note.AssetKey]; !ok || note.At.After(held) {
				d.pending[note.AssetKey] = note.At
			}
		}
		if !d.closed && d.timer == nil {
			d.timer = time.AfterFunc(d.every, d.flushLater)
		}
		return 0, err
	}
	if len(d.known)+len(stored) > maxKnown {
		d.known = map[AssetKey]time.Time{}
	}
	for key, at := range stored {
		d.known[key] = at
	}
	return written, nil
}

// flushLater is the batch the timer of the first note writes.
func (d *DownloadTimes) flushLater() {
	if _, err := d.Flush(); err != nil {
		slog.Warn(fmt.Sprintf("the last download times are not written: %v", err), "logger", loggerName)
	}
}

// Close writes the notes that wait; later notes are dropped.
func (d *DownloadTimes) Close() (int, error) {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return d.Flush()
}
