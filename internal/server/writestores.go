package server

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// One writer per write store (ruling SR1 of leg 6-stores-b). local-write
// keeps its writer in writePath.writer, opened at the start as before. A
// created store, a record of state.sqlite whose path lies below
// <data>/stores/ (adminrules.WritableStore), gets its own writer on first
// use, with the layout of the old product and the lock of W12 of its own
// folder. A hosted repository writes into the store its write_store names
// (writeSettingsOf). Every imported store stays read only.

// errNoWriteStore is the error of a store name that is no write store.
var errNoWriteStore = errors.New("no write store")

// openStore is a write store with its writer.
type openStore struct {
	store  *stores.BlobStore
	writer *blobwrite.Writer
}

// createdStores are the writers of the created stores that are open.
type createdStores struct {
	mu   sync.Mutex
	open map[string]openStore
	// gen counts the stores opened and closed, so that the read path
	// attaches a store a writer opened after its last take over.
	gen atomic.Uint64
	// find gives the root of the created store name, false for a name that
	// is no write store now; nil finds none.
	find func(name string) (string, bool)
	opts blobwrite.Options
	// read are the created stores attached read only (readCreated): every
	// one at a start with writes and upstream off, and one that did not open
	// at a start with writes on (review R9).
	read []stores.Store
	// dataDir is the data directory of the server.
	dataDir string
}

// list are the created stores to attach: the open ones, then the read only
// ones whose name no open store has.
func (c *createdStores) list() []stores.Store {
	var out []stores.Store
	names := map[string]bool{}
	for _, o := range c.all() {
		out = append(out, o.store)
		names[o.store.Name()] = true
	}
	for _, r := range c.read {
		if !names[r.Name()] {
			out = append(out, r)
		}
	}
	return out
}

// readOnly reports a created store attached read only.
func (c *createdStores) readOnly(b *stores.BlobStore) bool {
	return slices.ContainsFunc(c.read, func(s stores.Store) bool { return s == stores.Store(b) })
}

// findCreated is the find of the created stores of a server: the record of
// the name in the state, a write store by adminrules.WritableStore with the
// stores config.json attaches now.
func findCreated(cfg *config.Config, snapshot func() *model.Snapshot, attached func() []string) func(name string) (string, bool) {
	return func(name string) (string, bool) {
		if snapshot == nil {
			return "", false
		}
		snap := snapshot()
		if snap == nil {
			return "", false
		}
		for _, b := range snap.BlobStores {
			if b.Name == name && adminrules.WritableStore(cfg.DataDir, b, attached(), snap.BlobStores) {
				return b.Path, true
			}
		}
		return "", false
	}
}

// writerOf is the writer of the write store name (SR2): local-write, or a
// created store, opened on first use. A name that is no write store, or a
// store whose folder cannot be opened, is the error.
func (wp *writePath) writerOf(name string) (*blobwrite.Writer, error) {
	if wp.writer == nil {
		return nil, errors.New("writes are off")
	}
	if name == wp.writer.Name() {
		return wp.writer, nil
	}
	o, err := wp.created.get(name, true)
	if err != nil {
		return nil, err
	}
	return o.writer, nil
}

// markWriter is the writer that sets a delete mark in the store name: an
// open one, also of a store whose record went since, else as writerOf.
func (wp *writePath) markWriter(name string) (*blobwrite.Writer, error) {
	if wp.writer == nil {
		return nil, errors.New("writes are off")
	}
	if name == "" || name == wp.writer.Name() {
		return wp.writer, nil
	}
	o, err := wp.created.get(name, false)
	if err != nil {
		return nil, err
	}
	return o.writer, nil
}

// isWriteStore reports whether name is local-write or a created store with
// an open writer: a row of it is the content of this server.
func (wp *writePath) isWriteStore(name string) bool {
	if name == "" {
		return false
	}
	if name == wp.writeStoreName() {
		return true
	}
	wp.created.mu.Lock()
	defer wp.created.mu.Unlock()
	_, open := wp.created.open[name]
	return open
}

// ownStore reports a store whose blobs this server wrote: local-write, a
// created store with an open writer, or one whose record makes it a write
// store, also with writes off.
func (wp *writePath) ownStore(name string) bool {
	if wp.isWriteStore(name) {
		return true
	}
	if wp.created.find == nil {
		return false
	}
	_, ok := wp.created.find(name)
	return ok
}

// writeStoreName is the name of local-write, also with writes off.
func (wp *writePath) writeStoreName() string {
	if wp.writer != nil {
		return wp.writer.Name()
	}
	return repos.DefaultWriteStore
}

// get gives the open store name, opening it when it is a write store.
// checked asks find also for an open store, so that a store whose record
// went takes no new write.
func (c *createdStores) get(name string, checked bool) (openStore, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, open := c.open[name]
	if open && !checked {
		return o, nil
	}
	if c.find == nil {
		return openStore{}, fmt.Errorf("%w: %s", errNoWriteStore, name)
	}
	root, ok := c.find(name)
	switch {
	case !ok:
		return openStore{}, fmt.Errorf("%w: %s", errNoWriteStore, name)
	case open:
		return o, nil
	}
	if err := blobwrite.Create(root, time.Now()); err != nil {
		return openStore{}, err
	}
	s, err := stores.NewWriteStore(name, root)
	if err != nil {
		return openStore{}, err
	}
	w, err := blobwrite.Open(s, c.opts)
	if err != nil {
		return openStore{}, err
	}
	if c.open == nil {
		c.open = map[string]openStore{}
	}
	o = openStore{store: s, writer: w}
	c.open[name] = o
	c.gen.Add(1)
	logAt(slogInfo, serverLogger, "store %s is open for writes at %s", name, root)
	return o, nil
}

// all are the open created stores, sorted by name.
func (c *createdStores) all() []openStore {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]openStore, 0, len(c.open))
	for _, name := range slices.Sorted(maps.Keys(c.open)) {
		out = append(out, c.open[name])
	}
	return out
}

// release closes the writer of the created store name, for its delete
// (SR8); a store without one is left alone.
func (c *createdStores) release(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, open := c.open[name]
	if !open {
		return nil
	}
	delete(c.open, name)
	c.gen.Add(1)
	return o.writer.Close()
}

// releaseStore is the release of a deleted created store (SR8, review R2,
// R8): its writer closes and the marks that wait in it go, since no writer
// can set them any more; the store keeps the blobs live, and a scan of the
// folder, if it is created again, finds them. A failure is logged: the
// record is gone already.
func (wp *writePath) releaseStore(name string) error {
	if err := wp.created.release(name); err != nil {
		logAt(slogWarn, serverLogger, "the writer of the deleted store %s did not close: %v", name, err)
	}
	if n := wp.dropWaiting(name); n > 0 {
		logAt(slogWarn, serverLogger, "%d delete marks that waited in the deleted store %s are dropped; those blobs stay live in its folder", n, name)
	}
	return nil
}

// closeAll closes every writer of a created store.
func (c *createdStores) closeAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for name, o := range c.open {
		errs = append(errs, o.writer.Close())
		delete(c.open, name)
	}
	c.gen.Add(1)
	return errors.Join(errs...)
}

// writers are the open writers: local-write first, then the created stores.
func (wp *writePath) writers() []openStore {
	if wp.writer == nil {
		return nil
	}
	return append([]openStore{{store: wp.store, writer: wp.writer}}, wp.created.all()...)
}

// openCreated opens the writer of every created store at a start with
// writes or upstream on, so that its content is served and scanned like the
// one of local-write (SR3). A store that does not open is logged and keeps
// no writer; a write into it is refused.
func (wp *writePath) openCreated(snap *model.Snapshot) {
	if wp.writer == nil || snap == nil {
		return
	}
	for _, b := range snap.BlobStores {
		if _, ok := wp.created.find(b.Name); !ok {
			continue
		}
		if _, err := wp.created.get(b.Name, true); err != nil {
			logAt(slogWarn, serverLogger, "store %s is not open for writes: %v; its files are served read only", b.Name, err)
			wp.created.read = append(wp.created.read, readCreated(wp.created.dataDir, &model.Snapshot{BlobStores: []model.BlobStore{b}}, nil)...) // review R9
		}
	}
}

// readCreated are the created stores with content, read only, for a start
// with writes and upstream off: what was written into them is served as the
// content of local-write is (ExistingWriteStore).
func readCreated(dataDir string, snap *model.Snapshot, attached []string) []stores.Store {
	if snap == nil {
		return nil
	}
	var out []stores.Store
	for _, b := range snap.BlobStores {
		if !adminrules.WritableStore(dataDir, b, attached, snap.BlobStores) {
			continue
		}
		info, err := os.Stat(filepath.Join(b.Path, stores.ContentDir))
		if err != nil || !info.IsDir() {
			continue
		}
		s, err := stores.NewBlobStore(b.Name, b.Path)
		if err != nil {
			logAt(slogWarn, serverLogger, "store %s is not attached: %v", b.Name, err)
			continue
		}
		out = append(out, s)
	}
	return out
}
