package manifest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pystr"
	_ "modernc.org/sqlite" // the "sqlite" driver
)

// The statements of the Python module, unchanged.
const (
	createRows = "CREATE TABLE rows (repo TEXT NOT NULL, path TEXT NOT NULL, size INTEGER NOT NULL, sha1 TEXT," +
		" content_type TEXT, created_ms INTEGER NOT NULL, PRIMARY KEY (repo, path)) WITHOUT ROWID"
	createMeta = "CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID"
	insertRow  = "INSERT INTO rows VALUES (?, ?, ?, ?, ?, ?)" +
		" ON CONFLICT (repo, path) DO UPDATE SET size = excluded.size, sha1 = excluded.sha1," +
		" content_type = excluded.content_type, created_ms = excluded.created_ms" +
		" WHERE excluded.created_ms > rows.created_ms"
	insertMeta = "INSERT INTO meta VALUES ('source', ?), ('version', ?)"
	selectRow  = "SELECT size, sha1, content_type, created_ms FROM rows WHERE repo = ? AND path = ?"
)

var bg = context.Background()

// Lookup answers the newest live row of a manifest per repository and path.
//
// The rows live in <cache>/<source id>-<state>.sqlite. The source id is a
// hash of the resolved source path, the state a hash of the names, sizes and
// modification times of its parts. A lookup over a known state opens the
// table read only; a new state builds a new table and removes the tables of
// the older states of the same source.
type Lookup struct {
	source, path string
	db           *sql.DB
	get          *sql.Stmt
}

// Source is the resolved manifest path.
func (l *Lookup) Source() string { return l.source }

// Path is the table file.
func (l *Lookup) Path() string { return l.path }

// Open opens the lookup of the manifest at source with its table below
// cacheDir, and builds the table when the state of the manifest has none.
func Open(source, cacheDir string) (*Lookup, error) {
	given := pystr.Path(source)
	resolved, err := realpath(given)
	if err != nil {
		return nil, newError("FileNotFoundError", "manifest %s does not exist", given)
	}
	cache := pystr.Path(cacheDir)
	found, err := parts(resolved)
	if err != nil {
		return nil, err
	}
	key := "[" + strconv.Itoa(LookupVersion)
	for _, p := range found {
		info, err := os.Stat(p.path)
		if err != nil {
			return nil, err
		}
		key += ", (" + pystr.Repr(fsDecode(p.name)) + ", " + strconv.FormatInt(info.Size(), 10) + ", " +
			strconv.FormatInt(info.ModTime().UnixNano(), 10) + ")"
	}
	key += "]"
	sourceID := digest(resolved)[:16]
	path := pystr.Join(cache, sourceID+"-"+digest(key)[:24]+".sqlite")
	ok, err := isFile(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := build(resolved, cache, sourceID, path); err != nil {
			return nil, err
		}
	}
	// Path.as_uri() of Python, after the build.
	if !strings.HasPrefix(path, "/") {
		return nil, newError("ValueError", "relative path can't be expressed as a file URI")
	}
	l, err := openTable(path)
	if err != nil {
		return nil, err
	}
	l.source = resolved
	return l, nil
}

// openTable opens a table file read only. Like sqlite3.connect, it opens
// the file now, so a later removal by another lookup does not end it.
func openTable(path string) (*Lookup, error) {
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	get, err := db.Prepare(selectRow)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Lookup{path: path, db: db, get: get}, nil
}

// fileURI is a file: URI of an absolute path, so that any character of the
// path reaches SQLite.
func fileURI(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath()
}

// Get returns nil, nil when the manifest has no live row for the path.
func (l *Lookup) Get(repo, path string) (*Row, error) {
	row := &Row{Repo: repo, Path: path}
	var sha1, contentType sql.NullString
	err := l.get.QueryRow(repo, path).Scan(&row.Size, &sha1, &contentType, &row.CreatedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sha1.Valid {
		row.SHA1 = &sha1.String
	}
	if contentType.Valid {
		row.ContentType = &contentType.String
	}
	return row, nil
}

// Close closes the table.
func (l *Lookup) Close() error {
	l.get.Close()
	return l.db.Close()
}

// build writes the table of the manifest into a temporary file of the cache
// directory, moves it to path and removes the tables of the older states.
func build(source, cache, sourceID, path string) error {
	if err := os.MkdirAll(cache, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(cache, "."+sourceID+"-*.building")
	if err != nil {
		return err
	}
	temp := f.Name()
	f.Close()
	if err := fill(temp, source); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	names, err := listDir(cache)
	if err != nil {
		return err
	}
	slices.Sort(names)
	keep := baseName(path)
	for _, name := range names {
		if name == keep || !strings.HasPrefix(name, sourceID+"-") || !strings.HasSuffix(name, ".sqlite") ||
			len(name) < len(sourceID)+1+len(".sqlite") {
			continue
		}
		// Path.unlink: no rmdir for a directory of that name.
		if err := syscall.Unlink(pystr.Join(cache, name)); err != nil && !errors.Is(err, syscall.ENOENT) {
			return &os.PathError{Op: "unlink", Path: pystr.Join(cache, name), Err: err}
		}
	}
	return nil
}

func fill(temp, source string) error {
	db, err := sql.Open("sqlite", fileURI(temp))
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(bg)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, statement := range []string{"PRAGMA journal_mode = OFF", "PRAGMA synchronous = OFF", createRows, createMeta, "BEGIN"} {
		if _, err := conn.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	insert, err := conn.PrepareContext(bg, insertRow)
	if err != nil {
		return err
	}
	err = ReadRows(source, func(r Row) error {
		if r.Overflow {
			return newError("OverflowError", "Python int too large to convert to SQLite INTEGER")
		}
		_, err := insert.ExecContext(bg, r.Repo, r.Path, r.Size, nullable(r.SHA1), nullable(r.ContentType), r.CreatedMS)
		return err
	})
	insert.Close()
	if err != nil {
		return err
	}
	// sqlite3 of Python encodes the str strictly; a path that is not UTF-8
	// holds surrogates there.
	if !utf8.ValidString(source) {
		return encodeError(source)
	}
	if _, err := conn.ExecContext(bg, insertMeta, source, strconv.Itoa(LookupVersion)); err != nil {
		return err
	}
	_, err = conn.ExecContext(bg, "COMMIT")
	return err
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// digest is the SHA-256 in hex of the UTF-8 bytes of a Python str; a file
// name is its own bytes, as surrogateescape gives them back.
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// encodeError is the UnicodeEncodeError of str.encode("utf-8") for a path
// with bytes that are not UTF-8.
func encodeError(path string) *Error {
	runes := pystr.Runes(fsDecode(path))
	start := slices.IndexFunc(runes, func(r rune) bool { return r >= 0xD800 && r <= 0xDFFF })
	end := start
	for end < len(runes) && runes[end] >= 0xD800 && runes[end] <= 0xDFFF {
		end++
	}
	if end-start == 1 {
		return newError("UnicodeEncodeError",
			`'utf-8' codec can't encode character '\u%04x' in position %d: surrogates not allowed`, runes[start], start)
	}
	return newError("UnicodeEncodeError",
		"'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", start, end-1)
}
