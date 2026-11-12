// Package stores reads file blob stores and plain directory trees, read only.
package stores

import "os"

// Entry is one live path of a repository as a store holds it.
type Entry struct {
	Repo, Path        string
	Size              int64
	SHA1, ContentType *string // nil where the Python server has None
	CreatedMS         int64
	Location          string // the bytes file, relative to the store root
}

// ScanCounts counts what a walk saw, by kind.
type ScanCounts struct {
	Live, Deleted, Copies, NoBytes, Unparsable int
}

// Store is a file blob store or a directory tree.
type Store interface {
	Name() string
	Kind() string
	Root() string
	// Entries walks the store and calls yield for every live entry. It
	// returns an error when the root or the content directory is missing.
	Entries(counts *ScanCounts, yield func(Entry) error) error
	// Open opens a location of an entry. The error satisfies
	// errors.Is(err, fs.ErrNotExist) when the file is gone, lies outside the
	// root or is not a regular file.
	Open(location string) (*os.File, error)
}
