package logs

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The reads of the log directory for the log routes (task 7.24): the list
// of the log files and the content of one. Only the five active files and
// their rotated files are served; a link, a folder, the lock file and
// every other name are not.

// ErrName refuses a name with "..", a slash, a backslash or a control byte,
// or one that names a link or no regular file.
var ErrName = errors.New("not the name of a log file of this server")

// LogFile is one row of the list of the log directory.
type LogFile struct {
	Name     string
	Size     int64
	Modified time.Time
}

// servedActive are the names of the files the server writes to.
var servedActive = []string{kindServer + extLog, kindRequests + extLog, kindClassic + extLog, kindAudit + extLog, kindTasks + extLog}

// ServedName is a name of an active file or of a rotated file, plain or
// compressed; the temporary name of a compression is not served.
func ServedName(name string) bool {
	if slices.Contains(servedActive, name) {
		return true
	}
	r, ok := parseRotated(name)
	return ok && r.ext != extTmp
}

// ListFiles lists the log files of dir by name, regular files only. An
// empty dir, the files off, and a missing directory list none.
func ListFiles(dir string) ([]LogFile, error) {
	out := []LogFile{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !ServedName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed by the retention since the read of the directory
		}
		out = append(out, LogFile{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	return out, nil
}

// Opened is a log file open for reading, with its size at the open.
type Opened struct {
	file       *os.File
	Size       int64
	Compressed bool // a rotated .log.gz file
}

// OpenFile opens one log file of dir. A name that is not plain, a link and
// anything but a regular file give ErrName; a plain name that is no log
// file, a log file that is not there, or the files off, give fs.ErrNotExist.
func OpenFile(dir, name string) (*Opened, error) {
	if !plainName(name) {
		return nil, ErrName
	}
	if !ServedName(name) {
		return nil, fs.ErrNotExist
	}
	if dir == "" {
		return nil, fs.ErrNotExist
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrName
	}
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) // #nosec G304 -- the name is one of the log files, checked above
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close() // read only
		if err == nil {
			err = ErrName
		}
		return nil, err
	}
	return &Opened{file: f, Size: info.Size(), Compressed: strings.HasSuffix(name, extGz)}, nil
}

// Close closes the file.
func (o *Opened) Close() error { return o.file.Close() }

// Reader reads the file as it is on disk, Size bytes, for a download.
func (o *Opened) Reader() io.Reader { return io.NewSectionReader(o.file, 0, o.Size) }

// Tail is the last n bytes of the content: of the file, or of the
// uncompressed lines of a compressed one.
func (o *Opened) Tail(n int64) ([]byte, error) {
	if !o.Compressed {
		start := max(0, o.Size-n)
		buf := make([]byte, o.Size-start)
		_, err := io.ReadFull(io.NewSectionReader(o.file, start, o.Size-start), buf)
		return buf, err
	}
	z, err := gzip.NewReader(o.Reader())
	if err != nil {
		return nil, err
	}
	defer func() { _ = z.Close() }() // a reader holds nothing to release
	ring := &tailBuffer{n: n}
	read, err := io.CopyN(ring, z, maxUncompressed+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if read > maxUncompressed {
		return nil, fmt.Errorf("the compressed file holds more than %d bytes, more than a rotated file can hold", maxUncompressed)
	}
	return ring.buf, nil
}

// maxUncompressed bounds the content read from a compressed file: a rotated
// file never holds more than the size cap of its kind (2 GiB by default).
var maxUncompressed int64 = 2 << 30

// tailBuffer keeps the last n bytes written to it.
type tailBuffer struct {
	n   int64
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if int64(len(p)) >= t.n {
		t.buf = append(t.buf[:0], p[int64(len(p))-t.n:]...)
		return written, nil
	}
	if drop := int64(len(t.buf)+len(p)) - t.n; drop > 0 {
		t.buf = append(t.buf[:0], t.buf[drop:]...)
	}
	t.buf = append(t.buf, p...)
	return written, nil
}
