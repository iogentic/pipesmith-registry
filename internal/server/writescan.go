package server

import (
	"bufio"
	"cmp"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The scan of the write store. A delete mark that waits (marks.go) lives in
// the memory of the process, so a kill between the row of a write and the
// mark of the blob it replaced leaves two live blobs of one path in the
// write store. A start with the writer open runs this scan before it takes
// a request (repairAtStart, writestore.go). The scan of the write store
// serves the blob of a path that was written last, keeps a path deleted whose delete came after its last live
// blob, and, while writes are on, gives every such older live blob its
// delete mark. No file format changes: the order of the writes is read from
// what the store holds.
//
// "Written last" between two blobs of one path is the later creationTime.
// Two writes in one millisecond carry the same one; then the later line of
// the reconciliation log decides.
// The writer appends that line under a lock as the last step of every blob,
// and writes of one path run one at a time under the lock of the path, so
// the lines of a path stand in the order of its writes. A blob without a
// line counts as written after one with a line: only a kill between the
// rename of its properties and its line leaves it so, and that write was
// the last of its path in that process. When neither line tells, the order
// of the walk decides, as for every other store. The stores of the old
// product keep the scan of 0.1.1 unchanged and are never written.

// writeOrder reads the reconciliation log of the write store to order two
// blobs of one path written in the same millisecond. It reads the log of a
// day once per scan, and only when such a pair asks for it.
type writeOrder struct {
	root string
	days map[string]map[string]int // day of a log file: blob id -> number of its line
}

func newWriteOrder(root string) *writeOrder {
	return &writeOrder{root: root, days: map[string]map[string]int{}}
}

// later reports whether b was written after a, both of one path.
func (o *writeOrder) later(a, b stores.Entry) bool {
	if a.CreatedMS != b.CreatedMS {
		return b.CreatedMS > a.CreatedMS
	}
	dayA, lineA, foundA := o.logLine(a)
	dayB, lineB, foundB := o.logLine(b)
	switch {
	case foundA && foundB:
		return dayB > dayA || dayB == dayA && lineB > lineA
	case foundA:
		return true // b has no line: the kill came between its properties and its line
	default:
		return false // a has no line, or neither has one: the order of the walk
	}
}

// logLine is the place of the line of a blob in the reconciliation log: the
// day of its file and its number there. The line is written after the blob
// is placed in the directory of the minute its id was drawn in, so it lies
// in the log of that day or of the next one.
func (o *writeOrder) logLine(e stores.Entry) (day string, line int, found bool) {
	dir, name := path.Split(e.Location)
	id := strings.TrimSuffix(name, stores.BytesSuffix)
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	if len(parts) != 6 || parts[0] != stores.ContentDir {
		return "", 0, false
	}
	first := parts[1] + "-" + parts[2] + "-" + parts[3]
	for _, day := range []string{first, nextDay(first)} {
		if n, ok := o.day(day)[id]; ok {
			return day, n, true
		}
	}
	return "", 0, false
}

// nextDay is the day after a day yyyy-MM-dd, "" for a text that is none.
func nextDay(day string) string {
	at, err := time.Parse("2006-01-02", day)
	if err != nil {
		return ""
	}
	return at.AddDate(0, 0, 1).Format("2006-01-02")
}

// day is the number of the line of every blob id in the log of a day.
func (o *writeOrder) day(day string) map[string]int {
	if lines, ok := o.days[day]; ok {
		return lines
	}
	lines := map[string]int{}
	o.days[day] = lines
	if day == "" {
		return lines
	}
	f, err := os.Open(filepath.Join(o.root, blobwrite.ReconciliationDir, filepath.Base(day))) // #nosec G304 -- the log of a day below the root of the write store.
	if err != nil {
		return lines // no log of that day: no blob of it has a line
	}
	defer f.Close()
	lineScanner := bufio.NewScanner(f)
	for n := 0; lineScanner.Scan(); n++ {
		fields := strings.Split(lineScanner.Text(), ",")
		if len(fields) != 3 {
			continue
		}
		if _, seen := lines[fields[1]]; !seen {
			lines[fields[1]] = n
		}
	}
	return lines
}

// staleBlob is a live blob of the write store that a later blob of its path
// replaces or a later delete of its path removes, with the reason of its
// mark.
type staleBlob struct {
	stores.Entry
	reason string
}

// writeScan is the arbiter of one scan of the write store.
type writeScan struct {
	order   *writeOrder
	deletes map[[2]string]stores.Entry // repository and path: the blob written last that a delete of the path marked
	stale   []staleBlob
}

var _ index.Arbiter = (*writeScan)(nil)

func (a *writeScan) Newer(held, next stores.Entry) bool {
	if a.order.later(held, next) {
		a.stale = append(a.stale, staleBlob{held, reasonReplaced})
		return true
	}
	a.stale = append(a.stale, staleBlob{next, reasonReplaced})
	return false
}

// marked keeps the blobs a delete of their path marked; the marks of a
// replaced blob and of a blob the index did not take decide nothing.
func (a *writeScan) marked(m stores.Marked) error {
	if m.Reason != reasonDeleted {
		return nil
	}
	key := [2]string{m.Repo, m.Path}
	if held, ok := a.deletes[key]; !ok || a.order.later(held, m.Entry) {
		a.deletes[key] = m.Entry
	}
	return nil
}

// Settle drops the row of a path whose delete was written after the blob of
// the row: the path stays deleted, and the blob is stale.
func (a *writeScan) Settle(rows index.Rows) error {
	keys := slices.SortedFunc(maps.Keys(a.deletes), func(x, y [2]string) int {
		return cmp.Or(cmp.Compare(x[0], y[0]), cmp.Compare(x[1], y[1]))
	})
	for _, key := range keys {
		row, err := rows.Row(key[0], key[1])
		if err != nil {
			return err
		}
		if row == nil || !a.order.later(*row, a.deletes[key]) {
			continue
		}
		if err := rows.Drop(key[0], key[1]); err != nil {
			return err
		}
		a.stale = append(a.stale, staleBlob{*row, reasonDeleted})
	}
	return nil
}

// walker is a store whose walk gives its marked blobs too.
type walker interface {
	Walk(counts *stores.ScanCounts, yield func(stores.Entry) error, marked func(stores.Marked) error) error
}

// looseWalker is a walker that gives its bytes files without properties too.
type looseWalker interface {
	WalkLoose(counts *stores.ScanCounts, yield func(stores.Entry) error, marked func(stores.Marked) error, loose func(string) error) error
}

// markedWalk is a store whose Entries give its marked blobs to marked, and
// its bytes files without properties to loose when that is set.
type markedWalk struct {
	stores.Store
	marked func(stores.Marked) error
	loose  func(location string) error
}

func (m *markedWalk) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	if w, ok := m.Store.(looseWalker); ok && m.loose != nil {
		return w.WalkLoose(counts, yield, m.marked, m.loose)
	}
	if w, ok := m.Store.(walker); ok {
		return w.Walk(counts, yield, m.marked)
	}
	return m.Store.Entries(counts, yield)
}

// scanRunnerFor is the runner of the scan job for the write store, nil for
// every other store.
func (wp *writePath) scanRunnerFor(s stores.Store) scanRunner {
	if wp.store != nil && s == stores.Store(wp.store) {
		return wp.scanStore
	}
	if late := wp.late.Load(); late != nil && s == stores.Store(late) {
		return (&writePath{store: late, file: wp.file}).scanStore // writes are off: no delete mark
	}
	return nil
}

// scanStore is the scan runner of the job for the write store: the scan with
// the arbiter of the write order, then the marks of the stale blobs while
// writes are on. With writes off the store is read only and the marks wait
// for a scan with writes on.
func (wp *writePath) scanStore(s stores.Store, ix *index.Index, progress func(live int)) (scan.Result, error) {
	arbiter := &writeScan{order: newWriteOrder(wp.store.Root()), deletes: map[[2]string]stores.Entry{}}
	walk := &markedWalk{Store: s, marked: arbiter.marked}
	if wp.writer != nil {
		walk.loose = wp.removeLoose
	}
	result, err := scan.RunIn(wp.file, walk, ix, progress, arbiter)
	if err == nil {
		wp.noteDeletes(arbiter.deletes)
	}
	if err != nil || wp.writer == nil {
		return result, err
	}
	marked := 0
	for _, blob := range arbiter.stale {
		if wp.markStale(ix, blob) {
			marked++
		}
	}
	if len(arbiter.stale) > 0 {
		logAt(slogInfo, serverLogger, "the scan of store %s found %d live blobs that a later write or delete of their path replaces; %d have their delete mark now",
			wp.writer.Name(), len(arbiter.stale), marked)
	}
	return result, nil
}

// removeLoose removes a bytes file without properties that the walk of a
// scan with writes on meets (N2). The scan holds the index for its walk, so
// no write of this process is between the renames of its two files; a file
// that cannot be removed stays and the scan goes on.
func (wp *writePath) removeLoose(location string) error {
	if err := wp.writer.RemoveLoose(location); err != nil {
		logAt(slogWarn, serverLogger, "the bytes file %s of store %s has no properties and stays: %v", location, wp.writer.Name(), err)
		return nil
	}
	logAt(slogInfo, serverLogger, "the scan of store %s removed the bytes file %s, which had no properties", wp.writer.Name(), location)
	return nil
}

// ScanWriteStore is the scan of the write store for the command scan
// (finding S1 of review 4): the rule of its write order, as the scan job
// runs it while writes are off. It sets the rows and writes no delete mark,
// since a server with writes on may write the store beside the command.
func ScanWriteStore(s *stores.BlobStore, ix *index.Index, progress func(live int)) (scan.Result, error) {
	return (&writePath{store: s}).scanStore(s, ix, progress)
}

// markStale sets the delete mark of a stale blob under the lock of its path,
// unless the row of the path names the blob now. A mark that fails waits as
// the mark of a replaced blob does (marks.go).
func (wp *writePath) markStale(ix *index.Index, blob staleBlob) bool {
	unlock := wp.locks.lock(blob.Repo, blob.Path)
	defer unlock()
	rows, err := ix.Rows(blob.Repo, blob.Path)
	if err != nil {
		logAt(slogWarn, serverLogger, "blob %s of %s/%s is stale and keeps no delete mark: %v", blob.Location, blob.Repo, blob.Path, err)
		return false
	}
	for _, row := range rows {
		if row.Store == wp.writer.Name() && row.Location == blob.Location {
			return false
		}
	}
	if err := wp.mark(blob.Location, blob.reason); err != nil {
		if wp.wait(blob.Repo, blob.Path, blob.Location) {
			logAt(slogWarn, serverLogger, "blob %s of %s/%s is stale and has no delete mark: %v; it is tried again, and the writable status answers 503 until then",
				blob.Location, blob.Repo, blob.Path, err)
		}
		return false
	}
	return true
}
