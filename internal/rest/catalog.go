package rest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	_ "modernc.org/sqlite" // the driver of the index
)

// Catalog reads the file rows of index.sqlite for the search, read only,
// on connections of its own. The tables are those of index schema 1, which
// the Python server shares: stores (id, name) and per store files_<id>
// keyed by (repo, parent, name). The index package has no call that walks
// a repository, and a search walks every path of one.
type Catalog struct {
	db *sql.DB
}

// OpenCatalog opens the index file at path read only. The file must exist:
// index.Open creates it before.
func OpenCatalog(path string) (*Catalog, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	params := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(30000)", "query_only(1)"}}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?"+params.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		_ = db.Close() // the ping error is the one returned
		return nil, err
	}
	return &Catalog{db: db}, nil
}

// Close closes the connections.
func (c *Catalog) Close() error { return c.db.Close() }

// prefilter narrows the rows in SQL before the search looks at them: each
// group is a list of LIKE patterns of which one must match the path, and
// sha1, when set, is the stored SHA-1. It may let rows through that the
// search then refuses, never the other way round. The patterns depend on
// the path only, so all rows of a path pass or none; the SHA-1 differs per
// row, so a row found by it is checked against the winner of its path.
type prefilter struct {
	groups [][]string
	sha1   string
}

// cursor is the rows of one store of one repository in key order.
type cursor struct {
	rows  *sql.Rows
	store string
	row   index.Asset
	key   [2]string // parent, name
	done  bool
}

func (c *cursor) next() error {
	if !c.rows.Next() {
		c.done = true
		return c.rows.Err()
	}
	var sha1, contentType sql.NullString
	var parent, name string
	if err := c.rows.Scan(&parent, &name, &c.row.Location, &c.row.Size, &sha1, &contentType, &c.row.CreatedMS); err != nil {
		return err
	}
	c.key = [2]string{parent, name}
	c.row.Path = name
	if parent != "" {
		c.row.Path = parent + "/" + name
	}
	c.row.SHA1, c.row.ContentType = nil, nil
	if sha1.Valid {
		s := sha1.String
		c.row.SHA1 = &s
	}
	if contentType.Valid {
		s := contentType.String
		c.row.ContentType = &s
	}
	return nil
}

func less(a, b [2]string) bool { return a[0] < b[0] || a[0] == b[0] && a[1] < b[1] }

// walk calls fn with each path of repo once, with its winning row: the
// newest, on a tie the store whose name sorts first, as index.Lookup
// chooses. The rows of all stores are read in one read transaction. With a
// SHA-1 in the prefilter a row is passed on only when lookup names it the
// winner of its path.
func (c *Catalog) walk(ctx context.Context, repo string, pre prefilter, lookup func(repo, path string) (*index.Asset, error),
	fn func(index.Asset) error) (err error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() {
		if rerr := tx.Rollback(); err == nil && rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			err = rerr
		}
	}()
	storeRows, err := tx.QueryContext(ctx, "SELECT id, name FROM stores ORDER BY name")
	if err != nil {
		return err
	}
	type store struct {
		id   int64
		name string
	}
	var list []store
	for storeRows.Next() {
		var s store
		if err := storeRows.Scan(&s.id, &s.name); err != nil {
			_ = storeRows.Close() // the scan error is the one returned
			return err
		}
		list = append(list, s)
	}
	if err := storeRows.Close(); err != nil {
		return err
	}
	where, args := "repo = ?", []any{repo}
	for _, group := range pre.groups {
		var or []string
		for _, pattern := range group {
			or = append(or, `(parent || '/' || name) LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		where += " AND (" + strings.Join(or, " OR ") + ")"
	}
	if pre.sha1 != "" {
		where += " AND lower(sha1) = ?"
		args = append(args, strings.ToLower(pre.sha1))
	}
	var cursors []*cursor
	defer func() {
		for _, cur := range cursors {
			_ = cur.rows.Close() // cleanup; an error of the walk is the one returned
		}
	}()
	for _, s := range list {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf( // #nosec G201 -- the table name is files_ of an integer store id; every value is a placeholder.
			"SELECT parent, name, location, size, sha1, content_type, created_ms FROM files_%d WHERE %s ORDER BY parent, name", s.id, where),
			args...)
		if err != nil {
			return err
		}
		cur := &cursor{rows: rows, store: s.name}
		cur.row.Repo, cur.row.Store = repo, s.name
		cursors = append(cursors, cur)
		if err := cur.next(); err != nil {
			return err
		}
	}
	for {
		var best *cursor
		for _, cur := range cursors {
			if !cur.done && (best == nil || less(cur.key, best.key)) {
				best = cur
			}
		}
		if best == nil {
			return ctx.Err()
		}
		key, winner := best.key, best.row
		for _, cur := range cursors {
			if cur.done || cur.key != key {
				continue
			}
			if cur.row.CreatedMS > winner.CreatedMS {
				winner = cur.row
			}
			if err := cur.next(); err != nil {
				return err
			}
		}
		if pre.sha1 != "" {
			found, err := lookup(repo, winner.Path)
			if err != nil {
				return err
			}
			if found == nil || found.Store != winner.Store || found.Location != winner.Location {
				continue
			}
		}
		if err := fn(winner); err != nil {
			return err
		}
	}
}
