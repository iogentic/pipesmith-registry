// Package catalog is catalog.sqlite of the data directory: what the REST
// answers need beyond index.sqlite. Version 1 holds one row per store and
// path in blobs (size, SHA-1, content type, creation time, uploader and
// address as the properties file gives them, and the other digests a write
// computed), the winning blob of each path in assets with the format and the
// component, the coordinates in components, and the rows per store in scans.
// Version 2 adds imported_assets, which stays empty until the import of
// phase 2 fills it.
//
// The search tables of the phase 1c plan (terms) are left out on purpose:
// how search works on an index of millions of rows is an open decision of the
// owner, and the table of that choice arrives later as a version of its own.
//
// The file is side data. A scan fills blobs, assets and components of a
// store again from the store, in one transaction per store after the index
// has the rows of that store, so a reader sees the old rows or the new rows.
// index.sqlite stays the source of every file answer. A failing migration
// removes a file the same open created; a file that existed stays as the
// runner left it, with the copy catalog.sqlite.v<old> when a later step
// failed. A file of a newer schema is refused and kept.
//
// One writer connection in WAL mode, foreign keys on, a busy timeout; readers
// use a pool of their own. Writes of one opening run one at a time, and each
// waits for the one before it at most as long as the busy timeout, as a
// write of another opening waits in SQLite.
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
	_ "modernc.org/sqlite" // the driver "sqlite"
)

const (
	// FileName is the file in the data directory.
	FileName = "catalog.sqlite"

	busyTimeoutMS = 30_000
	readerPool    = 16
	walSizeLimit  = 64 * 1024 * 1024
	writeCacheKiB = 131_072
)

var bg = context.Background()

// writeWait is how long a write waits for the write before it; a variable
// so that a test can shorten it.
var writeWait = busyTimeoutMS * time.Millisecond

// ErrBusy is the error of a write that waited writeWait for the write
// before it.
var ErrBusy = errors.New("catalog: another write held the catalog file longer than the busy timeout")

// Catalog is one open catalog file. It is safe for use by several goroutines;
// writes run one at a time.
type Catalog struct {
	path    string
	dataDir string
	writer  *sql.DB
	readers *sql.DB
	// writing holds a token while a write runs (lock, unlock)
	writing chan struct{}
}

// Path is the catalog file of a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Open opens the catalog file of dataDir, creates it with mode 0600 when it
// does not exist, and brings it to SchemaVersion.
func Open(dataDir string) (*Catalog, error) {
	return openWith(Path(dataDir), steps)
}

// dsn is a file: URI, so that any character of the path reaches SQLite.
func dsn(path string, params url.Values) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?" + params.Encode(), nil
}

// openWith opens path and brings it to the last of the steps given. When the
// open created the file and a step fails, the file and its WAL files are
// removed; a file that existed is kept.
func openWith(path string, steps []migrate.Step) (*Catalog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	created := false
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the catalog file of the data directory the operator configured.
	switch {
	case err == nil:
		created = true
		if err := f.Close(); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrExist):
	default:
		return nil, err
	}
	c, err := connect(path, steps)
	var newer *migrate.NewerError
	if err != nil && created && !errors.As(err, &newer) {
		err = errors.Join(err, remove(path))
	}
	return c, err
}

// connect opens both pools and runs the steps.
func connect(path string, steps []migrate.Step) (c *Catalog, err error) {
	busy := fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)
	writerDSN, err := dsn(path, url.Values{"mode": {"rw"}, "_txlock": {"immediate"}, "_pragma": {busy, "foreign_keys(1)",
		"synchronous(NORMAL)", fmt.Sprintf("journal_size_limit(%d)", walSizeLimit)}})
	if err != nil {
		return nil, err
	}
	readerDSN, err := dsn(path, url.Values{"_pragma": {busy, "query_only(1)"}})
	if err != nil {
		return nil, err
	}
	c = &Catalog{path: path, dataDir: filepath.Dir(path), writing: make(chan struct{}, 1)}
	if c.writer, err = sql.Open("sqlite", writerDSN); err != nil {
		return nil, err
	}
	c.writer.SetMaxOpenConns(1)
	if c.readers, err = sql.Open("sqlite", readerDSN); err != nil {
		_ = c.writer.Close() // cleanup; the open error is returned
		return nil, err
	}
	c.readers.SetMaxIdleConns(readerPool)
	if _, err := migrate.Run(c.writer, path, steps, migrate.Options{Kind: "catalog file", Create: true, CopyWhen: migrate.CopyRule(migrate.FreeSpace)}); err != nil {
		_ = c.Close() // cleanup; the migration error is returned
		return nil, err
	}
	return c, nil
}

// remove deletes a catalog file whose migration failed, with its WAL files,
// once no connection of this process holds it.
func remove(path string) error {
	var errs []error
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close closes both pools.
func (c *Catalog) Close() error {
	return errors.Join(c.readers.Close(), c.writer.Close())
}

// File is the path of the catalog file.
func (c *Catalog) File() string { return c.path }

// lock waits at most writeWait until no other write runs, and then holds
// the writer until unlock.
func (c *Catalog) lock() error {
	timer := time.NewTimer(writeWait)
	defer timer.Stop()
	select {
	case c.writing <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	}
}

func (c *Catalog) unlock() { <-c.writing }

// write runs fn in one write transaction on the writer connection.
func (c *Catalog) write(fn func(tx *sql.Tx) error) error {
	if err := c.lock(); err != nil {
		return err
	}
	defer c.unlock()
	return c.writeLocked(fn)
}

// writeLocked is write for a caller that holds the writer.
func (c *Catalog) writeLocked(fn func(tx *sql.Tx) error) error {
	tx, err := c.writer.BeginTx(bg, nil)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback() // only after a failed step, whose error is returned
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	done = true
	return tx.Commit()
}

// read runs fn in one read transaction on a pooled connection, so that it
// sees one snapshot.
func (c *Catalog) read(fn func(tx *sql.Tx) error) error {
	tx, err := c.readers.BeginTx(bg, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a read transaction has nothing to keep
	return fn(tx)
}
