package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/cleanup"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The compaction of a store of this server (task 12 of the phase 6 plan,
// decision A8, rulings CL4, CL5, CL9, CL10 and CL12 of leg 6-cleanup): a
// blob whose delete mark is older than the setting is removed in a unit of
// work of its own, with the catalog row that still describes it; a store
// of the old product is refused.

// CompactCounts are the counts of a compaction (ruling CL10): the blobs
// removed with their bytes, those kept because their mark is younger than
// the setting, and those skipped because a write holds their path or
// their mark cannot be read.
type CompactCounts struct {
	Removed, Kept, Skipped int
	Bytes                  int64
}

// MarkedBlob is a blob with the delete mark of a store (ruling CL12).
type MarkedBlob struct {
	Repository, Path, Reason string
	Size                     int64
	MarkedAt                 time.Time // zero when the mark has no time
}

// MarkedList is the count and the size of the marked blobs of a store and
// the first of them in the order of a walk.
type MarkedList struct {
	Count     int
	Bytes     int64
	Blobs     []MarkedBlob
	Truncated bool
}

// tryLock takes the lock of a path when nobody holds or waits for it, in
// one step under the map of the locks.
func (p *pathLocks) tryLock(repo, path string) (unlock func(), ok bool) {
	key := [2]string{repo, path}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = map[[2]string]*pathLock{}
	}
	if p.held[key] != nil {
		return nil, false
	}
	l := &pathLock{users: 1}
	l.mu.Lock() // a new lock nobody else knows yet
	p.held[key] = l
	return func() {
		l.mu.Unlock()
		p.mu.Lock()
		if l.users--; l.users == 0 {
			delete(p.held, key)
		}
		p.mu.Unlock()
	}, true
}

// lockStep is how often lockCtx tries a held lock again.
const lockStep = 5 * time.Millisecond

// lockCtx takes the lock of a path, waiting while a write holds it, and
// gives up when ctx ends, so that a stop ends a run within a second.
func (p *pathLocks) lockCtx(ctx context.Context, repo, path string) (unlock func(), err error) {
	for {
		if unlock, ok := p.tryLock(repo, path); ok {
			return unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockStep):
		}
	}
}

// markedOf walks a store and gives each blob with the delete mark.
func markedOf(ctx context.Context, name, root string, each func(stores.Marked) error) error {
	return walkMarks(ctx, name, root, each, nil)
}

// walkMarks is markedOf that also gives loose the location of each bytes
// file without its properties file; a nil loose gives none.
func walkMarks(ctx context.Context, name, root string, each func(stores.Marked) error, loose func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := stores.NewBlobStore(name, root)
	if err != nil {
		return failed("the store cannot be read", err)
	}
	var looseOf func(string) error
	if loose != nil {
		looseOf = func(location string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return loose(location)
		}
	}
	err = store.WalkLoose(nil, func(stores.Entry) error { return ctx.Err() }, func(m stores.Marked) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return each(m)
	}, looseOf)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return failed("the store cannot be read", err)
	}
	return nil
}

// MarkedBlobs counts the blobs with the delete mark of a store of this
// server and lists the first limit of them, cleanup.PreviewLimit for a
// limit below 1 (ruling CL12). It writes nothing.
func (s *Server) MarkedBlobs(ctx context.Context, store string, limit int) (MarkedList, error) {
	var list MarkedList
	if limit < 1 {
		limit = cleanup.PreviewLimit
	}
	root, err := s.ownStoreRoot(store)
	if err != nil {
		return list, err
	}
	err = markedOf(ctx, store, root, func(m stores.Marked) error {
		list.Count++
		list.Bytes += m.Size
		if len(list.Blobs) == limit {
			list.Truncated = true
			return nil
		}
		blob := MarkedBlob{Repository: m.Repo, Path: m.Path, Reason: m.Reason, Size: m.Size}
		if mark, ok, err := blobwrite.ReadMark(root, m.Location); err == nil && ok {
			blob.MarkedAt = mark.At
		}
		list.Blobs = append(list.Blobs, blob)
		return nil
	})
	if list.Blobs == nil {
		list.Blobs = []MarkedBlob{}
	}
	return list, err
}

// CompactStore removes the blobs of a store of this server whose delete
// mark is older than olderThan (rulings CL4 and CL5), each in a unit of
// work of its own (ruling CL9). A store of the old product is refused and
// nothing is removed (decision A8).
func (s *Server) CompactStore(ctx context.Context, store string, olderThan time.Duration) (CompactCounts, error) {
	var c CompactCounts
	root, err := s.ownStoreRoot(store)
	if err != nil {
		return c, err
	}
	var locations, loose []string
	if err := walkMarks(ctx, store, root, func(m stores.Marked) error {
		locations = append(locations, m.Location)
		return nil
	}, func(location string) error {
		loose = append(loose, location)
		return nil
	}); err != nil {
		return c, err
	}
	cutoff := s.side().clock().Add(-olderThan)
	for _, location := range locations {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		mark, marked, err := blobwrite.ReadMark(root, location)
		switch {
		case err != nil:
			logAt(slogWarn, serverLogger, "the delete mark of %s in store %s cannot be read: %v", location, store, err)
			c.Skipped++
			continue
		case !marked:
			continue // removed since the walk
		case !mark.At.Before(cutoff):
			c.Kept++
			continue
		}
		unlock, free := s.writes.locks.tryLock(mark.Repo, mark.Path)
		if !free {
			c.Skipped++
			continue
		}
		end, err := s.taskUnit(ctx)
		if err != nil {
			unlock()
			if errors.Is(err, errFrozen) {
				return c, &tasks.Failure{Text: "stopped: the server froze", Cause: errFrozen}
			}
			return c, err
		}
		removed, err := s.compactBlob(store, root, location, mark, cutoff)
		end()
		unlock()
		if err != nil {
			return c, failed("a blob of the store cannot be removed, so the run stops", err)
		}
		if removed {
			c.Removed++
			c.Bytes += mark.Size
		} else {
			c.Skipped++
		}
	}
	return c, s.compactLoose(ctx, store, root, loose, cutoff, &c)
}

// compactLoose removes the bytes files without properties of a write store
// (local-write or a created store) last written before cutoff: a kill between the two unlinks of a
// compaction leaves one, and so does a kill between the two renames of a
// write, which lasts far less than any cutoff. The writer of the store
// removes them, each in a unit of work of its own.
func (s *Server) compactLoose(ctx context.Context, store, root string, loose []string, cutoff time.Time, c *CompactCounts) error {
	w, err := s.writes.markWriter(store) // the writer of local-write or of a created store (SR3)
	if err != nil || w.Name() != store {
		return nil
	}
	for _, location := range loose {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(location)))
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		end, err := s.taskUnit(ctx)
		if err != nil {
			if errors.Is(err, errFrozen) {
				return &tasks.Failure{Text: "stopped: the server froze", Cause: errFrozen}
			}
			return err
		}
		err = w.RemoveLoose(location)
		end()
		switch {
		case err == nil:
			c.Removed++
			c.Bytes += info.Size()
		case errors.Is(err, fs.ErrNotExist):
		default:
			logAt(slogWarn, serverLogger, "a loose bytes file of store %s is not removed: %v", store, err)
			c.Skipped++
		}
	}
	return nil
}

// compactBlob removes one marked blob under the lock of its path: a blob
// that a row of the index still names stays; else its files, then the row
// of catalog.sqlite that still describes it.
func (s *Server) compactBlob(store, root, location string, mark blobwrite.Mark, cutoff time.Time) (bool, error) {
	rows, err := s.rp.index.Rows(mark.Repo, mark.Path)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Store == store && r.Location == location {
			return false, nil
		}
	}
	var removed bool
	if w, werr := s.writes.markWriter(store); werr == nil && w.Name() == store {
		_, removed, err = w.Compact(location, cutoff)
	} else {
		_, removed, err = blobwrite.CompactAt(root, location, cutoff)
	}
	if err != nil || !removed {
		return removed, err
	}
	s.marked.delta(store, -1, -mark.Counted) // the kept count of the marked blobs, which holds the size of the properties (SR15, review P3)
	cat, err := s.side().catalog()
	if err == nil && cat != nil {
		err = cat.DropMarked(store, mark.Repo, mark.Path, mark.CreatedMS)
	}
	if err != nil {
		logAt(slogWarn, serverLogger, "the catalog row of %s/%s is not removed: %v", mark.Repo, mark.Path, err)
	}
	return true, nil
}
