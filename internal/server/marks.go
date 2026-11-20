package server

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The delete marks the write path sets on blobs of the write store (W4,
// W5 of the phase 4 plan): on a blob a write replaced, on a blob a delete
// removes, on a blob the index did not take.
//
// The mark of a replaced blob comes after the new blob is stored, so a
// failure of it fails no write (risk 3 of the 4a report). The blob stays
// live beside the new one; a scan from the files serves the newer creation
// time (TestAFailedMarkOfAReplacedBlob). The failure is logged, and the
// blob waits: its mark is tried again at the next write or delete of its
// path and at each GET /v1/status/writable, which answers 503 while a mark
// waits, so that a client that asks before it writes holds back until the
// store takes writes in full again. The marks that wait live in the memory
// of the process and are not written down: the store holds them already. A
// blob that waits is a live blob of the write store that a later blob of its
// path replaces, and the next start finds every such blob in its scan of the
// store and marks it, or lets its mark wait again (repairAtStart,
// writestore.go). No file and no schema step of state.sqlite is needed.

// maxWaitingMarks bounds the marks that wait to be tried again.
const maxWaitingMarks = 1024

// marks is the state of the delete marks of one write path.
type marks struct {
	mu      sync.Mutex
	fault   func(location, reason string) error // tests: an error in place of a mark
	waiting map[[2]string][]string              // repository and path: the blobs whose mark failed
	count   int
	now     func() time.Time // the clock of the retries of the writable status; time.Now when nil
	retried time.Time        // the last retry of every waiting mark (retryDue)
}

// retryClock sets the clock of the retries of the writable status; tests
// move it.
func (wp *writePath) retryClock(now func() time.Time) {
	wp.marks.mu.Lock()
	defer wp.marks.mu.Unlock()
	wp.marks.now = now
}

// wait keeps a blob of a path whose delete mark failed; beyond
// maxWaitingMarks a blob is not kept.
func (wp *writePath) wait(repo, path, location string) bool {
	wp.marks.mu.Lock()
	defer wp.marks.mu.Unlock()
	if wp.marks.count >= maxWaitingMarks {
		return false
	}
	if wp.marks.waiting == nil {
		wp.marks.waiting = map[[2]string][]string{}
	}
	key := [2]string{repo, path}
	wp.marks.waiting[key] = append(wp.marks.waiting[key], location)
	wp.marks.count++
	return true
}

// waiting is the number of blobs whose delete mark waits.
func (wp *writePath) waiting() int {
	wp.marks.mu.Lock()
	defer wp.marks.mu.Unlock()
	return wp.marks.count
}

// mark sets the delete mark on the blob of the write store at location.
func (wp *writePath) mark(location, reason string) error {
	wp.marks.mu.Lock()
	fault := wp.marks.fault
	wp.marks.mu.Unlock()
	if fault != nil {
		if err := fault(location, reason); err != nil {
			return err
		}
	}
	return wp.writer.MarkDeleted(location, reason)
}

// failMarks makes every mark that fault refuses fail; nil ends it. Tests
// only.
func (wp *writePath) failMarks(fault func(location, reason string) error) {
	wp.marks.mu.Lock()
	defer wp.marks.mu.Unlock()
	wp.marks.fault = fault
}

// markReplaced sets the delete mark on the blob a write of a path
// replaced; a failure is logged and the blob waits.
func (wp *writePath) markReplaced(repo, path, location string) {
	err := wp.mark(location, reasonReplaced)
	if err == nil {
		return
	}
	if wp.wait(repo, path, location) {
		logAt(slogWarn, serverLogger, "blob %s of %s/%s, replaced, has no delete mark: %v; it is tried again, and the writable status answers 503 until then",
			location, repo, path, err)
		return
	}
	logAt(slogWarn, serverLogger, "blob %s of %s/%s, replaced, has no delete mark: %v; %d marks wait already, so this one is not tried again",
		location, repo, path, err, maxWaitingMarks)
}

// retryPath tries the waiting marks of a path again; the caller holds the
// lock of the path.
func (wp *writePath) retryPath(repo, path string) {
	key := [2]string{repo, path}
	wp.marks.mu.Lock()
	locations := append([]string(nil), wp.marks.waiting[key]...)
	wp.marks.mu.Unlock()
	if len(locations) > 0 {
		wp.retry(key, locations)
	}
}

// retryEvery is the shortest time between two tries of every waiting mark
// that GET /v1/status/writable makes (finding N4 of review 4): the route
// needs no sign in, so the rate of its requests must not set the rate of
// the tries. A write or a delete of a path tries the marks of its path at
// once.
const retryEvery = time.Second

// retryDue tries every waiting mark again when the last such try is
// retryEvery ago, or the clock went back since, and returns how many marks
// wait.
func (wp *writePath) retryDue() int {
	wp.marks.mu.Lock()
	now := time.Now()
	if wp.marks.now != nil {
		now = wp.marks.now()
	}
	due := wp.marks.retried.IsZero() || now.Sub(wp.marks.retried) >= retryEvery || now.Before(wp.marks.retried)
	if due {
		wp.marks.retried = now
	}
	wp.marks.mu.Unlock()
	if !due {
		return wp.waiting()
	}
	return wp.retryAll()
}

// retryAll tries every waiting mark again and returns how many still wait.
func (wp *writePath) retryAll() int {
	wp.marks.mu.Lock()
	all := make(map[[2]string][]string, len(wp.marks.waiting))
	for key, locations := range wp.marks.waiting {
		all[key] = append([]string(nil), locations...)
	}
	wp.marks.mu.Unlock()
	for key, locations := range all {
		wp.retry(key, locations)
	}
	return wp.waiting()
}

// maxDeletedTimes bounds the paths whose deleted blob keeps its creation
// time (deletedTimes).
const maxDeletedTimes = 1 << 16

// deletedTimes are the creation times of the blobs that a delete of their
// path marked in this process. The scan of the write store takes a delete
// for a write of its path at the creationTime of the marked blob, and the
// index holds no row of it any more, so a write of the path after a clock
// that stepped back takes its time from here (finding S5 of review 4). A
// write of the path forgets it, its row carries a later time. A restart
// forgets them, and every scan of the write store, the one at the start
// included (repairAtStart), takes them in again from the delete marks of the
// store (noteDeletes).
type deletedTimes struct {
	mu    sync.Mutex
	times map[[2]string]int64
}

// noteDeleted keeps the creation time of a blob a delete of its path
// marked. Past maxDeletedTimes paths the older half goes: only a clock that
// steps back by more than such a time lies behind could undercut it.
func (wp *writePath) noteDeleted(repo, path string, created int64) {
	d := &wp.deleted
	d.mu.Lock()
	defer d.mu.Unlock()
	key := [2]string{repo, path}
	if d.times == nil {
		d.times = map[[2]string]int64{}
	}
	if _, kept := d.times[key]; !kept && len(d.times) >= maxDeletedTimes {
		times := slices.Sorted(maps.Values(d.times))
		cut := times[len(times)/2]
		maps.DeleteFunc(d.times, func(_ [2]string, t int64) bool { return t <= cut })
	}
	d.times[key] = max(d.times[key], created)
}

// noteDeletes keeps the creation time of the blob written last of each path
// that a delete marked, as a scan of the write store found them. A path
// written again after its delete keeps a time too: its row carries a later
// one, and bounds takes the later of both.
func (wp *writePath) noteDeletes(deletes map[[2]string]stores.Entry) {
	for key, blob := range deletes {
		wp.noteDeleted(key[0], key[1], blob.CreatedMS)
	}
}

// deletedTime is the creation time of the newest blob a delete of the path
// marked in this process, 0 for none.
func (wp *writePath) deletedTime(repo, path string) int64 {
	wp.deleted.mu.Lock()
	defer wp.deleted.mu.Unlock()
	return wp.deleted.times[[2]string{repo, path}]
}

// forgetDeleted drops the time of a path once a write of it has a row.
func (wp *writePath) forgetDeleted(repo, path string) {
	wp.deleted.mu.Lock()
	defer wp.deleted.mu.Unlock()
	delete(wp.deleted.times, [2]string{repo, path})
}

// retry tries the marks of locations of a path and forgets each that is
// set now.
func (wp *writePath) retry(key [2]string, locations []string) {
	for _, location := range locations {
		if err := wp.mark(location, reasonReplaced); err != nil {
			continue
		}
		wp.marks.mu.Lock()
		kept := wp.marks.waiting[key][:0]
		for _, l := range wp.marks.waiting[key] {
			if l != location {
				kept = append(kept, l)
			} else {
				wp.marks.count--
			}
		}
		if len(kept) == 0 {
			delete(wp.marks.waiting, key)
		} else {
			wp.marks.waiting[key] = kept
		}
		wp.marks.mu.Unlock()
		logAt(slogInfo, serverLogger, "blob %s of %s/%s, replaced, has its delete mark now", location, key[0], key[1])
	}
}
