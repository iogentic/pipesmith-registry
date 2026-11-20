package catalog

import (
	"database/sql"
	"sort"
)

// ComponentPaths is the asset paths of the components of repo whose
// coordinates match accepts, in path order. match sees each component of
// repo once, in the order of namespace, name and version; an empty version
// or namespace means the coordinates have none.
func (c *Catalog) ComponentPaths(repo string, match func(namespace, name, version string) bool) ([]string, error) {
	var paths []string
	err := c.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT id, namespace, name, version FROM components WHERE repo = ?"+
			" ORDER BY namespace, name, version", repo)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			var namespace, name, version string
			if err := rows.Scan(&id, &namespace, &name, &version); err != nil {
				_ = rows.Close() // the scan error is the one returned
				return err
			}
			if match(namespace, name, version) {
				ids = append(ids, id)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := collect(tx, &paths, "SELECT path FROM assets WHERE component = ?", id); err != nil {
				return err
			}
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func collect(tx *sql.Tx, out *[]string, query string, args ...any) error {
	rows, err := tx.QueryContext(bg, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }() // read only; an error of the reads is the one returned
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		*out = append(*out, s)
	}
	return rows.Err()
}
