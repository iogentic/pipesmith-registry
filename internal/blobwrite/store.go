package blobwrite

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

const (
	metadataFile = "metadata.properties"
	// fileStoreType is the type line of metadata.properties of a file store.
	fileStoreType = "file/1"
	directPathDir = "directpath"
)

// dateDir is the directory of a blob in the date scheme.
var dateDir = regexp.MustCompile(`^content/(\d{4})/(\d\d)/(\d\d)/(\d\d)/(\d\d)$`)

// Create makes an empty store at root in the layout of the old product:
// metadata.properties with the type line of a file store, content/,
// content/tmp/ and reconciliation/. What exists stays as it is.
func Create(root string, now time.Time) error {
	for _, dir := range []string{root, filepath.Join(root, stores.ContentDir, TmpDir), filepath.Join(root, ReconciliationDir)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	meta := filepath.Join(root, metadataFile)
	if _, err := os.Lstat(meta); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	data, err := Properties(map[string]string{"type": fileStoreType}, now)
	if err != nil {
		return err
	}
	work := filepath.Join(root, stores.ContentDir, TmpDir, "tmp$"+randomUUID()+"."+randomUUID()+stores.PropertiesSuffix)
	if err := writeSynced(work, data); err != nil {
		return err
	}
	if err := os.Rename(work, meta); err != nil {
		_ = os.Remove(work) // the rename error is the one returned
		return err
	}
	return syncDir(root)
}

// Open starts the one writer of a store with the writable flag. It refuses
// any other store before it touches a file (W1), takes an advisory lock on
// the open content/tmp directory and refuses to start while another writer
// holds it (W12), and removes the work files a stopped writer left there
// (W2).
func Open(s *stores.BlobStore, opts Options) (*Writer, error) {
	if !s.Writable() {
		return nil, fmt.Errorf("store %s is read only; blobs are written into the write store of this server only", s.Name())
	}
	root := s.Root()
	if info, err := os.Stat(filepath.Join(root, stores.ContentDir)); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("store %s at %s has no %s/ directory", s.Name(), root, stores.ContentDir)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(root, stores.ContentDir, TmpDir)
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return nil, err
	}
	lock, err := lockDir(tmp)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("store %s has another writer: %s is locked", s.Name(), tmp)
	}
	if err != nil {
		return nil, err
	}
	w := &Writer{name: s.Name(), root: root, realRoot: realRoot, now: opts.Now, newID: opts.NewID, lock: lock}
	if w.now == nil {
		w.now = time.Now
	}
	if w.newID == nil {
		w.newID = randomUUID
	}
	if err := removeWorkFiles(tmp); err != nil {
		_ = w.Close() // the clean up error is the one returned
		return nil, err
	}
	return w, nil
}

// lockDir opens a directory and takes an exclusive advisory lock on it
// without waiting. The lock lasts until the file is closed.
func lockDir(dir string) (*os.File, error) {
	f, err := os.Open(dir) // #nosec G304 -- content/tmp of a store root with the writable flag.
	if err != nil {
		return nil, err
	}
	conn, err := f.SyscallConn()
	if err == nil {
		var lockErr error
		err = conn.Control(func(fd uintptr) {
			lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a file descriptor fits in an int.
		})
		if err == nil {
			err = lockErr
		}
	}
	if err != nil {
		_ = f.Close() // the lock error is the one returned
		return nil, err
	}
	return f, nil
}

// workFile is the name of a work file: tmp$<id>.<id>.bytes or .properties.
func workFile(name string) bool {
	stem, ok := strings.CutPrefix(name, "tmp$")
	if !ok {
		return false
	}
	if s, ok := strings.CutSuffix(stem, stores.BytesSuffix); ok {
		stem = s
	} else if s, ok := strings.CutSuffix(stem, stores.PropertiesSuffix); ok {
		stem = s
	} else {
		return false
	}
	id, suffix, ok := strings.Cut(stem, ".")
	return ok && validID(id) && validID(suffix)
}

// removeWorkFiles removes the regular files of tmp named as work files.
func removeWorkFiles(tmp string) error {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !workFile(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(tmp, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// blob is the files of the blob at a location of a store.
type blob struct {
	dir, id      string // the directory relative to the root, slash form; the blob id
	bytes, props string // the two files
	propsData    []byte
	items        map[string]string
	parsed       *blobprops.BlobProps
	bytesSize    int64
}

// blobAt reads the blob whose bytes file is at location: content/<...>/<id>.bytes
// relative to the root, outside content/tmp and content/directpath. Both
// files are resolved and must lie inside the root.
func (w *Writer) blobAt(location string) (*blob, error) {
	parts := strings.Split(location, "/")
	bad := len(parts) < 3 || parts[0] != stores.ContentDir || parts[1] == TmpDir || parts[1] == directPathDir ||
		path.Clean(location) != location || strings.ContainsAny(location, "\\\x00")
	for _, part := range parts {
		bad = bad || part == "" || part == "." || part == ".."
	}
	id, isBytes := strings.CutSuffix(parts[len(parts)-1], stores.BytesSuffix)
	if bad || !isBytes || id == "" {
		return nil, fmt.Errorf("%q is no location of a blob of store %s", location, w.name)
	}
	b := &blob{dir: path.Dir(location), id: id}
	var err error
	if b.bytes, err = w.inside(location); err != nil {
		return nil, err
	}
	if b.props, err = w.inside(b.dir + "/" + id + stores.PropertiesSuffix); err != nil {
		return nil, err
	}
	info, err := os.Stat(b.bytes)
	if err != nil {
		return nil, err
	}
	b.bytesSize = info.Size()
	f, err := os.Open(b.props) // #nosec G304 -- resolved and checked against the store root by inside.
	if err != nil {
		return nil, err
	}
	b.propsData, err = io.ReadAll(io.LimitReader(f, stores.MaxPropertiesBytes+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	if len(b.propsData) > stores.MaxPropertiesBytes {
		return nil, fmt.Errorf("the properties of %s are larger than %d bytes", location, stores.MaxPropertiesBytes)
	}
	// A properties file is ISO 8859-1: each byte is one character.
	text := make([]rune, len(b.propsData))
	for i, c := range b.propsData {
		text[i] = rune(c)
	}
	if b.items, err = blobprops.Load(string(text)); err != nil {
		return nil, fmt.Errorf("the properties of %s cannot be read: %w", location, err)
	}
	if b.parsed, err = blobprops.Parse(b.propsData); err != nil {
		return nil, err
	}
	if _, isCopy := b.items["originalLocation"]; isCopy || b.parsed == nil {
		return nil, fmt.Errorf("%s is no blob: its properties are a copy or lack a required key", location)
	}
	return b, nil
}

// inside resolves a location below the root and refuses one whose real path
// is a link out of the root or no regular file.
func (w *Writer) inside(location string) (string, error) {
	abs, err := filepath.Abs(w.file(location))
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(real, w.realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%s lies outside store %s", location, w.name)
	}
	info, err := os.Lstat(real)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is no regular file", location)
	}
	return real, nil
}

// MarkDeleted writes what the old product writes for a deleted blob (W5):
// when the directory of the current minute is not the one of the blob, a
// copy of the properties there with originalLocation, the date of the
// blob's directory or "" for the old directory scheme; then the four keys
// deleted, deletedReason, deletedDateTime and softDeletedLocation in the
// properties of the blob, replaced through a work file. No file is removed.
// A blob that carries the key deleted already stays as it is.
func (w *Writer) MarkDeleted(location, reason string) error {
	if err := w.open(); err != nil {
		return err
	}
	w.markMu.Lock()
	defer w.markMu.Unlock()
	b, err := w.blobAt(location)
	if err != nil {
		return err
	}
	if _, deleted := b.items["deleted"]; deleted {
		return nil
	}
	if !utf8.ValidString(reason) {
		return fmt.Errorf("delete reason %q is not UTF-8", reason)
	}
	at := w.now()
	minute := minuteDir(at)
	if b.dir != minute {
		original := ""
		if m := dateDir.FindStringSubmatch(b.dir); m != nil {
			original = m[1] + "-" + m[2] + "-" + m[3] + "T" + m[4] + ":" + m[5]
		}
		items := map[string]string{"originalLocation": original}
		for key, value := range b.items {
			items[key] = value
		}
		if err := w.mkdirs(minute); err != nil {
			return err
		}
		if err := w.replace(minute+"/"+b.id+stores.PropertiesSuffix, items, at); err != nil {
			return err
		}
	}
	items := map[string]string{
		"deleted":             "true",
		"deletedReason":       reason,
		"deletedDateTime":     strconv.FormatInt(at.UnixMilli(), 10),
		"softDeletedLocation": at.UTC().Format("2006-01-02T15:04"),
	}
	for key, value := range b.items {
		if _, set := items[key]; !set {
			items[key] = value
		}
	}
	return w.replace(b.dir+"/"+b.id+stores.PropertiesSuffix, items, at)
}

// replace writes a properties file through a work file in content/tmp and a
// rename over the target, then syncs the directory of the target.
func (w *Writer) replace(rel string, items map[string]string, at time.Time) error {
	data, err := Properties(items, at)
	if err != nil {
		return err
	}
	work := w.file(stores.ContentDir + "/" + TmpDir + "/tmp$" + randomUUID() + "." + randomUUID() + stores.PropertiesSuffix)
	if err := writeSynced(work, data); err != nil {
		return err
	}
	if err := os.Rename(work, w.file(rel)); err != nil {
		_ = os.Remove(work) // the rename error is the one returned
		return err
	}
	return syncDir(w.file(path.Dir(rel)))
}

// Link makes a second blob of the bytes file at location: a hard link in
// the directory of the current minute with a properties file of its own
// and a line in the reconciliation log (section Layout, row Copy). Size and
// SHA-1 come from the properties of the source, which must be a live blob
// of this store.
func (w *Writer) Link(location string, a stores.Attributes) (stores.Written, error) {
	if err := w.open(); err != nil {
		return stores.Written{}, err
	}
	if err := checkAttributes(a); err != nil {
		return stores.Written{}, err
	}
	source, err := w.blobAt(location)
	if err != nil {
		return stores.Written{}, err
	}
	if source.parsed.Deleted || source.parsed.SHA1 == nil || source.parsed.Size != source.bytesSize {
		return stores.Written{}, fmt.Errorf("%s is no live blob with its size and SHA-1", location)
	}
	id, at, err := w.draw()
	if err != nil {
		return stores.Written{}, err
	}
	u := w.newUpload(id, at)
	u.tmpBytes = ""
	u.size, u.digests = source.bytesSize, stores.Digests{SHA1: *source.parsed.SHA1}
	var dir string
	err = w.step(StepRenameBytes, func() (err error) {
		dir, err = u.place(func(final string) error { return os.Link(source.bytes, final) })
		return err
	})
	var written stores.Written
	if err == nil {
		written, err = u.finish(a, dir)
	}
	u.done = true
	if err != nil {
		return stores.Written{}, errors.Join(err, u.cleanup())
	}
	return written, nil
}
