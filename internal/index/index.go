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
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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
// open is a write: it bumps the generation, as in Python.
func Open(path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
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
	if err := ix.setUp(); err != nil {
		_ = ix.Close() // cleanup; the set up error is the one returned
		return nil, err
	}
	return ix, nil
}

func (ix *Index) setUp() error {
	var err error
	if ix.writer, err = ix.writerDB.Conn(bg); err != nil {
		return err
	}
	for _, pragma := range []string{"PRAGMA journal_mode = WAL", fmt.Sprintf("PRAGMA journal_size_limit = %d", walSizeLimit)} {
		if _, err := ix.writer.ExecContext(bg, pragma); err != nil {
			return err
		}
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
		// Python compares with ==, so 1.0 matches as well.
		if v, ok := version.(int64); ok && v == SchemaVersion {
			return nil
		}
		if v, ok := version.(float64); ok && v == SchemaVersion {
			return nil
		}
		return fmt.Errorf("%s has index schema %v, expected %d; delete it and scan again", pathText(ix.path), version, SchemaVersion)
	})
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

// ReplaceStore replaces the rows of one store with the entries, in one
// transaction, and returns the rows written. Entries whose path is empty,
// starts or ends with "/" are skipped; of two entries of one path the newer
// wins. When entries returns an error, the old rows stay and ReplaceStore
// returns that error.
func (ix *Index) ReplaceStore(store string, entries func(yield func(stores.Entry) error) error) (int64, error) {
	var written int64
	err := ix.write(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(bg, fmt.Sprintf("PRAGMA cache_size = -%d", writeCacheKiB)); err != nil {
			return err
		}
		id, err := storeIDOrNew(tx, store)
		if err != nil {
			return err
		}
		files, dirs := filesTable(id), dirsTable(id)
		for _, statement := range []string{
			"CREATE TABLE IF NOT EXISTS " + files + " (" +
				" repo TEXT NOT NULL, parent TEXT NOT NULL, name TEXT NOT NULL, location TEXT NOT NULL," +
				" size INTEGER NOT NULL, sha1 TEXT, content_type TEXT, created_ms INTEGER NOT NULL," +
				" PRIMARY KEY (repo, parent, name)) WITHOUT ROWID",
			"CREATE TABLE IF NOT EXISTS " + dirs + " (" +
				" repo TEXT NOT NULL, parent TEXT NOT NULL, name TEXT NOT NULL," +
				" PRIMARY KEY (repo, parent, name)) WITHOUT ROWID",
			"DELETE FROM " + files,
			"DELETE FROM " + dirs,
		} {
			if _, err := tx.ExecContext(bg, statement); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(bg, "DELETE FROM repo_counts WHERE store_id = ?", id); err != nil {
			return err
		}
		insert, err := tx.PrepareContext(bg,
			"INSERT INTO "+files+" (repo, parent, name, location, size, sha1, content_type, created_ms)"+ // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?)"+
				" ON CONFLICT (repo, parent, name) DO UPDATE SET"+
				" location = excluded.location, size = excluded.size, sha1 = excluded.sha1,"+
				" content_type = excluded.content_type, created_ms = excluded.created_ms"+
				" WHERE excluded.created_ms > "+files+".created_ms")
		if err != nil {
			return err
		}
		defer insert.Close()
		var failed error
		err = entries(func(e stores.Entry) error {
			if failed != nil {
				return failed
			}
			if !addressable(e.Path) {
				return nil
			}
			parent, name := split(e.Path)
			_, failed = insert.ExecContext(bg, e.Repo, parent, name, e.Location, e.Size, nullable(e.SHA1), nullable(e.ContentType), e.CreatedMS)
			return failed
		})
		if err != nil {
			return err
		}
		if failed != nil {
			return failed
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
func (ix *Index) Drop(repo, path, store string) (err error) {
	if !addressable(path) || !ix.writeMu.TryLock() {
		return nil
	}
	defer ix.writeMu.Unlock()
	if ix.closed.Load() {
		return ErrClosed
	}
	if _, err := ix.writer.ExecContext(bg, fmt.Sprintf("PRAGMA busy_timeout = %d", dropBusyTimeoutMS)); err != nil {
		return err
	}
	defer func() {
		if _, restore := ix.writer.ExecContext(bg, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMS)); restore != nil && err == nil {
			err = restore
		}
	}()
	err = ix.transaction(func(tx *sql.Tx) error { return dropRow(tx, repo, path, store) })
	if busy(err) {
		return nil
	}
	return err
}

func dropRow(tx *sql.Tx, repo, path, store string) error {
	id, found, err := storeID(tx, store)
	if err != nil || !found {
		return err
	}
	files, dirs := filesTable(id), dirsTable(id)
	parent, name := split(path)
	var size, created int64
	err = tx.QueryRowContext(bg, "SELECT size, created_ms FROM "+files+" WHERE repo = ? AND parent = ? AND name = ?",
		repo, parent, name).Scan(&size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(bg, "DELETE FROM "+files+" WHERE repo = ? AND parent = ? AND name = ?", repo, parent, name); err != nil { // #nosec G202 -- the table name is filesTable of an integer store id; every value is a placeholder.
		return err
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
			return err
		}
		if held {
			break
		}
		up, leaf := split(directory)
		if _, err := tx.ExecContext(bg, "DELETE FROM "+dirs+" WHERE repo = ? AND parent = ? AND name = ?", repo, up, leaf); err != nil { // #nosec G202 -- the table name is dirsTable of an integer store id; every value is a placeholder.
			return err
		}
		directory = up
	}
	var count, newest int64
	err = tx.QueryRowContext(bg, "SELECT files, newest_ms FROM repo_counts WHERE store_id = ? AND repo = ?", id, repo).Scan(&count, &newest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if count <= 1 {
		_, err := tx.ExecContext(bg, "DELETE FROM repo_counts WHERE store_id = ? AND repo = ?", id, repo)
		return err
	}
	if created >= newest {
		if err := tx.QueryRowContext(bg, "SELECT max(created_ms) FROM "+files+" WHERE repo = ?", repo).Scan(&newest); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(bg, "UPDATE repo_counts SET files = files - 1, bytes = bytes - ?, newest_ms = ? WHERE store_id = ? AND repo = ?",
		size, newest, id, repo)
	return err
}

// Reads

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
