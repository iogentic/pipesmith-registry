package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// writeStoresDir holds the write store of the data directory (W1 of the
// phase 4 plan): <data>/stores/local-write.
const writeStoresDir = "stores"

// writeStoreRoot is the root of the write store of a data directory.
func writeStoreRoot(dataDir string) string {
	return filepath.Join(dataDir, writeStoresDir, repos.DefaultWriteStore)
}

// writePath is the write state of one server: the writer of the write
// store, nil while writes and upstream are off, and what the ingest needs
// beside it.
type writePath struct {
	writer *blobwrite.Writer
	// clients is the switch writes: client writes are on. With upstream on
	// and writes off the writer is open for the proxies and client writes
	// keep every answer of writes off (decision P5-2 of the phase 5 session).
	clients   bool
	store     *stores.BlobStore                // the write store, attached with writes on and off; nil for none
	catalog   CatalogHook                      // Options.Catalog
	file      *catalog.Catalog                 // Options.CatalogFile, which the scans of the write store fill; nil: each scan opens it
	freeSpace func(dir string) (uint64, error) // the free bytes of a file system; tests replace it
	locks     pathLocks                        // one write of a path of a repository at a time
	space     spaceHeld                        // the bytes the bodies in flight hold of the free space (checkSpace)
	marks     marks                            // the delete marks (marks.go)
	deleted   deletedTimes                     // the creation times of deleted blobs (marks.go)
	metadata  metadataGate                     // a metadata rebuild against a deploy of its metadata (write_rebuild.go)
	// late is the write store that a server without writes attached after
	// its start (readPath.attachLate), so that a scan of it keeps the rule
	// of its write order.
	late atomic.Pointer[stores.BlobStore]
	// created are the writers of the created stores (writestores.go, SR1).
	created createdStores
	// onMark is told the store and the size of each delete mark set
	// (markedcount.go, SR15); nil for none.
	onMark func(store string, size int64)
}

// lateStore gives the write store once it exists, for a server without
// writes that found none at its start (readPath.attachLate); nil while it
// does not exist and while a store of config.json has its name.
func (wp *writePath) lateStore(cfg *config.Config) func() stores.Store {
	return func() stores.Store {
		s, err := ExistingWriteStore(cfg)
		if err != nil || s == nil {
			return nil
		}
		wp.late.Store(s)
		logAt(slogInfo, serverLogger, "store %s appeared after the start; it is attached read only", s.Name())
		return s
	}
}

// openWrites is the write path of New, and the write store to attach beside
// the stores of config.json, nil for none. Writes need security (decision
// E4): without it every caller is anonymous.
func openWrites(cfg *config.Config, opts Options) (*writePath, stores.Store, error) {
	if opts.Writes && opts.Security == nil {
		return nil, nil, errors.New("writes need state.sqlite: without security every caller is anonymous, and an anonymous write is never allowed (decision E4)")
	}
	if opts.Upstream && opts.Security == nil {
		return nil, nil, errors.New("upstream needs state.sqlite: without security every caller is anonymous, and an anonymous request would make the proxies ask their remotes and write blobs (decision P5-2)")
	}
	switched := "writes are"
	if !opts.Writes {
		switched = "upstream is"
	}
	store, writer, err := openWriteStore(cfg, opts.Writes || opts.Upstream, switched, opts.writeOptions)
	if err != nil {
		return nil, nil, err
	}
	wp := &writePath{writer: writer, clients: opts.Writes, store: store, catalog: opts.Catalog, file: opts.CatalogFile, freeSpace: freeBytes}
	wp.created.opts = opts.writeOptions
	wp.marks.fault = opts.marksFault
	if store == nil {
		return wp, nil, nil
	}
	return wp, store, nil
}

// accessWrite is W_OK of access(2), the same on Linux and macOS.
const accessWrite = 0x2

// writable reports whether the write store takes a write now, for GET
// /v1/status/writable (task 15 of the phase 4 plan): writes are on, the
// directories a write creates files in may be written (content/tmp,
// content and reconciliation), and no delete mark of a replaced blob waits
// once each is tried again (marks.go), at most once per retryEvery.
func (wp *writePath) writable() bool {
	return wp.writableBy(syscall.Access) && wp.retryDue() == 0
}

// writableBy is writable with the access check of the file system given;
// tests give one that refuses, because root writes in spite of the modes.
func (wp *writePath) writableBy(access func(path string, mode uint32) error) bool {
	if wp.writer == nil || !wp.clients {
		return false
	}
	for _, o := range wp.writers() {
		for _, dir := range writeDirsOf(o.writer) {
			if err := access(dir, accessWrite); err != nil {
				return false
			}
		}
	}
	return true
}

// writeDirs are the directories of local-write a write creates files in.
func (wp *writePath) writeDirs() []string { return writeDirsOf(wp.writer) }

// writeDirsOf are the directories of the store of w a write creates files
// in; every open write store is asked (SR3).
func writeDirsOf(w *blobwrite.Writer) []string {
	root := w.Root()
	return []string{filepath.Join(root, stores.ContentDir, blobwrite.TmpDir), filepath.Join(root, stores.ContentDir),
		filepath.Join(root, blobwrite.ReconciliationDir)}
}

// close gives the locks of the write stores back.
func (wp *writePath) close() error {
	if wp.writer == nil {
		return nil
	}
	return errors.Join(wp.created.closeAll(), wp.writer.Close())
}

// openWriteStore gives the write store of the data directory. With writes
// or upstream on it is made in the layout of the old product when it is
// missing and opened by its one writer, which holds the lock of W12. With
// both off a write store that exists is attached read only, so that what
// was written earlier is served; one that does not exist is not made. A
// store of config.json with the same name keeps its place while both are off
// and stops a start with either on; switched names the switch for the
// refusal.
func openWriteStore(cfg *config.Config, on bool, switched string, opts blobwrite.Options) (*stores.BlobStore, *blobwrite.Writer, error) {
	name, root := repos.DefaultWriteStore, writeStoreRoot(cfg.DataDir)
	if !on {
		s, err := ExistingWriteStore(cfg)
		return s, nil, err
	}
	if slices.ContainsFunc(cfg.Stores, func(sc config.StoreConfig) bool { return sc.Name == name }) {
		return nil, nil, fmt.Errorf("store %s of %s has the name of the write store of this server; remove it before %s switched on",
			name, cfg.ConfigPath(), switched)
	}
	if err := blobwrite.Create(root, time.Now()); err != nil {
		return nil, nil, err
	}
	s, err := stores.NewWriteStore(name, root)
	if err != nil {
		return nil, nil, err
	}
	w, err := blobwrite.Open(s, opts)
	if err != nil {
		return nil, nil, err
	}
	return s, w, nil
}

// ExistingWriteStore is the write store of a data directory, read only, when
// its folder exists and no store of config.json has its name: what a start
// with writes off attaches, and what the command scan takes in beside the
// stores of config.json (finding S1 of review 4). nil otherwise; a store of
// config.json with the name is scanned as the other stores of config.json.
func ExistingWriteStore(cfg *config.Config) (*stores.BlobStore, error) {
	name, root := repos.DefaultWriteStore, writeStoreRoot(cfg.DataDir)
	if slices.ContainsFunc(cfg.Stores, func(sc config.StoreConfig) bool { return sc.Name == name }) {
		return nil, nil
	}
	info, err := os.Stat(filepath.Join(root, stores.ContentDir))
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	return stores.NewBlobStore(name, root)
}

// repairAtStart scans the write store at the start of a server whose writer
// is open, before the server takes a request (Repair at start, plan 0.2.0).
// The scan of the write store serves the blob of each path that was written
// last and gives every older live blob of the path its delete mark: a kill
// between the row of a write and the mark of the blob it replaced, or a mark
// that waited in the memory of the process before (marks.go), leaves such a
// blob. A mark that fails here waits as every other one.
//
// An index that does not know the store while the store holds blobs was
// lost and built again from the stores of config.json alone: every written
// path would answer 404, and ALLOW_ONCE would take a second write of it
// (finding S1 of review 4). A failed scan then stops the start. When the
// index knows the store, a failed scan is logged and the start goes on with
// the rows the index holds; the next scan of the store repairs it.
func (wp *writePath) repairAtStart(ix *index.Index) error {
	stats, err := ix.StoreStats()
	if err != nil {
		return err
	}
	for _, o := range wp.writers() {
		if err := wp.repairStore(ix, stats, o); err != nil {
			return err
		}
	}
	return nil
}

// repairStore is repairAtStart for one write store: local-write or a
// created store (SR3).
func (wp *writePath) repairStore(ix *index.Index, stats []index.StoreStat, o openStore) error {
	name := o.writer.Name()
	known := slices.ContainsFunc(stats, func(stat index.StoreStat) bool { return stat.Store == name })
	if !holdsBlobs(o.writer.Root()) {
		return nil
	}
	if !known {
		logAt(slogInfo, serverLogger, "the index holds no row of store %s; the store is scanned before writes start", name)
	}
	result, err := wp.scanWith(o.store, o.writer)(o.store, ix, nil)
	if err != nil {
		if !known {
			return fmt.Errorf("the scan of store %s before writes start failed: %w", name, err)
		}
		logAt(slogWarn, serverLogger, "the scan of store %s before writes start failed: %v; its live blobs are repaired by the next scan of it",
			name, err)
		return nil
	}
	logAt(slogInfo, serverLogger, "scan %s: live=%d deleted=%d copies=%d no_bytes=%d unparsable=%d seconds=%.2f", result.Store, result.Live,
		result.Deleted, result.Copies, result.NoBytes, result.Unparsable, result.Seconds)
	return nil
}

// holdsBlobs reports a store whose content/ holds more than the folder of
// the work files.
func holdsBlobs(root string) bool {
	entries, err := os.ReadDir(filepath.Join(root, stores.ContentDir))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != blobwrite.TmpDir {
			return true
		}
	}
	return false
}
