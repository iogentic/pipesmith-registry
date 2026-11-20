package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// The audit line of a command that runs while a server holds the log
// directory (finding 7b-1) waits as one file in audit-pending of that
// directory. The command writes it under a temporary name and renames it,
// so a crash leaves no half line under a name of a line. The server that
// holds the lock moves every pending line into audit.log, and so to its
// log targets, on its next Tick and removes the file; Open takes them
// first when no tap is set. Lines are taken in the order of their names:
// the time of the command, its process id and a sequence.

// pendingName is the folder of the pending lines in the log directory.
const pendingName = "audit-pending"

// maxPending is the most bytes one pending file may hold; a capped audit
// line is far below it.
const maxPending = 64 << 10

// pendingTmpAge is the age after which a temporary file of a command that
// did not rename it is removed.
const pendingTmpAge = time.Hour

// pendingPattern is the name of a pending line.
var pendingPattern = regexp.MustCompile(`^[0-9]{20}-[0-9]{10}-[0-9]{6}\.line$`)

// pendingSeq makes the names of one process unique.
var pendingSeq atomic.Uint32

// writePending writes line, one audit line with its line break, as one file
// into the pending folder of dir: mode 0700 for the folder, 0600 for the
// file, written to a temporary name, synced and renamed.
func writePending(dir string, line []byte, now time.Time) error {
	folder := filepath.Join(dir, pendingName)
	if err := os.Mkdir(folder, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if info, err := os.Lstat(folder); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", folder)
	}
	tmp, err := os.CreateTemp(folder, ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(line)
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		_ = os.Remove(tmp.Name()) // the line is reported by the caller
		return err
	}
	name := fmt.Sprintf("%020d-%010d-%06d.line", now.UnixNano(), os.Getpid(), pendingSeq.Add(1)%1000000)
	if err := os.Rename(tmp.Name(), filepath.Join(folder, name)); err != nil {
		_ = os.Remove(tmp.Name()) // the line is reported by the caller
		return err
	}
	return nil
}

// takePending moves the pending lines into audit.log in the order of their
// names and removes each file once its line was written. A line that cannot
// be written stays for the next call. A file that holds no single audit
// line of this package is removed and counted as an error.
func (f *Files) takePending() {
	if f.off() {
		return
	}
	f.pending.Lock()
	defer f.pending.Unlock()
	folder := filepath.Join(f.dir, pendingName)
	if info, err := os.Lstat(folder); err != nil || !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(folder) // sorted by name
	if err != nil {
		return
	}
	now := f.now()
	for _, entry := range entries {
		path := filepath.Join(folder, entry.Name())
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > pendingTmpAge {
				_ = os.Remove(path) // a command ended before its rename; its line was reported to it
			}
			continue
		}
		if !pendingPattern.MatchString(entry.Name()) {
			continue
		}
		line, ok := readPending(path)
		if !ok {
			f.errors.Add(1)
			_ = os.Remove(path) // not a line of this package
			continue
		}
		if err := f.audit.flush(line); err != nil {
			return // the file failed; the line waits for the next call
		}
		_ = os.Remove(path) // a file that stays is written again by the next call
	}
}

// readPending reads one pending file: a regular file, not a link, of one
// audit line of this package with its line break.
func readPending(path string) ([]byte, bool) {
	file, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) // #nosec G304 -- a pending file of this package in the log directory of the operator.
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }() // read only
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > maxPending {
		return nil, false
	}
	line, err := io.ReadAll(io.LimitReader(file, maxPending+1))
	if err != nil || len(line) > maxPending || bytes.IndexByte(line, '\n') != len(line)-1 {
		return nil, false
	}
	if !json.Valid(line) {
		return nil, false
	}
	if h, ok := headOf(line[:len(line)-1]); !ok || h.Logger != loggerAudit {
		return nil, false
	}
	return line, true
}
