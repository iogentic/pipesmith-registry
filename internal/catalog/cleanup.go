package catalog

import (
	"database/sql"
	"errors"
	"time"
)

// AssetRow is one asset of a repository as a cleanup selection reads it
// (task 12 of the phase 6 plan): the store of its winning blob, its
// coordinates, the creation time of that blob and the two download times.
// A zero time is none.
type AssetRow struct {
	Store, Path                             string
	Group, Name, Version                    string
	Created, Downloaded, ImportedDownloaded time.Time
}

// AssetRows are the assets of a repository, sorted by path.
func (c *Catalog) AssetRows(repo string) ([]AssetRow, error) {
	var out []AssetRow
	err := c.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT a.path, a.store, coalesce(c.namespace, ''), coalesce(c.name, ''),"+
			" coalesce(c.version, ''), b.created_ms, a.last_downloaded_ms, i.last_downloaded_ms FROM assets a"+
			" JOIN blobs b ON b.store = a.store AND b.repo = a.repo AND b.path = a.path"+
			" LEFT JOIN components c ON c.id = a.component"+
			" LEFT JOIN imported_assets i ON i.repo = a.repo AND i.path = a.path AND i.path <> ''"+
			" WHERE a.repo = ? ORDER BY a.path", repo)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }() // read only; the scan error is the one returned
		for rows.Next() {
			var r AssetRow
			var created int64
			var downloaded, imported sql.NullInt64
			if err := rows.Scan(&r.Path, &r.Store, &r.Group, &r.Name, &r.Version, &created, &downloaded, &imported); err != nil {
				return err
			}
			r.Created = time.UnixMilli(created).UTC()
			r.Downloaded, r.ImportedDownloaded = timeOf(downloaded), timeOf(imported)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func timeOf(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

// BlobCreated is the creation time in milliseconds of the blob row of a
// store for a path, false for no row.
func (c *Catalog) BlobCreated(store, repo, path string) (int64, bool, error) {
	var created int64
	found := false
	err := c.read(func(tx *sql.Tx) error {
		err := tx.QueryRowContext(bg, "SELECT created_ms FROM blobs WHERE store = ? AND repo = ? AND path = ?", store, repo,
			path).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return created, found, err
}

// DropMarked removes the row of a store for a path when it describes the
// blob created at createdMS, the blob a compaction removed; a row of a
// newer blob of the path stays. The caller holds the lock of the path.
func (c *Catalog) DropMarked(store, repo, path string, createdMS int64) error {
	created, found, err := c.BlobCreated(store, repo, path)
	if err != nil || !found || created != createdMS {
		return err
	}
	return c.Drop(repo, path, store)
}
