package blobwrite

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// Compaction of a blob with the delete mark (decision A8, rulings CL4 and
// CL5 of leg 6-cleanup): the copy of the mark goes first, then the
// properties file with a sync of its directory, then the bytes file. A
// kill after the properties file leaves a bytes file without properties,
// which a scan ignores and a later compaction leaves alone.

// Mark is the delete mark of a blob as compaction reads it.
type Mark struct {
	Repo, Path string
	CreatedMS  int64
	Size       int64     // of the bytes file, 0 when it is gone
	At         time.Time // deletedDateTime
	// Counted is the size of the properties, the bytes the count of the
	// marked blobs holds for the blob (ruling SR15 of leg 6-stores-b).
	Counted int64
}

// compactBetween is called between the removal of the properties file and
// the removal of the bytes file of a compaction; an error stops there, as a
// kill would. Tests set it.
var compactBetween func(location string) error

// errNoMarkTime is a mark without a readable deletedDateTime: its age is
// not known, so it is never compacted.
var errNoMarkTime = errors.New("the delete mark has no time")

// markedFiles are the files of one blob location of a store.
type markedFiles struct {
	dir, id      string // the directory below the root in slash form, the blob id
	props, bytes string // resolved paths
	realRoot     string
}

// filesOf checks a bytes location as blobAt does and resolves the files of
// its blob below the real root; neither file has to exist.
func filesOf(root, location string) (markedFiles, error) {
	var f markedFiles
	parts := strings.Split(location, "/")
	bad := len(parts) < 3 || parts[0] != stores.ContentDir || parts[1] == TmpDir || parts[1] == directPathDir ||
		path.Clean(location) != location || strings.ContainsAny(location, "\\\x00")
	for _, part := range parts {
		bad = bad || part == "" || part == "." || part == ".."
	}
	id, isBytes := strings.CutSuffix(parts[len(parts)-1], stores.BytesSuffix)
	if bad || !isBytes || id == "" || strings.ContainsAny(id, "/") {
		return f, fmt.Errorf("%q is no location of a blob", location)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return f, err
	}
	if f.realRoot, err = filepath.EvalSymlinks(abs); err != nil {
		return f, err
	}
	f.dir, f.id = path.Dir(location), id
	realDir, err := filepath.EvalSymlinks(filepath.Join(f.realRoot, filepath.FromSlash(f.dir)))
	if errors.Is(err, fs.ErrNotExist) {
		realDir = filepath.Join(f.realRoot, filepath.FromSlash(f.dir))
	} else if err != nil {
		return f, err
	}
	if !strings.HasPrefix(realDir, f.realRoot+string(filepath.Separator)) {
		return f, fmt.Errorf("%s lies outside the store", location)
	}
	f.props = filepath.Join(realDir, id+stores.PropertiesSuffix)
	f.bytes = filepath.Join(realDir, id+stores.BytesSuffix)
	return f, nil
}

// regular is the state of a file: present and regular, absent, or an error
// for anything else.
func regular(file string) (bool, error) {
	info, err := os.Lstat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is no regular file", filepath.Base(file))
	}
	return true, nil
}

// readItems reads a properties file of at most stores.MaxPropertiesBytes.
func readItems(file string) (map[string]string, []byte, error) {
	f, err := os.Open(file) // #nosec G304 -- resolved below the store root by filesOf.
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, stores.MaxPropertiesBytes+1))
	f.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(data) > stores.MaxPropertiesBytes {
		return nil, nil, fmt.Errorf("the properties are larger than %d bytes", stores.MaxPropertiesBytes)
	}
	// A properties file is ISO 8859-1: each byte is one character.
	text := make([]rune, len(data))
	for i, c := range data {
		text[i] = rune(c)
	}
	items, err := blobprops.Load(string(text))
	return items, data, err
}

// readMark is the mark of the blob of f; false for a blob without the mark
// or without its properties file.
func readMark(f markedFiles) (Mark, bool, error) {
	present, err := regular(f.props)
	if err != nil || !present {
		return Mark{}, false, err
	}
	items, data, err := readItems(f.props)
	if err != nil {
		return Mark{}, false, err
	}
	parsed, err := blobprops.Parse(data)
	if err != nil {
		return Mark{}, false, err
	}
	if _, isCopy := items["originalLocation"]; isCopy || parsed == nil {
		return Mark{}, false, errors.New("the properties are a copy or lack a required key")
	}
	if !parsed.Deleted {
		return Mark{}, false, nil
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(items["deletedDateTime"]), 10, 64)
	if err != nil {
		return Mark{}, false, errNoMarkTime
	}
	m := Mark{Repo: parsed.Repo, Path: parsed.Path, CreatedMS: parsed.CreatedMS, At: time.UnixMilli(ms).UTC(), Counted: parsed.Size}
	if info, err := os.Lstat(f.bytes); err == nil && info.Mode().IsRegular() {
		m.Size = info.Size()
	}
	return m, true, nil
}

// copyOf is the copy of the mark that MarkDeleted wrote in the directory of
// its minute, "" when the mark names none or names the directory of the
// blob.
func copyOf(f markedFiles) (string, error) {
	items, _, err := readItems(f.props)
	if err != nil {
		return "", err
	}
	at, err := time.Parse("2006-01-02T15:04", items["softDeletedLocation"])
	if err != nil {
		return "", nil
	}
	dir := minuteDir(at)
	if dir == f.dir {
		return "", nil
	}
	return filepath.Join(f.realRoot, filepath.FromSlash(dir), f.id+stores.PropertiesSuffix), nil
}

// ReadMark reads the delete mark of the blob whose bytes file is at
// location of the store at root; false for a blob without the mark or
// without its properties file.
func ReadMark(root, location string) (Mark, bool, error) {
	f, err := filesOf(root, location)
	if err != nil {
		return Mark{}, false, err
	}
	return readMark(f)
}

// CompactAt removes the blob at location of the store at root when it
// carries the delete mark and the mark was written before cutoff: the copy
// of the mark, the properties file, a sync of its directory, then the bytes
// file. It gives the mark read and whether it removed the blob.
func CompactAt(root, location string, cutoff time.Time) (Mark, bool, error) {
	f, err := filesOf(root, location)
	if err != nil {
		return Mark{}, false, err
	}
	m, marked, err := readMark(f)
	if err != nil || !marked || !m.At.Before(cutoff) {
		return m, false, err
	}
	copied, err := copyOf(f)
	if err != nil {
		return m, false, err
	}
	if copied != "" {
		if err := removeCopy(copied); err != nil {
			return m, false, err
		}
	}
	if err := os.Remove(f.props); err != nil {
		return m, false, err
	}
	if err := syncDir(filepath.Dir(f.props)); err != nil {
		return m, false, err
	}
	if compactBetween != nil {
		if err := compactBetween(location); err != nil {
			return m, false, err
		}
	}
	if err := os.Remove(f.bytes); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return m, false, err
	}
	return m, true, syncDir(filepath.Dir(f.bytes))
}

// removeCopy removes a copy of a mark: only a regular file whose
// properties carry originalLocation.
func removeCopy(file string) error {
	present, err := regular(file)
	if err != nil || !present {
		return err
	}
	items, _, err := readItems(file)
	if err != nil {
		return err
	}
	if _, isCopy := items["originalLocation"]; !isCopy {
		return nil
	}
	if err := os.Remove(file); err != nil {
		return err
	}
	return syncDir(filepath.Dir(file))
}

// Compact is CompactAt for the store of the writer, one mark or compaction
// at a time.
func (w *Writer) Compact(location string, cutoff time.Time) (Mark, bool, error) {
	if err := w.open(); err != nil {
		return Mark{}, false, err
	}
	w.markMu.Lock()
	defer w.markMu.Unlock()
	return CompactAt(w.root, location, cutoff)
}
