package logs

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A rotated file whose compression fails keeps its .log, and maint.failed
// keeps "<file>: <error of the system>" for Health. The hourly run queues
// every such file again, and then every rotated .log of the directory that
// waits in no queue (sweep); a later success of the file, or a file that is
// gone, clears its part of the detail. Errors counts lines only and stays as
// it is.

// compressOne compresses one queued file and keeps or clears its failure.
func (m *maint) compressOne(path string) {
	run := m.compressor
	if run == nil {
		run = compress
	}
	err := run(path)
	if err != nil && gone(path) {
		err = nil // nothing is left to compress
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.failed, path)
		return
	}
	if m.failed == nil {
		m.failed = map[string]string{}
	}
	m.failed[path] = filepath.Base(path) + ": " + strings.ReplaceAll(err.Error(), "\n", "; ")
}

// gone reports whether path no longer exists.
func gone(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// hourly is the hourly run of the maintenance: every rotated file whose last
// compression failed is queued again, then every other rotated .log of the
// directory (sweep), then the retention.
func (m *maint) hourly() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, path := range slices.Sorted(maps.Keys(m.failed)) {
		if !slices.Contains(m.queue, path) {
			m.queue = append(m.queue, path)
		}
	}
	m.sweep, m.retain = true, true
}

// sweep queues every rotated .log of the directory that waits in no queue:
// one that a crash, a failed compression or an operator left. It runs in the
// goroutine of maint once the queue is empty, so no compression is half
// done. A file that a rotation queues as well is compressed once; the second
// entry finds it gone, which counts as done.
func (f *Files) sweep() {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return // the next hour tries again
	}
	for _, e := range entries {
		if r, ok := parseRotated(e.Name()); ok && r.ext == extLog && e.Type().IsRegular() {
			f.maint.queueOnce(filepath.Join(f.dir, r.name))
		}
	}
}

// queueOnce queues path unless it waits in the queue already.
func (m *maint) queueOnce(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Contains(m.queue, path) {
		m.queue = append(m.queue, path)
	}
}

// failures are the details of the failed compressions, in the order of the
// names.
func (m *maint) failures() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var details []string
	for _, path := range slices.Sorted(maps.Keys(m.failed)) {
		details = append(details, m.failed[path])
	}
	return details
}
