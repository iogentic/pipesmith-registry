package blobwrite

import (
	"crypto/md5" // #nosec G501 -- the MD5 digest that Maven and npm clients publish, not a security use.
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- the SHA-1 the blob format stores, not a security use.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The layout of a file blob store besides the names of package stores.
const (
	// TmpDir holds the work files below content/ (W2).
	TmpDir = "tmp"
	// ReconciliationDir holds one log file per day.
	ReconciliationDir = "reconciliation"

	createdByKey   = "@BlobStore.created-by"
	createdByIPKey = "@BlobStore.created-by-ip"

	// maxTries is the number of blob ids a write draws before it gives up.
	maxTries   = 8
	copyBuffer = 256 * 1024
)

var errNoFreeID = fmt.Errorf("no free blob id after %d tries", maxTries)

// Step is one of the seven steps of a write, in their order. A test hooks
// in between them.
type Step int

const (
	StepCreate           Step = iota + 1 // the work file of the bytes in content/tmp
	StepStream                           // the body into it, with the four digests
	StepSyncBytes                        // the sync of the bytes
	StepRenameBytes                      // the bytes into the directory of the minute
	StepProperties                       // the work file of the properties, synced
	StepRenameProperties                 // the properties next to the bytes
	StepLog                              // the line of the reconciliation log
)

var stepNames = [...]string{"", "create", "stream", "sync bytes", "rename bytes", "write properties", "rename properties", "log"}

func (s Step) String() string {
	if s > 0 && int(s) < len(stepNames) {
		return stepNames[s]
	}
	return "step " + strconv.Itoa(int(s))
}

// Options set the clock and the id source of a Writer; tests fix both.
type Options struct {
	Now   func() time.Time // time.Now when nil
	NewID func() string    // a random lower case UUID when nil
}

// Writer writes new blobs into one file blob store with the writable flag,
// and marks blobs of it deleted. It is safe for use by many goroutines.
type Writer struct {
	name, root string
	realRoot   string // the root with every link resolved
	now        func() time.Time
	newID      func() string
	placeMu    sync.Mutex // the check that a bytes file is free, and its rename
	logMu      sync.Mutex // one append to the reconciliation log at a time
	markMu     sync.Mutex // one delete mark at a time
	lockMu     sync.Mutex
	lock       *os.File // the open content/tmp with the lock of W12; nil once closed
	heldMu     sync.Mutex
	held       map[string]int // the work files of content/tmp that running uploads hold (ruling KN15)

	after func(Step)       // tests: called after each step
	fault func(Step) error // tests: an error in place of a step
}

var _ stores.Writer = (*Writer)(nil)

// Name is the name of the store.
func (w *Writer) Name() string { return w.name }

// Root is the root of the store.
func (w *Writer) Root() string { return w.root }

// Close releases the lock, so that another writer may start on the store.
// Every call after it fails; a second Close does nothing.
func (w *Writer) Close() error {
	w.lockMu.Lock()
	defer w.lockMu.Unlock()
	if w.lock == nil {
		return nil
	}
	err := w.lock.Close()
	w.lock = nil
	return err
}

func (w *Writer) open() error {
	w.lockMu.Lock()
	defer w.lockMu.Unlock()
	if w.lock == nil {
		return fmt.Errorf("the writer of store %s is closed", w.name)
	}
	return nil
}

// randomUUID is a random lower case UUID of version 4, 36 characters.
func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// validID is a lower case UUID: it becomes a file name as it is.
func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// minuteDir is content/yyyy/MM/dd/HH/mm of a moment in UTC.
func minuteDir(t time.Time) string {
	return path.Join(stores.ContentDir, t.UTC().Format("2006/01/02/15/04"))
}

// file is a path below the root, given in slash form.
func (w *Writer) file(rel string) string {
	return filepath.Join(w.root, filepath.FromSlash(rel))
}

func (w *Writer) step(s Step, fn func() error) error {
	if w.fault != nil {
		if err := w.fault(s); err != nil {
			return err
		}
	}
	if err := fn(); err != nil {
		return err
	}
	if w.after != nil {
		w.after(s)
	}
	return nil
}

// draw takes a blob id whose bytes file does not exist in the directory of
// the minute it is drawn in; at most maxTries ids.
func (w *Writer) draw() (string, time.Time, error) {
	for range maxTries {
		id, at := w.newID(), w.now()
		if !validID(id) {
			return "", time.Time{}, fmt.Errorf("blob id %q is no lower case UUID", id)
		}
		_, err := os.Lstat(w.file(minuteDir(at) + "/" + id + stores.BytesSuffix))
		if errors.Is(err, fs.ErrNotExist) {
			return id, at, nil
		}
		if err != nil {
			return "", time.Time{}, err
		}
	}
	return "", time.Time{}, errNoFreeID
}

// checkAttributes refuses attributes a blob cannot carry.
func checkAttributes(a stores.Attributes) error {
	switch {
	case a.Repo == "":
		return errors.New("a blob needs the name of its repository")
	case a.Path == "" || strings.HasPrefix(a.Path, "/"):
		return fmt.Errorf("blob path %q is empty or starts with a slash", a.Path)
	case a.CreatedBy == "":
		return errors.New("a blob needs the name of the user who writes it")
	}
	for _, text := range []string{a.Repo, a.Path, a.ContentType, a.CreatedBy, a.CreatedByIP} {
		if !utf8.ValidString(text) {
			return fmt.Errorf("attribute %q is not UTF-8", text)
		}
	}
	return nil
}

// Write streams body into a new blob: Upload without a limit, then Commit.
func (w *Writer) Write(body io.Reader, a stores.Attributes) (stores.Written, error) {
	if err := checkAttributes(a); err != nil {
		return stores.Written{}, err
	}
	u, err := w.Upload(body, -1)
	if err != nil {
		return stores.Written{}, err
	}
	defer u.Discard()
	return u.Commit(a)
}

// upload is the stores.Upload of a Writer.
type upload struct {
	w                  *Writer
	id                 string
	drawn              time.Time // the moment the id was drawn; its minute is the directory
	tmpBytes, tmpProps string    // the work files
	file               *os.File  // the open work file of the bytes
	placedBytes        string    // the bytes file once renamed, for the clean up
	placedProps        string    // the properties file once renamed
	size               int64
	digests            stores.Digests
	done               bool
	unhold             func()             // gives the work files back; nil once given
	prepared           *stores.Attributes // the attributes of Prepare; nil before it
	created            int64              // the creation time Prepare wrote
}

// newUpload names the work files of a blob as the old product names them:
// the blob id and a second random id.
func (w *Writer) newUpload(id string, at time.Time) *upload {
	work := stores.ContentDir + "/" + TmpDir + "/tmp$" + id + "." + randomUUID()
	u := &upload{w: w, id: id, drawn: at, tmpBytes: w.file(work + stores.BytesSuffix), tmpProps: w.file(work + stores.PropertiesSuffix)}
	bytesHeld, propsHeld := w.Hold(filepath.Base(u.tmpBytes)), w.Hold(filepath.Base(u.tmpProps))
	u.unhold = func() { bytesHeld(); propsHeld() }
	return u
}

// release gives the work files of the upload back.
func (u *upload) release() {
	if u.unhold != nil {
		u.unhold()
		u.unhold = nil
	}
}

// Upload streams body into a work file in content/tmp while it computes
// MD5, SHA-1, SHA-256 and SHA-512, and syncs it, so that Commit only renames
// and writes the small files. A body of more than limit bytes fails with
// stores.ErrTooLarge; a negative limit sets none. On an error the work file
// is gone.
func (w *Writer) Upload(body io.Reader, limit int64) (stores.Upload, error) {
	if err := w.open(); err != nil {
		return nil, err
	}
	id, at, err := w.draw()
	if err != nil {
		return nil, err
	}
	u := w.newUpload(id, at)
	err = w.step(StepCreate, func() error {
		f, err := os.OpenFile(u.tmpBytes, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- content/tmp of the store root and a name made of two UUIDs.
		u.file = f
		return err
	})
	if err == nil {
		err = w.step(StepStream, func() error { return u.stream(body, limit) })
	}
	if err == nil {
		err = w.step(StepSyncBytes, u.syncBytes)
	}
	if err != nil {
		u.cleanup()
		return nil, err
	}
	return u, nil
}

// syncBytes syncs and closes the work file of the bytes.
func (u *upload) syncBytes() error {
	err := u.file.Sync()
	if closeErr := u.file.Close(); err == nil {
		err = closeErr
	}
	u.file = nil
	return err
}

func (u *upload) stream(body io.Reader, limit int64) error {
	m, s1, s256, s512 := md5.New(), sha1.New(), sha256.New(), sha512.New() // #nosec G401 -- see the imports.
	src := body
	if limit >= 0 {
		src = io.LimitReader(body, limit+1)
	}
	n, err := io.CopyBuffer(io.MultiWriter(u.file, m, s1, s256, s512), src, make([]byte, copyBuffer))
	if err != nil {
		return err
	}
	if limit >= 0 && n > limit {
		return stores.ErrTooLarge
	}
	u.size = n
	u.digests = stores.Digests{MD5: hex.EncodeToString(m.Sum(nil)), SHA1: hex.EncodeToString(s1.Sum(nil)),
		SHA256: hex.EncodeToString(s256.Sum(nil)), SHA512: hex.EncodeToString(s512.Sum(nil))}
	return nil
}

func (u *upload) Size() int64             { return u.size }
func (u *upload) Digests() stores.Digests { return u.digests }

func (u *upload) Discard() error {
	if u.done {
		return nil
	}
	u.done = true
	return u.cleanup()
}

// cleanup closes the work file and removes every file of the upload, the
// properties before the bytes.
func (u *upload) cleanup() error {
	var errs []error
	if u.file != nil {
		errs = append(errs, u.file.Close())
		u.file = nil
	}
	for _, name := range []string{u.placedProps, u.placedBytes, u.tmpProps, u.tmpBytes} {
		if name == "" {
			continue
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	u.placedProps, u.placedBytes = "", ""
	u.release()
	return errors.Join(errs...)
}

// Prepare writes and syncs the properties work file of the attributes, so
// that Commit with the same attributes only renames and appends the log
// line. The creation time is taken here.
func (u *upload) Prepare(a stores.Attributes) error {
	if u.done || u.prepared != nil {
		return errors.New("the upload is committed, discarded or prepared")
	}
	if err := checkAttributes(a); err != nil {
		return err
	}
	data, created, err := u.properties(a)
	if err != nil {
		return err
	}
	if err := u.w.step(StepProperties, func() error { return writeSynced(u.tmpProps, data) }); err != nil {
		return err
	}
	u.prepared, u.created = &a, created
	return nil
}

func (u *upload) Commit(a stores.Attributes) (stores.Written, error) {
	if u.done {
		return stores.Written{}, errors.New("the upload is committed or discarded")
	}
	if err := checkAttributes(a); err != nil {
		return stores.Written{}, err
	}
	var written stores.Written
	err := errors.New("the attributes differ from those of Prepare")
	if u.prepared == nil || *u.prepared == a {
		written, err = u.commit(a)
	}
	u.done = true
	if err != nil {
		if cleanErr := u.cleanup(); cleanErr != nil {
			err = errors.Join(err, cleanErr)
		}
		return stores.Written{}, err
	}
	return written, nil
}

func (u *upload) commit(a stores.Attributes) (stores.Written, error) {
	w := u.w
	var dir string
	err := w.step(StepRenameBytes, func() (err error) {
		dir, err = u.place(func(final string) error { return os.Rename(u.tmpBytes, final) })
		return err
	})
	if err != nil {
		return stores.Written{}, err
	}
	return u.finish(a, dir)
}

// properties is the properties file of the attributes and its creation
// time, which is taken here, after the upload.
func (u *upload) properties(a stores.Attributes) ([]byte, int64, error) {
	at := u.w.now()
	created := at.UnixMilli()
	if a.NotBefore != 0 && created < a.NotBefore {
		created = a.NotBefore + 1
	}
	if a.NewerThan != 0 && created <= a.NewerThan {
		created = a.NewerThan + 1
	}
	items := map[string]string{
		blobprops.NameKey: "/" + a.Path,
		createdByKey:      a.CreatedBy,
		blobprops.RepoKey: a.Repo,
		"creationTime":    strconv.FormatInt(created, 10),
		"sha1":            u.digests.SHA1,
		"size":            strconv.FormatInt(u.size, 10),
	}
	if a.ContentType != "" {
		items[blobprops.TypeKey] = a.ContentType
	}
	if a.CreatedByIP != "" {
		items[createdByIPKey] = a.CreatedByIP
	}
	data, err := Properties(items, at)
	return data, created, err
}

// finish places the properties file next to the placed bytes file, after
// it writes it unless Prepare did, and the line of the reconciliation log.
func (u *upload) finish(a stores.Attributes, dir string) (stores.Written, error) {
	w := u.w
	created := u.created
	if u.prepared == nil {
		data, at, err := u.properties(a)
		if err != nil {
			return stores.Written{}, err
		}
		if err := w.step(StepProperties, func() error { return writeSynced(u.tmpProps, data) }); err != nil {
			return stores.Written{}, err
		}
		created = at
	}
	err := w.step(StepRenameProperties, func() error {
		final := w.file(dir + "/" + u.id + stores.PropertiesSuffix)
		if err := os.Rename(u.tmpProps, final); err != nil {
			return err
		}
		u.placedProps = final
		return syncDir(w.file(dir))
	})
	if err != nil {
		return stores.Written{}, err
	}
	if err := w.step(StepLog, func() error { return w.log(u.id) }); err != nil {
		return stores.Written{}, err
	}
	// The blob is complete: no clean up may remove it now.
	u.placedProps, u.placedBytes = "", ""
	u.release()
	sha := u.digests.SHA1
	entry := stores.Entry{Repo: a.Repo, Path: a.Path, Size: u.size, SHA1: &sha, CreatedMS: created,
		Location: dir + "/" + u.id + stores.BytesSuffix}
	if a.ContentType != "" {
		contentType := a.ContentType
		entry.ContentType = &contentType
	}
	return stores.Written{Entry: entry, ID: u.id, Digests: u.digests}, nil
}

// place puts the bytes file into the directory of the minute of its id
// through put, a rename or a link. An id whose bytes file appeared meanwhile
// gives way to a new one.
func (u *upload) place(put func(final string) error) (string, error) {
	w := u.w
	w.placeMu.Lock()
	defer w.placeMu.Unlock()
	for tries := 1; ; tries++ {
		_, err := os.Lstat(w.file(minuteDir(u.drawn) + "/" + u.id + stores.BytesSuffix))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		if tries >= maxTries {
			return "", errNoFreeID
		}
		if u.id, u.drawn = w.newID(), w.now(); !validID(u.id) {
			return "", fmt.Errorf("blob id %q is no lower case UUID", u.id)
		}
	}
	dir := minuteDir(u.drawn)
	if err := w.mkdirs(dir); err != nil {
		return "", err
	}
	final := w.file(dir + "/" + u.id + stores.BytesSuffix)
	if err := put(final); err != nil {
		return "", err
	}
	u.placedBytes = final
	return dir, syncDir(w.file(dir))
}

// mkdirs makes every missing directory of rel below the root and syncs the
// parent of each one it made.
func (w *Writer) mkdirs(rel string) error {
	current := w.root
	for _, part := range strings.Split(rel, "/") {
		parent := current
		current = filepath.Join(current, part)
		err := os.Mkdir(current, 0o750)
		switch {
		case err == nil:
			if err := syncDir(parent); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrExist):
			return err
		}
	}
	return nil
}

// log appends "yyyy-MM-dd HH:mm:ss,<id>,true" to reconciliation/<yyyy-MM-dd>,
// both in UTC (W9).
func (w *Writer) log(id string) error {
	at := w.now().UTC()
	line := at.Format("2006-01-02 15:04:05") + "," + id + ",true\n"
	w.logMu.Lock()
	defer w.logMu.Unlock()
	if err := w.mkdirs(ReconciliationDir); err != nil {
		return err
	}
	f, err := os.OpenFile(w.file(ReconciliationDir+"/"+at.Format("2006-01-02")), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) // #nosec G304 -- the log of the day below the store root.
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, line)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// writeSynced writes data to a new file and syncs it; on an error the file
// is gone.
func writeSynced(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a work file in content/tmp of the store root.
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name) // the write error is the one returned
	}
	return err
}

// syncDir syncs a directory, so that a rename in it lasts. A file system
// that cannot sync a directory says so with one of the errors below; its
// renames are what it gives.
func syncDir(dir string) error {
	f, err := os.Open(dir) // #nosec G304 -- a directory below the store root.
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	for _, unsupported := range []error{syscall.EINVAL, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOTTY} {
		if errors.Is(err, unsupported) {
			return nil
		}
	}
	return err
}
