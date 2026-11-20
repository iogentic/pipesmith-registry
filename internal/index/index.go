// Package index is the SQLite index of the live paths of every attached store.
//
// Layout: every store gets two tables of its own, files_<id> with one row per
// path and dirs_<id> with one row per directory. Both are keyed by
// (repo, parent, name), so Lookup is one key probe per store and Children is
// one ordered range scan per store. ReplaceStore empties and refills the two
// tables of one store in one transaction. Readers use their own connections
// in WAL mode and see the old rows or the new rows, never a mix.
//
// The schema, the tables and the meta rows are those of the Python server,
// so a data directory indexed by one server is served by the other.
package index

import (
	"cmp"
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
	"modernc.org/sqlite"
)

const (
	// SchemaVersion is the value of the schema row of the meta table.
	SchemaVersion = 1
	// DefaultLimit is the default page size of children in Python.
	DefaultLimit = 10_000

	poolSize          = 16
	busyTimeoutMS     = 30_000
	dropBusyTimeoutMS = 250
	writeCacheKiB     = 131_072
	walSizeLimit      = 64 * 1024 * 1024
	dirCacheLimit     = 100_000
	sqliteBusy        = 5 // primary result codes of SQLite
	sqliteLocked      = 6
)

// ErrClosed is returned by every call after Close.
var ErrClosed = errors.New("the index is closed")

// ErrBusy is the error of a bounded write (PutWithin, DeleteWithin) that did
// not get the write lock of the index within its wait: another write held
// it, in this process or in another one, a scan for the whole walk of its
// store. The bounded write changed nothing.
var ErrBusy = errors.New("another write holds the index")

var schema = []string{
	"CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS stores (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)",
	"CREATE TABLE IF NOT EXISTS repo_counts (" +
		" store_id INTEGER NOT NULL, repo TEXT NOT NULL, files INTEGER NOT NULL, bytes INTEGER NOT NULL," +
		" newest_ms INTEGER NOT NULL, PRIMARY KEY (store_id, repo)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS hints (" +
		" store_id INTEGER NOT NULL, repo TEXT NOT NULL, format TEXT NOT NULL," +
		" PRIMARY KEY (store_id, repo)) WITHOUT ROWID",
}

// Asset is the winning row of a path: the newest, on a tie the store whose
// name sorts first.
type Asset struct {
	Repo, Path, Store, Location string
	Size                        int64
	SHA1, ContentType           *string // nil where the Python server has None
	CreatedMS                   int64
}

// Child is one name in a directory. Size and CreatedMS are nil for a
// directory.
type Child struct {
	Name            string
	IsDir           bool
	Size, CreatedMS *int64
}

// RepoStat counts the paths of a repository; a path in several stores counts
// once, with its winning row.
type RepoStat struct {
	Repo                   string
	Files, Bytes, NewestMS int64
}

// StoreStat counts the rows of one store.
type StoreStat struct {
	Store        string
	Files, Bytes int64
}

// Index is safe for use by many goroutines. Writes go through one writer
// connection, one at a time; every read call runs in one read transaction on
// a pooled connection and so sees one snapshot.
type Index struct {
	path     string
	readers  *sql.DB
	writerDB *sql.DB
	writer   *sql.Conn
	writeMu  sync.Mutex
	closed   atomic.Bool

	cacheMu    sync.Mutex
	cacheGen   int64
	cacheStats []RepoStat // nil: nothing cached
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var bg = context.Background()

func filesTable(id int64) string { return fmt.Sprintf("files_%d", id) }
func dirsTable(id int64) string  { return fmt.Sprintf("dirs_%d", id) }

// split is str.rpartition("/") without the separator.
func split(path string) (parent, name string) {
	cut := strings.LastIndexByte(path, '/')
	if cut < 0 {
		return "", path
	}
	return path[:cut], path[cut+1:]
}

func addressable(path string) bool {
	return path != "" && !strings.HasPrefix(path, "/") && !strings.HasSuffix(path, "/")
}

// pathText is str(pathlib.Path(p)) on POSIX: empty and "." parts dropped,
// ".." kept, a leading "//" kept.
func pathText(p string) string {
	lead := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		lead = "//"
	case strings.HasPrefix(p, "/"):
		lead = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if text := lead + strings.Join(parts, "/"); text != "" {
		return text
	}
	return "."
}

// dsn is a file: URI, so that any character of the path reaches SQLite.
func dsn(path string, params url.Values) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?" + params.Encode(), nil
}

// Open opens or creates the index at path and its parent directory. Every
// open is a write: it bumps the generation, as in Python. A process that
// writes the index opens it so; a process that reads it uses OpenRead.
func Open(path string) (*Index, error) {
	return open(path, (*Index).setUp)
}

// OpenRead opens the index at path for a process that reads it while
// another process may write it: a scan holds the write lock for as long as
// it reads its store. An existing file of schema 1 in WAL mode opens without
// a write, so the open neither waits for that lock nor changes the file, and
// every read answers the rows of the last finished write. Any other file
// gets the set up of Open, a write: a missing file, one without the tables
// or the meta rows, one in another journal mode. A file of another schema is
// refused with the error of Open. A write through the index waits for the
// lock as one of Open does.
func OpenRead(path string) (*Index, error) {
	return open(path, (*Index).attach)
}

func open(path string, prepare func(*Index) error) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	created, err := createPrivate(path)
	if err != nil {
		return nil, err
	}
	ix, err := openFile(path, prepare)
	if err != nil && created {
		_ = os.Remove(path) // cleanup of the empty file this call made; the open error is returned
	}
	return ix, err
}

// createPrivate creates an empty index file of mode 0600 when there is none
// (finding sa-21): SQLite gives the WAL and SHM side files the mode of the
// file. It reports whether it created the file.
func createPrivate(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the index file of the data directory the operator configured.
	switch {
	case err == nil:
		return true, f.Close()
	case errors.Is(err, fs.ErrExist):
		return false, nil
	}
	return false, err
}

func openFile(path string, prepare func(*Index) error) (*Index, error) {
	busy := fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)
	writerDSN, err := dsn(path, url.Values{"_pragma": {busy, "synchronous(NORMAL)"}, "_txlock": {"immediate"}})
	if err != nil {
		return nil, err
	}
	readerDSN, err := dsn(path, url.Values{"_pragma": {busy, "synchronous(NORMAL)", "query_only(1)"}})
	if err != nil {
		return nil, err
	}
	ix := &Index{path: path}
	if ix.writerDB, err = sql.Open("sqlite", writerDSN); err != nil {
		return nil, err
	}
	ix.writerDB.SetMaxOpenConns(1)
	if ix.readers, err = sql.Open("sqlite", readerDSN); err != nil {
		_ = ix.writerDB.Close() // cleanup; the open error is the one returned
		return nil, err
	}
	ix.readers.SetMaxIdleConns(poolSize)
	if err := prepare(ix); err != nil {
		_ = ix.Close() // cleanup; the set up error is the one returned
		return nil, err
	}
	return ix, nil
}

// setUp is the open of Python: WAL mode, then the tables and the meta rows
// in a write, which waits for the lock of another writer and bumps the
// generation.
func (ix *Index) setUp() error {
	if err := ix.connectWriter("PRAGMA journal_mode = WAL"); err != nil {
		return err
	}
	return ix.write(func(tx *sql.Tx) error {
		for _, statement := range schema {
			if _, err := tx.ExecContext(bg, statement); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(bg, "INSERT OR IGNORE INTO meta VALUES ('schema', ?), ('generation', 0)", SchemaVersion); err != nil {
			return err
		}
		var version any
		if err := tx.QueryRowContext(bg, "SELECT value FROM meta WHERE key = 'schema'").Scan(&version); err != nil {
			return err
		}
		return ix.checkSchema(version)
	})
}

// attach is the open of OpenRead: a file that holds what setUp writes is
// taken over after one read, any other file gets setUp.
func (ix *Index) attach() error {
	ready, err := ix.ready()
	if err != nil {
		return err
	}
	if !ready {
		return ix.setUp()
	}
	return ix.connectWriter()
}

// connectWriter takes the writer connection and runs the statements on it,
// then the size limit of the WAL file, which takes no lock.
func (ix *Index) connectWriter(statements ...string) error {
	var err error
	if ix.writer, err = ix.writerDB.Conn(bg); err != nil {
		return err
	}
	for _, statement := range append(statements, fmt.Sprintf("PRAGMA journal_size_limit = %d", walSizeLimit)) {
		if _, err := ix.writer.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	return nil
}

// ready reads, in one read transaction, whether the file holds what setUp
// writes: every table of schema, both meta rows and WAL mode. A file of
// another schema version is the error of setUp.
func (ix *Index) ready() (bool, error) {
	ready := false
	err := ix.read(func(tx *sql.Tx) error {
		tables := map[string]bool{}
		rows, err := tx.QueryContext(bg, "SELECT name FROM sqlite_master WHERE type = 'table'")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			tables[name] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !tables["meta"] {
			return nil
		}
		var version any
		err = tx.QueryRowContext(bg, "SELECT value FROM meta WHERE key = 'schema'").Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ix.checkSchema(version); err != nil {
			return err
		}
		var generations int
		if err := tx.QueryRowContext(bg, "SELECT count(*) FROM meta WHERE key = 'generation'").Scan(&generations); err != nil {
			return err
		}
		var mode string
		if err := tx.QueryRowContext(bg, "PRAGMA journal_mode").Scan(&mode); err != nil {
			return err
		}
		ready = generations == 1 && mode == "wal"
		for _, statement := range schema {
			ready = ready && tables[tableOf(statement)]
		}
		return nil
	})
	return ready, err
}

// tableOf is the name of the table a statement of schema creates.
func tableOf(statement string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(statement, "CREATE TABLE IF NOT EXISTS "), " ")
	return name
}

// checkSchema refuses a schema version other than SchemaVersion. Python
// compares with ==, so 1.0 matches as well.
func (ix *Index) checkSchema(version any) error {
	if v, ok := version.(int64); ok && v == SchemaVersion {
		return nil
	}
	if v, ok := version.(float64); ok && v == SchemaVersion {
		return nil
	}
	return fmt.Errorf("%s has index schema %v, expected %d; delete it and scan again", pathText(ix.path), version, SchemaVersion)
}

// Path is the file of the index as given to Open.
func (ix *Index) Path() string { return ix.path }

// read runs fn in one read transaction on a pooled connection.
func (ix *Index) read(fn func(tx *sql.Tx) error) error {
	if ix.closed.Load() {
		return ErrClosed
	}
	tx, err := ix.readers.BeginTx(bg, nil)
	if err != nil {
		if ix.closed.Load() {
			return ErrClosed
		}
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback() // the error of fn is the one returned
		return err
	}
	return tx.Commit()
}

// write runs fn in a write transaction on the writer connection, after the
// write lock.
func (ix *Index) write(fn func(tx *sql.Tx) error) error {
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	return ix.transaction(fn)
}

// lockPoll is the longest pause between two tries of lockWithin.
const lockPoll = 10 * time.Millisecond

// writeWithin is write with a bound on the wait for the write lock: first
// the lock of this process, then the lock of the file through the busy
// timeout of the writer connection, together at most wait. After it the
// result is ErrBusy and fn did not run, or its transaction rolled back.
func (ix *Index) writeWithin(wait time.Duration, fn func(tx *sql.Tx) error) (err error) {
	deadline := time.Now().Add(wait)
	for pause := time.Millisecond; !ix.writeMu.TryLock(); pause = min(2*pause, lockPoll) {
		left := time.Until(deadline)
		if left <= 0 {
			return ErrBusy
		}
		time.Sleep(min(pause, left))
	}
	defer ix.writeMu.Unlock()
	if ix.closed.Load() {
		return ErrClosed
	}
	left := max(time.Until(deadline), time.Millisecond)
	if _, err := ix.writer.ExecContext(bg, fmt.Sprintf("PRAGMA busy_timeout = %d", left.Milliseconds())); err != nil {
		return err
	}
	defer func() {
		if _, restore := ix.writer.ExecContext(bg, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMS)); restore != nil && err == nil {
			err = restore
		}
	}()
	err = ix.transaction(fn)
	if busy(err) {
		return ErrBusy
	}
	return err
}

// transaction is BEGIN IMMEDIATE, fn, the generation bump and COMMIT; on an
// error or a panic of fn it rolls back. The caller holds the write lock.
func (ix *Index) transaction(fn func(tx *sql.Tx) error) (err error) {
	if ix.closed.Load() {
		return ErrClosed
	}
	tx, err := ix.writer.BeginTx(bg, nil)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback() // only after a failed step, whose error is returned
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(bg, "UPDATE meta SET value = value + 1 WHERE key = 'generation'"); err != nil {
		return err
	}
	finished = true
	return tx.Commit()
}

type storeRow struct {
	name string
	id   int64
}

func storeList(q querier) ([]storeRow, error) {
	rows, err := q.QueryContext(bg, "SELECT name, id FROM stores ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storeRow
	for rows.Next() {
		var s storeRow
		if err := rows.Scan(&s.name, &s.id); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func storeID(q querier, store string) (int64, bool, error) {
	var id int64
	err := q.QueryRowContext(bg, "SELECT id FROM stores WHERE name = ?", store).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func storeIDOrNew(q querier, store string) (int64, error) {
	id, found, err := storeID(q, store)
	if err != nil || found {
		return id, err
	}
	result, err := q.ExecContext(bg, "INSERT INTO stores (name) VALUES (?)", store)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func optional(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// Writes

// storeTables makes the two tables of a store when they are missing.
func storeTables(tx *sql.Tx, id int64) (files, dirs string, err error) {
	files, dirs = filesTable(id), dirsTable(id)
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS " + files + " (" +
			" repo TEXT NOT NULL, parent TEXT NOT NULL, name TEXT NOT NULL, location TEXT NOT NULL," +
			" size INTEGER NOT NULL, sha1 TEXT, content_type TEXT, created_ms INTEGER NOT NULL," +
			" PRIMARY KEY (repo, parent, name)) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS " + dirs + " (" +
			" repo TEXT NOT NULL, parent TEXT NOT NULL, name TEXT NOT NULL," +
			" PRIMARY KEY (repo, parent, name)) WITHOUT ROWID",
	} {
		if _, err := tx.ExecContext(bg, statement); err != nil {
			return "", "", err
		}
	}
	return files, dirs, nil
}

// ReplaceStore replaces the rows of one store with the entries, in one
// transaction, and returns the rows written. Entries whose path is empty,
// starts or ends with "/" are skipped; of two entries of one path the newer
// wins. When entries returns an error, the old rows stay and ReplaceStore
// returns that error.
func (ix *Index) ReplaceStore(store string, entries func(yield func(stores.Entry) error) error) (int64, error) {
	return ix.ReplaceStoreWith(store, entries, nil)
}

// Arbiter takes part in ReplaceStoreWith for a store whose write order is
// known beyond the creation time of its entries: the store a server writes.
type Arbiter interface {
	// Newer reports whether next, met later in the walk than held and of the
	// same path, takes the row of that path.
	Newer(held, next stores.Entry) bool
	// Settle runs once after the last entry, in the same transaction, and
	// may drop rows of the store.
	Settle(rows Rows) error
}

// Rows are the rows of one store while ReplaceStoreWith writes them.
type Rows interface {
	// Row is the entry the row of a path holds, nil for none.
	Row(repo, path string) (*stores.Entry, error)
	// Drop removes the row of a path.
	Drop(repo, path string) error
}

// storeRows are the Rows of one files table inside a transaction.
type storeRows struct {
	tx    *sql.Tx
	files string
}

func (r storeRows) Row(repo, path string) (*stores.Entry, error) {
	parent, name := split(path)
	e := stores.Entry{Repo: repo, Path: path}
	var sha1, contentType sql.NullString
	err := r.tx.QueryRowContext(bg, "SELECT location, size, sha1, content_type, created_ms FROM "+r.files+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
		" WHERE repo = ? AND parent = ? AND name = ?", repo, parent, name).Scan(&e.Location, &e.Size, &sha1, &contentType, &e.CreatedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.SHA1, e.ContentType = optional(sha1), optional(contentType)
	return &e, nil
}

func (r storeRows) Drop(repo, path string) error {
	parent, name := split(path)
	_, err := r.tx.ExecContext(bg, "DELETE FROM "+r.files+" WHERE repo = ? AND parent = ? AND name = ?", repo, parent, name) // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
	return err
}

// ReplaceStoreWith is ReplaceStore with an arbiter: of two entries of one
// path the arbiter decides which takes the row, and after the last entry it
// may drop rows, before the directories and counts are written from the rows
// that stay. A nil arbiter is ReplaceStore.
func (ix *Index) ReplaceStoreWith(store string, entries func(yield func(stores.Entry) error) error, arbiter Arbiter) (int64, error) {
	var written int64
	err := ix.write(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(bg, fmt.Sprintf("PRAGMA cache_size = -%d", writeCacheKiB)); err != nil {
			return err
		}
		id, err := storeIDOrNew(tx, store)
		if err != nil {
			return err
		}
		files, dirs, err := storeTables(tx, id)
		if err != nil {
			return err
		}
		for _, statement := range []string{"DELETE FROM " + files, "DELETE FROM " + dirs} {
			if _, err := tx.ExecContext(bg, statement); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(bg, "DELETE FROM repo_counts WHERE store_id = ?", id); err != nil {
			return err
		}
		put, err := entryWriter(tx, files, arbiter)
		if err != nil {
			return err
		}
		defer put.close()
		var failed error
		err = entries(func(e stores.Entry) error {
			if failed != nil {
				return failed
			}
			if !addressable(e.Path) {
				return nil
			}
			failed = put.write(e)
			return failed
		})
		if err != nil {
			return err
		}
		if failed != nil {
			return failed
		}
		if arbiter != nil {
			if err := arbiter.Settle(storeRows{tx: tx, files: files}); err != nil {
				return err
			}
		}
		if err := insertDirs(tx, files, dirs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, "INSERT INTO repo_counts"+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
			" SELECT ?, repo, count(*), sum(size), max(created_ms) FROM "+files+" GROUP BY repo", id); err != nil {
			return err
		}
		return tx.QueryRowContext(bg, "SELECT coalesce(sum(files), 0) FROM repo_counts WHERE store_id = ?", id).Scan(&written)
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// entryWrites writes the entries of one walk into a files table.
type entryWrites struct {
	insert, update *sql.Stmt
	rows           storeRows
	arbiter        Arbiter
}

// entryWriter prepares the statements of ReplaceStoreWith. Without an
// arbiter an entry of a path takes the row when it is newer than the one the
// row holds; with one the arbiter decides.
func entryWriter(tx *sql.Tx, files string, arbiter Arbiter) (*entryWrites, error) {
	columns := "INSERT INTO " + files + " (repo, parent, name, location, size, sha1, content_type, created_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	conflict := " ON CONFLICT (repo, parent, name) DO UPDATE SET" +
		" location = excluded.location, size = excluded.size, sha1 = excluded.sha1," +
		" content_type = excluded.content_type, created_ms = excluded.created_ms" +
		" WHERE excluded.created_ms > " + files + ".created_ms"
	if arbiter != nil {
		conflict = " ON CONFLICT (repo, parent, name) DO NOTHING"
	}
	w := &entryWrites{rows: storeRows{tx: tx, files: files}, arbiter: arbiter}
	var err error
	if w.insert, err = tx.PrepareContext(bg, columns+conflict); err != nil { // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
		return nil, err
	}
	if arbiter != nil {
		w.update, err = tx.PrepareContext(bg, "UPDATE "+files+" SET location = ?, size = ?, sha1 = ?, content_type = ?, created_ms = ?"+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
			" WHERE repo = ? AND parent = ? AND name = ?")
		if err != nil {
			w.close()
			return nil, err
		}
	}
	return w, nil
}

func (w *entryWrites) write(e stores.Entry) error {
	parent, name := split(e.Path)
	result, err := w.insert.ExecContext(bg, e.Repo, parent, name, e.Location, e.Size, nullable(e.SHA1), nullable(e.ContentType), e.CreatedMS)
	if err != nil || w.arbiter == nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n > 0 {
		return err
	}
	held, err := w.rows.Row(e.Repo, e.Path)
	if err != nil || held == nil || !w.arbiter.Newer(*held, e) {
		return err
	}
	_, err = w.update.ExecContext(bg, e.Location, e.Size, nullable(e.SHA1), nullable(e.ContentType), e.CreatedMS, e.Repo, parent, name)
	return err
}

func (w *entryWrites) close() {
	for _, statement := range []*sql.Stmt{w.insert, w.update} {
		if statement != nil {
			_ = statement.Close() // the transaction ends with it; its error is the one returned
		}
	}
}

// insertDirs writes one row per directory of the files table, with every
// ancestor.
func insertDirs(tx *sql.Tx, files, dirs string) error {
	insert, err := tx.PrepareContext(bg, "INSERT OR IGNORE INTO "+dirs+" VALUES (?, ?, ?)") // #nosec G202 -- the table name is dirsTable of an integer store id; every value is a placeholder.
	if err != nil {
		return err
	}
	defer insert.Close()
	rows, err := tx.QueryContext(bg, "SELECT DISTINCT repo, parent FROM "+files+" WHERE parent <> ''") // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct{ repo, directory string }
	seen := map[key]struct{}{}
	for rows.Next() {
		var repo, directory string
		if err := rows.Scan(&repo, &directory); err != nil {
			return err
		}
		for directory != "" {
			if _, ok := seen[key{repo, directory}]; ok {
				break
			}
			seen[key{repo, directory}] = struct{}{}
			up, name := split(directory)
			if _, err := insert.ExecContext(bg, repo, up, name); err != nil {
				return err
			}
			directory = up
		}
		if len(seen) > dirCacheLimit {
			clear(seen) // only a cache; INSERT OR IGNORE keeps the rows unique
		}
	}
	return rows.Err()
}

// RemoveStore drops the tables, counts and hints of a store. An unknown
// store is no error.
func (ix *Index) RemoveStore(store string) error {
	return ix.write(func(tx *sql.Tx) error {
		id, found, err := storeID(tx, store)
		if err != nil || !found {
			return err
		}
		for _, statement := range []string{"DROP TABLE IF EXISTS " + filesTable(id), "DROP TABLE IF EXISTS " + dirsTable(id)} {
			if _, err := tx.ExecContext(bg, statement); err != nil {
				return err
			}
		}
		for _, statement := range []string{
			"DELETE FROM repo_counts WHERE store_id = ?",
			"DELETE FROM hints WHERE store_id = ?",
			"DELETE FROM stores WHERE id = ?",
		} {
			if _, err := tx.ExecContext(bg, statement, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetHints replaces the format hints of one store: repository -> format.
// Pairs with an empty repository or format are skipped.
func (ix *Index) SetHints(store string, hints map[string]string) error {
	return ix.write(func(tx *sql.Tx) error {
		id, err := storeIDOrNew(tx, store)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, "DELETE FROM hints WHERE store_id = ?", id); err != nil {
			return err
		}
		for _, repo := range slices.Sorted(maps.Keys(hints)) {
			if repo == "" || hints[repo] == "" {
				continue
			}
			if _, err := tx.ExecContext(bg, "INSERT INTO hints (store_id, repo, format) VALUES (?, ?, ?)", id, repo, hints[repo]); err != nil {
				return err
			}
		}
		return nil
	})
}

func busy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	code := e.Code() & 0xff
	return code == sqliteBusy || code == sqliteLocked
}

// Drop removes one row and the directories it leaves empty.
//
// Best effort: while another write of this index runs, the call returns at
// once and the row stays; while another process writes the file, it waits
// 250 ms and gives up. The running scan or the next request removes it.
func (ix *Index) Drop(repo, path, store string) error {
	_, err := ix.DropRow(repo, path, store)
	return err
}

// DropRow is Drop that reports whether it removed the row, so that the
// caller removes the side data of the row with it (catalog.Drop) only then.
func (ix *Index) DropRow(repo, path, store string) (dropped bool, err error) {
	if !addressable(path) || !ix.writeMu.TryLock() {
		return false, nil
	}
	defer ix.writeMu.Unlock()
	if ix.closed.Load() {
		return false, ErrClosed
	}
	if _, err := ix.writer.ExecContext(bg, fmt.Sprintf("PRAGMA busy_timeout = %d", dropBusyTimeoutMS)); err != nil {
		return false, err
	}
	defer func() {
		if _, restore := ix.writer.ExecContext(bg, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMS)); restore != nil && err == nil {
			err = restore
		}
	}()
	err = ix.transaction(func(tx *sql.Tx) error {
		removed, err := dropRow(tx, repo, path, store)
		dropped = removed != nil
		return err
	})
	if busy(err) {
		return false, nil
	}
	return dropped && err == nil, err
}

// rowOf is the row of a path in the files table of one store, nil for none.
func rowOf(tx *sql.Tx, files, store, repo, path string) (*Asset, error) {
	parent, name := split(path)
	a := Asset{Repo: repo, Path: path, Store: store}
	var sha1, contentType sql.NullString
	err := tx.QueryRowContext(bg, "SELECT location, size, sha1, content_type, created_ms FROM "+files+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
		" WHERE repo = ? AND parent = ? AND name = ?", repo, parent, name).Scan(&a.Location, &a.Size, &sha1, &contentType, &a.CreatedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.SHA1, a.ContentType = optional(sha1), optional(contentType)
	return &a, nil
}

// dropRow removes the row of a path from one store with the directories it
// leaves empty and keeps the counts of its repository; it returns the row,
// nil when there was none.
func dropRow(tx *sql.Tx, repo, path, store string) (*Asset, error) {
	id, found, err := storeID(tx, store)
	if err != nil || !found {
		return nil, err
	}
	files, dirs := filesTable(id), dirsTable(id)
	parent, name := split(path)
	removed, err := rowOf(tx, files, store, repo, path)
	if err != nil || removed == nil {
		return nil, err
	}
	if _, err := tx.ExecContext(bg, "DELETE FROM "+files+" WHERE repo = ? AND parent = ? AND name = ?", repo, parent, name); err != nil { // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
		return nil, err
	}
	exists := func(query string, args ...any) (bool, error) {
		var one int
		err := tx.QueryRowContext(bg, query, args...).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	for directory := parent; directory != ""; {
		held, err := exists("SELECT 1 FROM "+files+" WHERE repo = ? AND parent = ? LIMIT 1", repo, directory)
		if err == nil && !held {
			held, err = exists("SELECT 1 FROM "+dirs+" WHERE repo = ? AND parent = ? LIMIT 1", repo, directory)
		}
		if err != nil {
			return nil, err
		}
		if held {
			break
		}
		up, leaf := split(directory)
		if _, err := tx.ExecContext(bg, "DELETE FROM "+dirs+" WHERE repo = ? AND parent = ? AND name = ?", repo, up, leaf); err != nil { // #nosec G202 -- the table name is dirsTable of an integer store id; every value is a placeholder.
			return nil, err
		}
		directory = up
	}
	var count, newest int64
	err = tx.QueryRowContext(bg, "SELECT files, newest_ms FROM repo_counts WHERE store_id = ? AND repo = ?", id, repo).Scan(&count, &newest)
	if errors.Is(err, sql.ErrNoRows) {
		return removed, nil
	}
	if err != nil {
		return nil, err
	}
	if count <= 1 {
		_, err := tx.ExecContext(bg, "DELETE FROM repo_counts WHERE store_id = ? AND repo = ?", id, repo)
		return removed, err
	}
	if removed.CreatedMS >= newest {
		if err := tx.QueryRowContext(bg, "SELECT max(created_ms) FROM "+files+" WHERE repo = ?", repo).Scan(&newest); err != nil {
			return nil, err
		}
	}
	_, err = tx.ExecContext(bg, "UPDATE repo_counts SET files = files - 1, bytes = bytes - ?, newest_ms = ? WHERE store_id = ? AND repo = ?",
		removed.Size, newest, id, repo)
	return removed, err
}

// Put writes the row of one entry into the tables of a store, with the
// directories of its path and the counts of its repository, in one
// transaction, as a scan of the store writes them. The row of the same path
// in that store is replaced whatever its creation time: the writer marks
// the blob it replaced as deleted (W4), so the store holds one live blob
// per path and a scan agrees. Put returns the replaced row, nil for a new
// path. A path that is empty or starts or ends with "/" is refused.
func (ix *Index) Put(store string, e stores.Entry) (*Asset, error) {
	if !addressable(e.Path) {
		return nil, fmt.Errorf("index: path %q is empty or starts or ends with a slash", e.Path)
	}
	return ix.put(store, func() (stores.Entry, error) { return e, nil }, ix.write)
}

// PutAfter is Put of the entry of a blob that place puts into the store.
// place runs once the write lock of the index is held, within wait
// (writeWithin), so that a scan of the store, which holds that lock for its
// whole walk, never meets the blob before its row can follow. After wait it
// returns ErrBusy and place did not run. When place fails, or the row
// cannot be written after it, the rows stay as they were.
func (ix *Index) PutAfter(store string, wait time.Duration, place func() (stores.Entry, error)) (*Asset, error) {
	return ix.put(store, place, func(fn func(tx *sql.Tx) error) error { return ix.writeWithin(wait, fn) })
}

func (ix *Index) put(store string, entry func() (stores.Entry, error), write func(fn func(tx *sql.Tx) error) error) (*Asset, error) {
	var replaced *Asset
	err := write(func(tx *sql.Tx) error {
		e, err := entry()
		if err != nil {
			return err
		}
		if !addressable(e.Path) {
			return fmt.Errorf("index: path %q is empty or starts or ends with a slash", e.Path)
		}
		id, err := storeIDOrNew(tx, store)
		if err != nil {
			return err
		}
		files, dirs, err := storeTables(tx, id)
		if err != nil {
			return err
		}
		old, err := rowOf(tx, files, store, e.Repo, e.Path)
		if err != nil {
			return err
		}
		parent, name := split(e.Path)
		if _, err := tx.ExecContext(bg, "INSERT INTO "+files+" (repo, parent, name, location, size, sha1, content_type, created_ms)"+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (repo, parent, name) DO UPDATE SET"+
			" location = excluded.location, size = excluded.size, sha1 = excluded.sha1,"+
			" content_type = excluded.content_type, created_ms = excluded.created_ms",
			e.Repo, parent, name, e.Location, e.Size, nullable(e.SHA1), nullable(e.ContentType), e.CreatedMS); err != nil {
			return err
		}
		for directory := parent; directory != ""; {
			up, leaf := split(directory)
			if _, err := tx.ExecContext(bg, "INSERT OR IGNORE INTO "+dirs+" VALUES (?, ?, ?)", e.Repo, up, leaf); err != nil { // #nosec G202 -- the table name is dirsTable of an integer store id; every value is a placeholder.
				return err
			}
			directory = up
		}
		if err := putCounts(tx, id, files, e, old); err != nil {
			return err
		}
		replaced = old
		return nil
	})
	if err != nil {
		return nil, err
	}
	return replaced, nil
}

// putCounts keeps the counts of the repository of an entry that Put wrote;
// old is the row it replaced.
func putCounts(tx *sql.Tx, id int64, files string, e stores.Entry, old *Asset) error {
	var newest int64
	err := tx.QueryRowContext(bg, "SELECT newest_ms FROM repo_counts WHERE store_id = ? AND repo = ?", id, e.Repo).Scan(&newest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err := tx.ExecContext(bg, "INSERT INTO repo_counts"+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
			" SELECT ?, repo, count(*), sum(size), max(created_ms) FROM "+files+" WHERE repo = ? GROUP BY repo", id, e.Repo)
		return err
	case err != nil:
		return err
	case old == nil:
		_, err := tx.ExecContext(bg, "UPDATE repo_counts SET files = files + 1, bytes = bytes + ?, newest_ms = max(newest_ms, ?)"+
			" WHERE store_id = ? AND repo = ?", e.Size, e.CreatedMS, id, e.Repo)
		return err
	}
	switch {
	case e.CreatedMS >= newest:
		newest = e.CreatedMS
	case old.CreatedMS >= newest:
		if err := tx.QueryRowContext(bg, "SELECT max(created_ms) FROM "+files+" WHERE repo = ?", e.Repo).Scan(&newest); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(bg, "UPDATE repo_counts SET bytes = bytes - ? + ?, newest_ms = ? WHERE store_id = ? AND repo = ?",
		old.Size, e.Size, newest, id, e.Repo)
	return err
}

// Delete removes the row of a path from the tables of a store, with the
// directories it leaves empty and the counts of its repository, and
// returns the removed row, nil when the store had none. Unlike Drop it
// waits for the write lock and the busy timeout of every write.
func (ix *Index) Delete(repo, path, store string) (*Asset, error) {
	if !addressable(path) {
		return nil, nil
	}
	var removed *Asset
	err := ix.write(func(tx *sql.Tx) error {
		var err error
		removed, err = dropRow(tx, repo, path, store)
		return err
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// DeleteWithin is Delete with a bound on the wait for the write lock of the
// index (writeWithin), and with before, which runs once the lock is held and
// before the row goes: the delete mark of the blob, so that a busy index
// leaves the blob as it was. After wait it returns ErrBusy and before did not
// run; an error of before keeps the row.
func (ix *Index) DeleteWithin(repo, path, store string, wait time.Duration, before func() error) (*Asset, error) {
	if !addressable(path) {
		return nil, nil
	}
	var removed *Asset
	err := ix.writeWithin(wait, func(tx *sql.Tx) error {
		if err := before(); err != nil {
			return err
		}
		var err error
		removed, err = dropRow(tx, repo, path, store)
		return err
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// Reads

// RepoRows are every row of a repository in every store, sorted by store
// and path (decision A7 of the phase 6 plan: the delete of a repository).
func (ix *Index) RepoRows(repo string) ([]Asset, error) {
	var out []Asset
	err := ix.read(func(tx *sql.Tx) error {
		list, err := storeList(tx)
		if err != nil {
			return err
		}
		for _, s := range list {
			rows, err := tx.QueryContext(bg, "SELECT parent, name, location, size, sha1, content_type, created_ms FROM "+filesTable(s.id)+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
				" WHERE repo = ? ORDER BY parent, name", repo)
			if err != nil {
				return err
			}
			for rows.Next() {
				var parent, name string
				var sha1, contentType sql.NullString
				a := Asset{Repo: repo, Store: s.name}
				if err := rows.Scan(&parent, &name, &a.Location, &a.Size, &sha1, &contentType, &a.CreatedMS); err != nil {
					rows.Close()
					return err
				}
				a.Path = name
				if parent != "" {
					a.Path = parent + "/" + name
				}
				a.SHA1, a.ContentType = optional(sha1), optional(contentType)
				out = append(out, a)
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// Lookup is the winning row of a path over all stores, nil when no store
// has it or the path is empty, starts or ends with "/".
func (ix *Index) Lookup(repo, path string) (*Asset, error) {
	if !addressable(path) {
		return nil, nil
	}
	parent, name := split(path)
	var best *Asset
	err := ix.read(func(tx *sql.Tx) error {
		list, err := storeList(tx)
		if err != nil {
			return err
		}
		for _, s := range list {
			a := Asset{Repo: repo, Path: path, Store: s.name}
			var sha1, contentType sql.NullString
			err := tx.QueryRowContext(bg, "SELECT location, size, sha1, content_type, created_ms FROM "+filesTable(s.id)+
				" WHERE repo = ? AND parent = ? AND name = ?", repo, parent, name).Scan(&a.Location, &a.Size, &sha1, &contentType, &a.CreatedMS)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			a.SHA1, a.ContentType = optional(sha1), optional(contentType)
			if best == nil || a.CreatedMS > best.CreatedMS {
				best = &a
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return best, nil
}

// stream is one sorted cursor of Children: the directories or the files of
// one store, ordered by name.
type stream struct {
	rows          *sql.Rows
	kind          int // 0 directory, 1 file: a directory sorts before a file of its name
	rank          int // position of the store in name order
	name          string
	size, created int64
}

func (s *stream) next() (bool, error) {
	if !s.rows.Next() {
		return false, s.rows.Err()
	}
	if s.kind == 0 {
		return true, s.rows.Scan(&s.name)
	}
	return true, s.rows.Scan(&s.name, &s.size, &s.created)
}

// merge orders heads as heapq.merge orders the Python tuples
// (name, kind, -created_ms, rank).
type merge []*stream

func (m merge) Len() int { return len(m) }
func (m merge) Less(i, j int) bool {
	a, b := m[i], m[j]
	if a.name != b.name {
		return a.name < b.name
	}
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	if a.kind == 1 && a.created != b.created {
		return a.created > b.created
	}
	return a.rank < b.rank
}
func (m merge) Swap(i, j int) { m[i], m[j] = m[j], m[i] }
func (m *merge) Push(x any)   { *m = append(*m, x.(*stream)) }
func (m *merge) Pop() any {
	old := *m
	last := old[len(old)-1]
	*m = old[:len(old)-1]
	return last
}

// Children lists the names in a directory over all stores, directories and
// files merged by name. Of one name and kind the winning row is shown; a
// name that is a directory in one store and a file in another shows twice.
// The directory is taken without leading and trailing slashes; a limit of
// zero or less gives nothing, a negative offset counts as zero.
func (ix *Index) Children(repo, directory string, limit, offset int) ([]Child, error) {
	directory = strings.Trim(directory, "/")
	result := []Child{}
	if limit <= 0 {
		return result, nil
	}
	offset = max(offset, 0)
	err := ix.read(func(tx *sql.Tx) error {
		list, err := storeList(tx)
		if err != nil {
			return err
		}
		var opened []*stream
		defer func() {
			for _, s := range opened {
				_ = s.rows.Close() // cleanup of the merge streams
			}
		}()
		for rank, s := range list {
			for kind, query := range []string{
				"SELECT name FROM " + dirsTable(s.id) + " WHERE repo = ? AND parent = ? ORDER BY name",
				"SELECT name, size, created_ms FROM " + filesTable(s.id) + " WHERE repo = ? AND parent = ? ORDER BY name",
			} {
				rows, err := tx.QueryContext(bg, query, repo, directory)
				if err != nil {
					return err
				}
				opened = append(opened, &stream{rows: rows, kind: kind, rank: rank})
			}
		}
		heads := merge{}
		for _, s := range opened {
			ok, err := s.next()
			if err != nil {
				return err
			}
			if ok {
				heads = append(heads, s)
			}
		}
		heap.Init(&heads)
		previous, previousKind := "", -1
		skipped := 0
		for heads.Len() > 0 {
			s := heads[0]
			name, kind, size, created := s.name, s.kind, s.size, s.created
			ok, err := s.next()
			if err != nil {
				return err
			}
			if ok {
				heap.Fix(&heads, 0)
			} else {
				heap.Pop(&heads)
			}
			if name == previous && kind == previousKind {
				continue // the first row of a name is the winner
			}
			previous, previousKind = name, kind
			if skipped < offset {
				skipped++
				continue
			}
			child := Child{Name: name, IsDir: kind == 0}
			if kind == 1 {
				child.Size, child.CreatedMS = &size, &created
			}
			result = append(result, child)
			if len(result) >= limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// IsDir reports whether any store has the directory; the root of a
// repository is a directory when the repository has a file.
func (ix *Index) IsDir(repo, directory string) (bool, error) {
	directory = strings.Trim(directory, "/")
	parent, name := split(directory)
	found := false
	err := ix.read(func(tx *sql.Tx) error {
		list, err := storeList(tx)
		if err != nil {
			return err
		}
		for _, s := range list {
			var row *sql.Row
			if directory != "" {
				row = tx.QueryRowContext(bg, "SELECT 1 FROM "+dirsTable(s.id)+" WHERE repo = ? AND parent = ? AND name = ? LIMIT 1", repo, parent, name) // #nosec G202 -- the table name is dirsTable of an integer store id; every value is a placeholder.
			} else {
				row = tx.QueryRowContext(bg, "SELECT 1 FROM "+filesTable(s.id)+" WHERE repo = ? LIMIT 1", repo) // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
			}
			var one int
			err := row.Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			found = true
			return nil
		}
		return nil
	})
	return found, err
}

// RepoStats counts every repository, sorted by name. The answer is cached
// until the generation changes.
func (ix *Index) RepoStats() ([]RepoStat, error) {
	var stats []RepoStat
	var generation int64
	cached := false
	err := ix.read(func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(bg, "SELECT value FROM meta WHERE key = 'generation'").Scan(&generation); err != nil {
			return err
		}
		ix.cacheMu.Lock()
		if ix.cacheStats != nil && ix.cacheGen == generation {
			stats, cached = slices.Clone(ix.cacheStats), true
		}
		ix.cacheMu.Unlock()
		if cached {
			return nil
		}
		type part struct {
			id                   int64
			files, bytes, newest int64
		}
		var repos []string
		parts := map[string][]part{}
		rows, err := tx.QueryContext(bg, "SELECT c.repo, s.id, c.files, c.bytes, c.newest_ms FROM repo_counts c"+
			" JOIN stores s ON s.id = c.store_id ORDER BY c.repo, s.name")
		if err != nil {
			return err
		}
		for rows.Next() {
			var repo string
			var p part
			if err := rows.Scan(&repo, &p.id, &p.files, &p.bytes, &p.newest); err != nil {
				_ = rows.Close() // the scan error is the one returned
				return err
			}
			if _, ok := parts[repo]; !ok {
				repos = append(repos, repo)
			}
			parts[repo] = append(parts[repo], p)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		stats = []RepoStat{}
		for _, repo := range repos {
			if list := parts[repo]; len(list) == 1 {
				stats = append(stats, RepoStat{repo, list[0].files, list[0].bytes, list[0].newest})
				continue
			}
			ids := make([]int64, 0, len(parts[repo]))
			for _, p := range parts[repo] {
				ids = append(ids, p.id)
			}
			merged, err := mergedStat(tx, repo, ids)
			if err != nil {
				return err
			}
			stats = append(stats, merged)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !cached {
		ix.cacheMu.Lock()
		ix.cacheGen, ix.cacheStats = generation, slices.Clone(stats)
		ix.cacheMu.Unlock()
	}
	return stats, nil
}

// mergedStat counts a repository that lives in several stores; a path counts
// once, with its winning row.
func mergedStat(tx *sql.Tx, repo string, ids []int64) (RepoStat, error) {
	union := make([]string, len(ids))
	args := make([]any, len(ids))
	for rank, id := range ids {
		union[rank] = fmt.Sprintf("SELECT parent, name, size, created_ms, %d AS store_rank FROM %s WHERE repo = ?", rank, filesTable(id))
		args[rank] = repo
	}
	stat := RepoStat{Repo: repo}
	err := tx.QueryRowContext(bg, "SELECT count(*), coalesce(sum(size), 0), coalesce(max(created_ms), 0) FROM ("+
		" SELECT size, created_ms,"+
		" row_number() OVER (PARTITION BY parent, name ORDER BY created_ms DESC, store_rank) AS position"+
		" FROM ("+strings.Join(union, " UNION ALL ")+")) WHERE position = 1", args...).Scan(&stat.Files, &stat.Bytes, &stat.NewestMS)
	return stat, err
}

// Generation is a number that changes with every write to the index, also
// with a write by another process.
func (ix *Index) Generation() (int64, error) {
	var generation int64
	err := ix.read(func(tx *sql.Tx) error {
		return tx.QueryRowContext(bg, "SELECT value FROM meta WHERE key = 'generation'").Scan(&generation)
	})
	return generation, err
}

// RepoNames are the repositories with at least one row, sorted.
func (ix *Index) RepoNames() ([]string, error) {
	names := []string{}
	err := ix.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT DISTINCT repo FROM repo_counts ORDER BY repo")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// Hints maps a repository to its guessed format. Where stores disagree, the
// store name that sorts first wins.
func (ix *Index) Hints() (map[string]string, error) {
	hints := map[string]string{}
	err := ix.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT h.repo, h.format FROM hints h JOIN stores s ON s.id = h.store_id ORDER BY h.repo, s.name")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var repo, format string
			if err := rows.Scan(&repo, &format); err != nil {
				return err
			}
			if _, ok := hints[repo]; !ok {
				hints[repo] = format
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return hints, nil
}

// StoreStats counts rows and bytes per store, sorted by store name. A path
// in two stores counts in both.
func (ix *Index) StoreStats() ([]StoreStat, error) {
	stats := []StoreStat{}
	err := ix.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT s.name, coalesce(sum(c.files), 0), coalesce(sum(c.bytes), 0) FROM stores s"+
			" LEFT JOIN repo_counts c ON c.store_id = s.id GROUP BY s.id ORDER BY s.name")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s StoreStat
			if err := rows.Scan(&s.Store, &s.Files, &s.Bytes); err != nil {
				return err
			}
			stats = append(stats, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return stats, nil
}

// Close closes the pool, then waits for a running write and closes the
// writer. Later calls return ErrClosed; a second Close returns nil.
func (ix *Index) Close() error {
	if ix.closed.Swap(true) {
		return nil
	}
	err := ix.readers.Close()
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	if ix.writer != nil {
		err = errors.Join(err, ix.writer.Close())
	}
	return errors.Join(err, ix.writerDB.Close())
}

// Rows are the rows of a path in every store that holds it, the winning
// row first, as Lookup picks it: the newest, on a tie the store whose name
// sorts first. Empty when no store has the path or the path is empty,
// starts or ends with "/".
func (ix *Index) Rows(repo, path string) ([]Asset, error) {
	if !addressable(path) {
		return nil, nil
	}
	var rows []Asset
	err := ix.read(func(tx *sql.Tx) error {
		list, err := storeList(tx)
		if err != nil {
			return err
		}
		for _, s := range list {
			a, err := rowOf(tx, filesTable(s.id), s.name, repo, path)
			if err != nil {
				return err
			}
			if a != nil {
				rows = append(rows, *a)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// the stores come in name order; a stable sort keeps it on a tie
	slices.SortStableFunc(rows, func(a, b Asset) int { return cmp.Compare(b.CreatedMS, a.CreatedMS) })
	return rows, nil
}
