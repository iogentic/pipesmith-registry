package catalog

import (
	"database/sql"
	"errors"
)

// The table proxy_state of version 3 (section State of the phase 5 plan):
// per proxy repository and path the time of the last check at the remote,
// the entity tag and the last modified value the remote sent, and the end of
// a not found mark. Times are milliseconds since the epoch. The row with the
// empty path of a repository holds the time of its last bulk reset in
// checked_ms: every path checked before it is stale, also a path without a
// row, which counts as last verified at the creation time of its blob
// (decision 5.5). The file is side data: after its loss every path counts
// by its creation time and no cached byte is lost. The server does not open
// the catalog at run time yet; leg 5b wires these accessors.

// ProxyRow is the state of one path of a proxy repository.
type ProxyRow struct {
	Repo, Path string
	// CheckedMS is the time of the last check at the remote; nil means stale:
	// the next request asks the remote whatever the ages say.
	CheckedMS *int64
	// ETag and LastModified are the validators the remote sent, empty when it
	// sent none.
	ETag, LastModified string
	// NotFoundUntilMS is the end of a not found mark, nil when there is none.
	NotFoundUntilMS *int64
}

// ProxyState is what a request for one path needs: its row, nil when there
// is none, and the time of the last bulk reset of its repository, nil when
// there was none.
type ProxyState struct {
	Row     *ProxyRow
	ResetMS *int64
}

// resetPath is the path of the row that holds the time of a bulk reset.
const resetPath = ""

var errNoProxyKey = errors.New("catalog: a proxy_state row needs a repository and a path that is not empty")

func nullInt(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func intOf(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// ProxyState reads the row of one path and the reset time of its repository
// in one read transaction.
func (c *Catalog) ProxyState(repo, path string) (ProxyState, error) {
	if repo == "" || path == resetPath {
		return ProxyState{}, errNoProxyKey
	}
	var s ProxyState
	err := c.read(func(tx *sql.Tx) error {
		var checked, until sql.NullInt64
		var etag, modified sql.NullString
		err := tx.QueryRowContext(bg, "SELECT checked_ms, etag, last_modified, not_found_until_ms FROM proxy_state"+
			" WHERE repo = ? AND path = ?", repo, path).Scan(&checked, &etag, &modified, &until)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			s.Row = &ProxyRow{Repo: repo, Path: path, CheckedMS: intOf(checked), ETag: etag.String,
				LastModified: modified.String, NotFoundUntilMS: intOf(until)}
		}
		var reset sql.NullInt64
		err = tx.QueryRowContext(bg, "SELECT checked_ms FROM proxy_state WHERE repo = ? AND path = ?", repo,
			resetPath).Scan(&reset)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		s.ResetMS = intOf(reset)
		return err
	})
	return s, err
}

// PutProxyRow writes the row of one path; an existing row of the path is
// replaced.
func (c *Catalog) PutProxyRow(r ProxyRow) error {
	if r.Repo == "" || r.Path == resetPath {
		return errNoProxyKey
	}
	return c.write(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(bg, "INSERT INTO proxy_state VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (repo, path) DO UPDATE"+
			" SET checked_ms = excluded.checked_ms, etag = excluded.etag, last_modified = excluded.last_modified,"+
			" not_found_until_ms = excluded.not_found_until_ms", r.Repo, r.Path, nullInt(r.CheckedMS), text(r.ETag),
			text(r.LastModified), nullInt(r.NotFoundUntilMS))
		return err
	})
}

// ResetProxyState is the bulk reset of decision 5.17 at the time atMS: the
// not found marks of the repository go, every path becomes stale, a row that
// held no validator goes, and atMS is kept as the time of the reset for the
// paths without a row.
func (c *Catalog) ResetProxyState(repo string, atMS int64) error {
	if repo == "" {
		return errNoProxyKey
	}
	return c.write(func(tx *sql.Tx) error {
		for _, statement := range []string{
			"UPDATE proxy_state SET checked_ms = NULL, not_found_until_ms = NULL WHERE repo = ? AND path <> ''",
			"DELETE FROM proxy_state WHERE repo = ? AND path <> '' AND etag IS NULL AND last_modified IS NULL",
		} {
			if _, err := tx.ExecContext(bg, statement, repo); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(bg, "INSERT INTO proxy_state (repo, path, checked_ms) VALUES (?, ?, ?)"+
			" ON CONFLICT (repo, path) DO UPDATE SET checked_ms = excluded.checked_ms", repo, resetPath, atMS)
		return err
	})
}

// DropProxyState removes every row of a repository, its reset time among
// them.
func (c *Catalog) DropProxyState(repo string) error {
	return c.write(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(bg, "DELETE FROM proxy_state WHERE repo = ?", repo)
		return err
	})
}

// DropRepository removes every row of a repository: the asset and the blob
// rows of each of its paths through Delete, then its proxy state (decision
// A7 of the phase 6 plan, the delete of a repository).
func (c *Catalog) DropRepository(repo string) error {
	var paths []string
	err := c.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT path FROM blobs WHERE repo = ? UNION SELECT path FROM assets WHERE repo = ?", repo, repo)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				return err
			}
			paths = append(paths, path)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := c.Delete(repo, path); err != nil {
			return err
		}
	}
	return c.DropProxyState(repo)
}
