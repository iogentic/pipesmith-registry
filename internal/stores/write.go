package stores

import (
	"errors"
	"io"
)

// The write side of a file blob store. internal/blobwrite writes; this file
// holds the writable flag and the values the writer takes and gives, so
// that a caller of the write path needs no import of the writer itself. The
// read side above does not change.

// NewWriteStore returns the file blob store at root with the writable flag:
// the store this server writes its blobs into. A store of the old product
// never carries the flag (W1 and P8 of the phase plans), and Make never sets
// it.
func NewWriteStore(name, root string) (*BlobStore, error) {
	s, err := NewBlobStore(name, root)
	if err != nil {
		return nil, err
	}
	s.writable = true
	return s, nil
}

// Writable reports whether this process may write the store.
func (s *BlobStore) Writable() bool { return s.writable }

// Writable reports whether s is a file blob store with the writable flag.
func Writable(s Store) bool {
	b, ok := s.(*BlobStore)
	return ok && b.writable
}

// Digests are the lower case hex digests of one body, computed in one pass
// while the body is written.
type Digests struct {
	MD5, SHA1, SHA256, SHA512 string
}

// Attributes are what the properties file of a new blob says besides size,
// sha1 and creationTime.
type Attributes struct {
	Repo        string // @Bucket.repo-name
	Path        string // the asset path without a leading slash; @BlobStore.blob-name is "/" + Path
	ContentType string // @BlobStore.content-type; "" leaves the key out
	CreatedBy   string // @BlobStore.created-by: the name of the user who wrote it
	CreatedByIP string // @BlobStore.created-by-ip; "" leaves the key out
	// NewerThan is a creation time in milliseconds the new blob must pass:
	// the creation time is the time of the commit, or NewerThan + 1 when that
	// is not later. Zero sets no bound.
	NewerThan int64
	// NotBefore is a creation time in milliseconds the new blob must not
	// fall behind: a time of the commit before it becomes NotBefore + 1, an
	// equal one stays. Zero sets no bound.
	NotBefore int64
}

// Written is a blob the writer committed: the entry a scan of the store
// gives for it, its blob id and the digests of its bytes.
type Written struct {
	Entry   Entry
	ID      string
	Digests Digests
}

// Upload is a body in a work file of a writable store, not a blob until
// Commit. Discard removes the work file; after Commit it does nothing. An
// Upload belongs to one goroutine.
type Upload interface {
	// Size is the number of bytes of the body.
	Size() int64
	// Digests are the digests of the body.
	Digests() Digests
	// Commit makes the body a blob with the attributes: its bytes file, which
	// the upload synced, and then its properties file in the directory of the
	// current minute, and a line in the reconciliation log. On an error no
	// file of it is left.
	Commit(a Attributes) (Written, error)
	// Discard removes the work file of an upload that is not committed.
	Discard() error
}

// Writer is the write side of a store with the writable flag. One process
// holds it per store; it is safe for use by many goroutines.
type Writer interface {
	// Name is the name of the store.
	Name() string
	// Upload streams body into a work file while it computes the digests.
	// More than limit bytes fail with ErrTooLarge; a negative limit sets none.
	Upload(body io.Reader, limit int64) (Upload, error)
	// Link makes a second blob of the bytes file at location, a hard link
	// with a properties file of its own. Its Digests hold the SHA-1 only.
	Link(location string, a Attributes) (Written, error)
	// MarkDeleted writes the delete mark into the properties of the blob at
	// location and removes no file. A blob marked already stays as it is.
	MarkDeleted(location, reason string) error
	// Close releases the store for another writer.
	Close() error
}

// ErrTooLarge is the error of an upload whose body passes its limit.
var ErrTooLarge = errors.New("the body is larger than the limit")
