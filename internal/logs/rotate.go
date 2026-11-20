package logs

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// dayLayout is the UTC day in the name of a rotated file.
const dayLayout = "2006-01-02"

// The endings of the names of a rotated file: uncompressed, compressed, and
// the temporary name of a compression that has not ended.
const (
	extLog = ".log"
	extGz  = ".log.gz"
	extTmp = ".log.gz.tmp"
)

// rotatedPattern is a rotated file: <kind>-<day>.<n>.log, then .gz, then .tmp.
var rotatedPattern = regexp.MustCompile(`^(server|requests|requests-classic|audit|tasks)-([0-9]{4}-[0-9]{2}-[0-9]{2})\.([1-9][0-9]{0,8})(\.log|\.log\.gz|\.log\.gz\.tmp)$`)

// rotated is the name of a rotated file, read by parseRotated.
type rotated struct {
	name string
	kind string
	day  string
	n    int
	ext  string // extLog, extGz or extTmp
	size int64  // set by retain
}

// parseRotated reads a name of a rotated file; any other name is not ours.
func parseRotated(name string) (rotated, bool) {
	m := rotatedPattern.FindStringSubmatch(name)
	if m == nil {
		return rotated{}, false
	}
	if _, err := time.Parse(dayLayout, m[2]); err != nil {
		return rotated{}, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return rotated{}, false
	}
	return rotated{name: name, kind: m[1], day: m[2], n: n, ext: m[4]}, true
}

func dayOf(t time.Time) string { return t.UTC().Format(dayLayout) }

// rotate ends the active file: rest, the last lines that belong to it, is
// written, the file is closed and renamed to <kind>-<day>.<n>.log, a new
// active file is opened and the renamed file goes to the compressor. A
// failure is a failure of the file (fail); its lines are counted.
func (lf *logFile) rotate(rest []byte, day string, now time.Time) error {
	err := lf.write(rest, day, now)
	if lf.f == nil {
		return err
	}
	closeErr := lf.f.Close()
	lf.f = nil
	name, nameErr := lf.files.rotatedName(lf.kind, lf.day)
	if failed := errors.Join(closeErr, nameErr); failed != nil {
		lf.fail(failed, now)
		return errors.Join(err, failed)
	}
	if renameErr := os.Rename(lf.path, name); renameErr != nil {
		lf.fail(renameErr, now)
		return errors.Join(err, renameErr)
	}
	lf.files.maint.compress(name)
	lf.open(now)
	return errors.Join(err, lf.failed)
}

// rotatedName is the path of the next rotated file of kind and day: the
// number is one above the highest that a .log, .log.gz or temporary name of
// the kind and day has.
func (f *Files) rotatedName(kind, day string) (string, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return "", err
	}
	n := 0
	for _, e := range entries {
		if r, ok := parseRotated(e.Name()); ok && r.kind == kind && r.day == day && r.n > n {
			n = r.n
		}
	}
	return filepath.Join(f.dir, kind+"-"+day+"."+strconv.Itoa(n+1)+extLog), nil
}

// leftovers prepares the directory at Open: a rotated file that a crash left
// uncompressed goes to the compressor, which also writes a half written .gz
// beside it again, and the temporary file of a compression that did not end
// is removed.
func (f *Files) leftovers() {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		r, ok := parseRotated(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		switch r.ext {
		case extTmp:
			_ = os.Remove(filepath.Join(f.dir, r.name)) // what is left stays a temporary name
		case extLog:
			f.maint.compress(filepath.Join(f.dir, r.name))
		}
	}
}

// maint does the work on the log directory that can take long, one job at a
// time and outside the locks of the writers: the compression of rotated
// files, then the retention. Its goroutine starts when kick finds work and
// ends when none is left; wait returns once it ended.
type maint struct {
	retention func() // the retention of the directory, set by Open

	mu      sync.Mutex // the fields below
	queue   []string   // the rotated files to compress
	retain  bool       // the retention runs once the queue is empty
	running bool
	done    chan struct{} // closed when the running goroutine ends

	failed     map[string]string       // by path, the detail of each failed compression (recompress.go)
	compressor func(path string) error // nil is compress; a test sets it before the first kick
	sweep      bool                    // the hourly run lists the directory once the queue is empty (recompress.go)
	sweeper    func()                  // that listing, set by Open
}

// compress queues a rotated file and the retention after it; kick starts
// the work.
func (m *maint) compress(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = append(m.queue, path)
	m.retain = true
}

// retainSoon queues the retention; kick starts the work.
func (m *maint) retainSoon() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retain = true
}

// kick starts the goroutine when work waits and none runs.
func (m *maint) kick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running || (len(m.queue) == 0 && !m.retain) {
		return
	}
	m.running, m.done = true, make(chan struct{})
	go m.run(m.done)
}

func (m *maint) run(done chan struct{}) {
	defer close(done)
	for {
		m.mu.Lock()
		switch {
		case len(m.queue) > 0:
			path := m.queue[0]
			m.queue = m.queue[1:]
			m.mu.Unlock()
			m.compressOne(path) // on a failure the .log stays for the hourly run (recompress.go)
		case m.sweep:
			m.sweep = false
			m.mu.Unlock()
			m.sweeper()
		case m.retain:
			m.retain = false
			m.mu.Unlock()
			m.retention()
		default:
			m.running = false
			m.mu.Unlock()
			return
		}
	}
}

// wait returns once no goroutine of m runs.
func (m *maint) wait() {
	for {
		m.mu.Lock()
		running, done := m.running, m.done
		m.mu.Unlock()
		if !running {
			return
		}
		<-done
	}
}

// compress writes path to path.gz and removes path. The bytes go to a
// temporary name that is renamed once it holds the whole file, so a .gz name
// never holds a part.
func compress(path string) error {
	in, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) // #nosec G304 -- a rotated file of this package in the log directory.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // read only
	gz := path + ".gz"
	tmp := gz + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|noFollow, 0o600) // #nosec G304 -- the temporary name of a rotated file of this package.
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	err = errors.Join(err, zw.Close(), out.Sync(), out.Close())
	if err == nil {
		err = os.Rename(tmp, gz)
	}
	if err != nil {
		_ = os.Remove(tmp) // the .log stays
		return err
	}
	syncDir(filepath.Dir(path))
	return os.Remove(path)
}

// syncDir writes the entries of a directory to the disk, so the rename of a
// .gz is there before its .log goes.
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- the log directory of the operator.
	if err != nil {
		return
	}
	_ = d.Sync()  // best effort: the .log goes all the same
	_ = d.Close() // read only
}
