package logs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxWaiting is the most bytes one file holds in memory before a flush.
const maxWaiting = 8 << 20

// retryDelay is the time of the clock after which a failed file is opened
// again.
const retryDelay = 5 * time.Second

var errClosed = errors.New("the log files are closed")

// logFile is the writer of one log file. A line waits in memory until the
// next flush, or goes to the disk with the waiting ones before the call
// returns. The writer rotates the file at MaxBytes and at the day change in
// UTC (rotate.go).
type logFile struct {
	files *Files
	kind  string
	path  string // the active file, <kind>.log

	mu      sync.Mutex // buf, waitDay and closed
	buf     []byte     // the lines that wait for the next flush
	waitDay string     // the UTC day of the clock when the first waiting line came
	closed  bool

	disk   sync.Mutex // the fields below; held while bytes go to the disk, in the order they were taken
	f      *os.File   // nil after a failed open, write or rotation, until the open is tried again
	failed error      // the error of that failure
	retry  time.Time  // the time of the clock from which the open is tried again
	size   int64      // the bytes of the active file
	day    string     // the UTC day of the lines in the active file, while it holds any
	spare  []byte

	failure atomic.Pointer[string] // "<file>: <error>" while the last open or write failed, for Health
}

// openLogFile opens the active file of kind in the log directory.
func openLogFile(files *Files, kind string) *logFile {
	lf := &logFile{files: files, kind: kind, path: filepath.Join(files.dir, kind+".log")}
	lf.open(files.now())
	return lf
}

// open opens the active file for appending: mode 0600, never through a
// link. A file that holds lines and whose last write lies on an earlier UTC
// day rotates first, under that day.
func (lf *logFile) open(now time.Time) {
	f, err := os.OpenFile(lf.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|noFollow, 0o600) // #nosec G304 -- a file name of this package in the log directory of the operator.
	if err != nil {
		lf.fail(err, now)
		return
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close() // the stat failed; the file is not used
		lf.fail(err, now)
		return
	}
	size, err := cutFragment(lf.path, f, info.Size())
	if err != nil {
		_ = f.Close() // the fragment could not be cut; the file is not used
		lf.fail(err, now)
		return
	}
	lf.f, lf.failed, lf.size, lf.day = f, nil, size, dayOf(now)
	lf.failure.Store(nil)
	if last := dayOf(info.ModTime()); lf.size > 0 && last < lf.day {
		lf.day = last
		_ = lf.rotate(nil, last, now) // a failure stays in the state of the file
	}
}

// add puts a line in memory for the next flush. A line that would take the
// waiting bytes past maxWaiting is dropped and counted.
func (lf *logFile) add(line []byte) {
	lf.files.ship(lf.kind, line)
	if lf.files.off() {
		return
	}
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if lf.closed || len(lf.buf)+len(line) > maxWaiting {
		lf.files.errors.Add(1)
		return
	}
	if len(lf.buf) == 0 {
		lf.waitDay = dayOf(lf.files.now())
	}
	lf.buf = append(lf.buf, line...)
}

// flush writes the waiting lines and then extra before it returns. The
// lines are taken under mu and written outside it, so add never waits for
// the disk. A rotated file is compressed after the lock is released.
func (lf *logFile) flush(extra []byte) error {
	lf.files.ship(lf.kind, extra)
	if lf.files.off() {
		return nil
	}
	lf.disk.Lock()
	lf.mu.Lock()
	if lf.closed {
		lf.mu.Unlock()
		lf.disk.Unlock()
		lf.files.errors.Add(lineCount(extra))
		return errClosed
	}
	data, day := lf.buf, lf.waitDay
	lf.buf = lf.spare[:0]
	lf.mu.Unlock()
	err := lf.out(data, day, extra)
	lf.spare = data
	lf.disk.Unlock()
	lf.files.maint.kick()
	return err
}

// out writes the waiting lines as lines of their day, then extra as lines
// of the day of the clock. A failed file is opened again first once
// retryDelay passed.
func (lf *logFile) out(waiting []byte, waitDay string, extra []byte) error {
	now := lf.files.now()
	if lf.f == nil && !now.Before(lf.retry) {
		_ = os.MkdirAll(lf.files.dir, 0o750) // a directory that cannot be made fails the open
		lf.open(now)
	}
	var err error
	if len(waiting) > 0 {
		err = lf.put(waiting, waitDay, now)
	}
	return errors.Join(err, lf.put(extra, dayOf(now), now))
}

// put writes data, whole lines, to the active file as lines of day. The file
// rotates first when it holds lines of another day, and before a line that
// would take a file that holds lines past MaxBytes.
func (lf *logFile) put(data []byte, day string, now time.Time) error {
	var err error
	if lf.f != nil && lf.size > 0 && lf.day != day {
		err = lf.rotate(nil, day, now)
	}
	start := 0
	for i := 0; i < len(data); {
		end := len(data)
		if j := bytes.IndexByte(data[i:], '\n'); j >= 0 {
			end = i + j + 1
		}
		if held := lf.size + int64(i-start); lf.f != nil && held > 0 && held+int64(end-i) > lf.files.settings.MaxBytes {
			err = errors.Join(err, lf.rotate(data[start:i], day, now))
			start = i
		}
		i = end
	}
	return errors.Join(err, lf.write(data[start:], day, now))
}

// write appends p, whole lines of day, to the active file; the lines that do
// not reach it are counted, and a failed write closes the file (fail).
func (lf *logFile) write(p []byte, day string, now time.Time) error {
	if len(p) == 0 {
		return nil
	}
	if lf.f == nil {
		lf.files.errors.Add(lineCount(p))
		if lf.failed == nil {
			return errClosed
		}
		return lf.failed
	}
	if lf.size == 0 {
		lf.day = day
	}
	n, err := writeFile(lf.f, p)
	lf.size += int64(n)
	if err != nil {
		lf.files.errors.Add(lineCount(p[n:]))
		lf.fail(err, now)
	}
	return err
}

// fail closes the active file after a failed open, write or rotation: the
// check turns red with the error of the system, and the open is tried again
// once retryDelay of the clock passed.
func (lf *logFile) fail(err error, now time.Time) {
	if lf.f != nil {
		_ = lf.f.Close() // the file failed already; its lines are counted
		lf.f = nil
	}
	lf.failed, lf.retry = err, now.Add(retryDelay)
	detail := filepath.Base(lf.path) + ": " + strings.ReplaceAll(err.Error(), "\n", "; ")
	lf.failure.Store(&detail)
}

// close writes the waiting lines and closes the file; a later line is
// dropped and counted.
func (lf *logFile) close() error {
	lf.disk.Lock()
	defer lf.disk.Unlock()
	lf.mu.Lock()
	if lf.closed {
		lf.mu.Unlock()
		return nil
	}
	lf.closed = true
	data, day := lf.buf, lf.waitDay
	lf.buf = nil
	lf.mu.Unlock()
	if lf.files.off() {
		return nil
	}
	err := lf.out(data, day, nil)
	if lf.f != nil {
		err = errors.Join(err, lf.f.Close())
		lf.f = nil
	}
	return err
}

// lineCount is the number of lines in data, each ended by a line break.
func lineCount(data []byte) uint64 {
	var n uint64
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}
