package server

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
)

// The state of leg 6-cleanup beside a server: the open catalog.sqlite that
// the cleanup and the last download times (decision A19) read and write,
// the batch of those times, and the clock of both. It lives beside the
// Server value, so that the server and its options keep their fields, and
// closeTasks closes it (task_host.go).

// cleanupSide is the state of one server.
type cleanupSide struct {
	dataDir   string
	mu        sync.Mutex
	cat       *catalog.Catalog // nil until the file exists and is opened
	closed    bool
	now       func() time.Time
	downloads *catalog.DownloadTimes
}

// sides holds the cleanupSide of each server.
var sides sync.Map // *Server -> *cleanupSide

// side is the state of s, made at its first use.
func (s *Server) side() *cleanupSide {
	if v, ok := sides.Load(s); ok {
		return v.(*cleanupSide)
	}
	c := &cleanupSide{dataDir: s.rp.cfg.DataDir, now: time.Now}
	c.downloads = catalog.NewDownloadTimes(c.writeDownloads, 0)
	v, _ := sides.LoadOrStore(s, c)
	return v.(*cleanupSide)
}

// closeSide writes the download times that wait and closes the catalog.
func (s *Server) closeSide() {
	v, ok := sides.LoadAndDelete(s)
	if !ok {
		return
	}
	c := v.(*cleanupSide)
	if _, err := c.downloads.Close(); err != nil {
		logAt(slogWarn, serverLogger, "the last download times are not written: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.cat != nil {
		_ = c.cat.Close() // every write returned before
		c.cat = nil
	}
}

// clock is the clock of the cleanup and of the download times.
func (c *cleanupSide) clock() time.Time {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	return now()
}

// setClock sets the clock; tests move it.
func (c *cleanupSide) setClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// catalog is the open catalog file, nil while the file does not exist.
func (c *cleanupSide) catalog() (*catalog.Catalog, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cat != nil || c.closed {
		return c.cat, nil
	}
	if _, err := os.Stat(catalog.Path(c.dataDir)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	cat, err := catalog.Open(c.dataDir)
	if err != nil {
		return nil, err
	}
	c.cat = cat
	return cat, nil
}

// writeDownloads is the writer of the batch: without the catalog file no
// asset has a row, so nothing is written.
func (c *cleanupSide) writeDownloads(times []catalog.DownloadTime) (int, map[catalog.AssetKey]time.Time, error) {
	cat, err := c.catalog()
	if err != nil || cat == nil {
		return 0, map[catalog.AssetKey]time.Time{}, err
	}
	return cat.SetDownloaded(times, catalog.DownloadFresh)
}

// noteDownload keeps a download of a stored file for the batch.
func (s *Server) noteDownload(repo, path string) {
	c := s.side()
	c.downloads.Note(repo, path, c.clock())
}
