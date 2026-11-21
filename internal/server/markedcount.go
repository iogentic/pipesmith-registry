package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The count of the blobs with the delete mark of each store, for GET
// /api/blob-stores/{name}/deleted (rulings SR12 to SR16 of leg 6-stores-b).
// The count is kept in store_marks of catalog.sqlite: a scan writes it, the
// delete marks of the write path and the compaction move it by deltas,
// written right after their change, and
// a request reads it at once. A store without a row takes, once, the seed
// file of the data directory (a store of the old product only) or a walk
// of its properties files in the background, whose result is written as its
// row. A request waits for such a walk at most markedWait. One walk per
// store at a time, at most markedWalks over all stores; a failed walk is
// tried again after markedRetry. Without catalog.sqlite the count of a walk
// is kept in memory and moved by the deltas there.

const (
	markedRetry = 30 * time.Second // the pause after a failed walk before the next (review H1)
	markedWait  = 2 * time.Second
	markedWalks = 2
	markedFlush = 500 * time.Millisecond // how often deltas whose write failed are tried again
	// SeedFile is the file of counts made before the start (count_marked.py
	// of the host run book), read for a store of the old product without a
	// row.
	SeedFile = "marked-counts.json"
	// MissingFolderReason is the reason of a count that a store without
	// its folder has not, as on a disk that is not mounted (ruling SC1).
	MissingFolderReason = "the folder of the store does not exist"
)

// errMissingFolder is the failure of a walk of a store whose folder does
// not exist; the request gets MissingFolderReason for it (SC1).
var errMissingFolder = errors.New("the folder of the store does not exist")

// markedResult is one count: the marked blobs, their bytes and the time of
// the count.
type markedResult struct {
	Count, Bytes int64
	At           time.Time
}

// markedAnswer is what a request gets: the count when there is one, and
// whether a walk runs; err is the failure of a walk when there is no count
// to give, reason the cause of a missing count that is no failure of the
// server (SC1).
type markedAnswer struct {
	result         markedResult
	have, counting bool
	err            error
	reason         string
}

// markedEntry is the walk state of one store without a row.
type markedEntry struct {
	root     string
	have     bool         // a walk ended; without the catalog result is the count, with it the row
	result   markedResult // the count of the last walk, moved by the deltas while have
	err      error
	running  chan struct{}      // closed when the walk ends; nil while none runs
	failedAt time.Time          // the end of the last walk that failed
	cancel   context.CancelFunc // stops the running walk
	during   markedResult       // the deltas that came while the walk ran, added to its count (review P4)
}

// markedCounter keeps the counts of every store.
type markedCounter struct {
	flushMu  sync.Mutex // one flush at a time, so that inflight holds the deltas of that one
	mu       sync.Mutex
	entries  map[string]*markedEntry
	pending  map[string]markedResult // deltas not yet written into store_marks
	inflight map[string]markedResult // deltas a flush writes now (review P7)
	refused  map[string]bool         // stores whose seed entry was refused, read once (review P8)
	slots    chan struct{}
	ctx      context.Context
	stop     context.CancelFunc
	flushed  chan struct{} // closed when the flush loop ended
	wait     time.Duration
	walk     func(ctx context.Context, name, root string) (markedResult, error)
	now      func() time.Time
	// rows gives catalog.sqlite, nil when there is none; seed gives the
	// entry of the seed file for a store and its root; own reports a store
	// of this server, which takes no seed. Each may be nil.
	rows func() (*catalog.Catalog, error)
	seed func(name, root string) (catalog.Marks, bool)
	own  func(name string) bool
}

func newMarkedCounter() *markedCounter {
	ctx, stop := context.WithCancel(context.Background())
	c := &markedCounter{entries: map[string]*markedEntry{}, pending: map[string]markedResult{}, inflight: map[string]markedResult{},
		refused: map[string]bool{}, slots: make(chan struct{}, markedWalks),
		ctx: ctx, stop: stop, flushed: make(chan struct{}), wait: markedWait, walk: walkMarked, now: time.Now}
	go c.flushLoop()
	return c
}

// delta moves the count of store by blobs and bytes: a delete mark set,
// or a marked blob removed (SR15). It is written right after the change
// that made it, so that no later request sees the catalog change; a delta
// that fails to be written waits and is tried again every markedFlush.
func (c *markedCounter) delta(store string, blobs, bytes int64) {
	c.mu.Lock()
	if e := c.entries[store]; e != nil && e.running != nil {
		e.during.Count += blobs // the walk writes the row; the delta follows it (review P4)
		e.during.Bytes += bytes
		c.mu.Unlock()
		return
	}
	d := c.pending[store]
	d.Count += blobs
	d.Bytes += bytes
	c.pending[store] = d
	c.mu.Unlock()
	_ = c.flush() // a failure is logged and the delta waits for the next try
}

func (c *markedCounter) flushLoop() {
	defer close(c.flushed)
	tick := time.NewTicker(markedFlush)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			c.flush()
		case <-c.ctx.Done():
			return
		}
	}
}

// flush writes the deltas into store_marks; without the catalog it moves
// the counts kept in memory. A failed write keeps the deltas for the next.
func (c *markedCounter) flush() error {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return nil
	}
	pending := c.pending
	c.pending, c.inflight = map[string]markedResult{}, pending
	c.mu.Unlock()
	cat := c.catalog()
	if cat == nil {
		c.mu.Lock()
		for store, d := range pending {
			if e := c.entries[store]; e != nil && e.have {
				e.result.Count = max(e.result.Count+d.Count, 0)
				e.result.Bytes = max(e.result.Bytes+d.Bytes, 0)
			}
		}
		c.inflight = map[string]markedResult{}
		c.mu.Unlock()
		return nil
	}
	deltas := make(map[string]catalog.Marks, len(pending))
	for store, d := range pending {
		deltas[store] = catalog.Marks{Blobs: d.Count, Bytes: d.Bytes}
	}
	err := cat.AddStoreMarks(deltas)
	c.mu.Lock()
	c.inflight = map[string]markedResult{}
	if err != nil {
		for store, d := range pending {
			p := c.pending[store]
			p.Count += d.Count
			p.Bytes += d.Bytes
			c.pending[store] = p
		}
	}
	c.mu.Unlock()
	if err != nil {
		logAt(slogWarn, serverLogger, "the counts of the marked blobs are not written: %v; they are tried again", err)
	}
	return err
}

// close stops every walk and writes the deltas that wait, a second time
// when the first write fails (review P6).
func (c *markedCounter) close() {
	c.stop()
	<-c.flushed
	if c.flush() != nil {
		time.Sleep(markedFlush)
		if err := c.flush(); err != nil {
			logAt(slogWarn, serverLogger, "the counts of the marked blobs lost deltas at the close: %v; the next scan of each store counts again", err)
		}
	}
}

// catalog is catalog.sqlite, nil when there is none or it cannot be read.
func (c *markedCounter) catalog() *catalog.Catalog {
	if c.rows == nil {
		return nil
	}
	cat, err := c.rows()
	if err != nil {
		logAt(slogWarn, serverLogger, "the counts of the marked blobs cannot read the catalog: %v", err)
		return nil
	}
	return cat
}

// get gives the count of the store name at root: its row with the deltas
// that wait, else the seed, else a walk, for which it waits at most c.wait.
func (c *markedCounter) get(name, root string) markedAnswer {
	cat := c.catalog()
	if cat != nil {
		m, found, err := cat.StoreMarks(name)
		if err != nil {
			return markedAnswer{err: err}
		}
		if found {
			return c.fromRow(name, m)
		}
		c.mu.Lock()
		refused := c.refused[name]
		c.mu.Unlock()
		if c.seed != nil && !refused && (c.own == nil || !c.own(name)) {
			m, ok := c.seed(name, root)
			if !ok {
				c.mu.Lock()
				c.refused[name] = true // the file is read once per store (review P8)
				c.mu.Unlock()
			}
			if ok {
				if err := cat.SetStoreMarks(name, m); err != nil {
					return markedAnswer{err: err}
				}
				logAt(slogInfo, serverLogger, "the count of the marked blobs of store %s is taken from %s: %d blobs, %d bytes, counted %s",
					name, SeedFile, m.Blobs, m.Bytes, m.Counted.UTC().Format(time.RFC3339))
				return c.fromRow(name, m)
			}
		}
	}
	c.mu.Lock()
	e := c.entries[name]
	if e == nil || e.root != root {
		if e != nil && e.cancel != nil {
			e.cancel() // a new root: the walk of the old one stops (review H6)
		}
		e = &markedEntry{root: root}
		c.entries[name] = e
	}
	if e.have && e.running == nil {
		c.mu.Unlock()
		if cat != nil {
			// the walk ended after the read above and wrote the row: no second walk
			m, found, err := cat.StoreMarks(name)
			if err == nil && found {
				return c.fromRow(name, m)
			}
			return markedAnswer{err: errors.Join(err, errors.New("the count of the walk is not in the catalog"))}
		}
		c.mu.Lock()
		answer := markedAnswer{result: c.withPending(name, e.result), have: true}
		c.mu.Unlock()
		return answer
	}
	resting := e.err != nil && c.now().Sub(e.failedAt) < markedRetry
	if e.running == nil && !resting && c.ctx.Err() == nil {
		c.start(name, e, cat)
	}
	done := e.running
	c.mu.Unlock()
	if done != nil {
		timer := time.NewTimer(c.wait)
		select {
		case <-done:
		case <-timer.C:
		case <-c.ctx.Done():
		}
		timer.Stop()
	}
	if cat != nil {
		if m, found, err := cat.StoreMarks(name); err == nil && found {
			return c.fromRow(name, m)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	answer := markedAnswer{result: c.withPending(name, e.result), have: e.have, counting: e.running != nil}
	if !e.have && !answer.counting {
		if errors.Is(e.err, errMissingFolder) {
			answer.reason = MissingFolderReason
		} else {
			answer.err = e.err
		}
	}
	return answer
}

// fromRow is the answer of a row of store_marks with the deltas that wait.
func (c *markedCounter) fromRow(name string, m catalog.Marks) markedAnswer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return markedAnswer{result: c.withPending(name, markedResult{Count: m.Blobs, Bytes: m.Bytes, At: m.Counted}), have: true}
}

// withPending is r with the deltas of name that wait; the caller holds c.mu.
func (c *markedCounter) withPending(name string, r markedResult) markedResult {
	d, f := c.pending[name], c.inflight[name]
	r.Count = max(r.Count+d.Count+f.Count, 0)
	r.Bytes = max(r.Bytes+d.Bytes+f.Bytes, 0)
	return r
}

// start runs the walk of e in the background and writes its count as the
// row of the store, or keeps it in e without the catalog; the caller holds
// c.mu. The deltas of the store that waited are dropped: the walk counts
// the marks they stand for.
func (c *markedCounter) start(name string, e *markedEntry, cat *catalog.Catalog) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(c.ctx)
	e.running, e.cancel, e.during = done, cancel, markedResult{}
	delete(c.pending, name)
	root := e.root
	go func() {
		defer cancel()
		var result markedResult
		var err error
		select {
		case c.slots <- struct{}{}:
			result, err = c.walk(ctx, name, root)
			<-c.slots
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err == nil {
			result.At = c.now()
		}
		c.mu.Lock()
		if err == nil {
			// the deltas that came during the walk follow its count; one that
			// the walk saw as well counts twice, as for a scan (SR15)
			result.Count = max(result.Count+e.during.Count, 0)
			result.Bytes = max(result.Bytes+e.during.Bytes, 0)
			e.during = markedResult{}
			if cat != nil {
				err = cat.SetStoreMarks(name, catalog.Marks{Blobs: result.Count, Bytes: result.Bytes, Counted: result.At})
			}
		}
		if err == nil {
			e.err, e.have, e.result = nil, true, result // with the catalog the row holds the count; e.result is not read then
		} else {
			e.err, e.failedAt = err, c.now()
			if !errors.Is(err, context.Canceled) {
				logAt(slogWarn, serverLogger, "the count of the marked blobs of store %s failed: %v", name, err)
			}
		}
		e.running, e.cancel = nil, nil
		c.mu.Unlock()
		close(done)
	}()
}

// seedEntry is one store of the seed file.
type seedEntry struct {
	Blobs      *int64 `json:"blobs"`
	Bytes      *int64 `json:"bytes"`
	Unreadable *int64 `json:"unreadable"`
	CountedAt  string `json:"countedAt"`
	Root       string `json:"root"`
}

// readSeed gives the entry of the store name of the seed file of dataDir
// when its root is the root of the store, every file was read, and the
// entry is complete (the order of 2026-11-21); false otherwise, with a log
// line for a file or an entry that is refused.
func readSeed(dataDir, name, root string) (catalog.Marks, bool) {
	data, err := os.ReadFile(filepath.Join(dataDir, SeedFile)) // #nosec G304 -- a fixed name in the data directory.
	if errors.Is(err, fs.ErrNotExist) {
		return catalog.Marks{}, false
	}
	var all map[string]seedEntry
	if err == nil {
		err = json.Unmarshal(data, &all)
	}
	if err != nil {
		logAt(slogWarn, serverLogger, "%s is not read: %v", SeedFile, err)
		return catalog.Marks{}, false
	}
	e, ok := all[name]
	if !ok {
		return catalog.Marks{}, false
	}
	at, err := time.Parse(time.RFC3339, e.CountedAt)
	switch {
	case err != nil || e.Blobs == nil || e.Bytes == nil || e.Unreadable == nil:
		logAt(slogWarn, serverLogger, "the entry of store %s in %s is incomplete; the store is counted by a walk", name, SeedFile)
		return catalog.Marks{}, false
	case !sameRoot(e.Root, root):
		logAt(slogWarn, serverLogger, "the entry of store %s in %s names another folder than the store; the store is counted by a walk", name, SeedFile)
		return catalog.Marks{}, false
	case *e.Unreadable != 0:
		logAt(slogWarn, serverLogger, "the entry of store %s in %s has %d unreadable files; the store is counted by a walk", name, SeedFile, *e.Unreadable)
		return catalog.Marks{}, false
	}
	return catalog.Marks{Blobs: *e.Blobs, Bytes: *e.Bytes, Counted: at}, true
}

// sameRoot compares two folders after Clean and, where both resolve, with
// their links resolved.
func sameRoot(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// walkMarked counts the blobs of the file store at root that carry the
// delete mark, by a walk of its properties files; it stops when ctx ends.
func walkMarked(ctx context.Context, name, root string) (markedResult, error) {
	var r markedResult
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return r, fmt.Errorf("%w: store %s: %w", errMissingFolder, name, err) // a folder that is missing, as on a disk not mounted, has no count (review H5, SC1)
	} else if err != nil {
		return r, fmt.Errorf("the folder of store %s: %w", name, err)
	}
	if _, err := os.Stat(filepath.Join(root, stores.ContentDir)); errors.Is(err, fs.ErrNotExist) {
		return r, nil // a store that holds no blob yet
	}
	store, err := stores.NewBlobStore(name, root)
	if err != nil {
		return r, err
	}
	seen := 0
	check := func() error {
		seen++
		if seen%1024 == 0 {
			return ctx.Err()
		}
		return nil
	}
	err = store.Walk(nil, func(stores.Entry) error { return check() }, func(m stores.Marked) error {
		r.Count++
		r.Bytes += m.Size
		return check()
	})
	return r, err
}
