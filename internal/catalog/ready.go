package catalog

import (
	"database/sql"

	"github.com/iogentic/pipesmith-registry/internal/index"
)

// Behind is the first store, by name, whose rows in scans differ from the
// counts of the index (decision R10 of the phase 1c plan): a store the index
// counts with files and the catalog does not, the other way round, or one
// whose count of blobs or bytes differs. "" means the catalog holds what the
// index holds. stats is index.StoreStats of the same data directory.
func (c *Catalog) Behind(stats []index.StoreStat) (string, error) {
	scanned := map[string][2]int64{}
	err := c.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT store, blobs, bytes FROM scans ORDER BY store")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }() // read only; an error of the reads is the one returned
		for rows.Next() {
			var store string
			var blobs, bytes int64
			if err := rows.Scan(&store, &blobs, &bytes); err != nil {
				return err
			}
			scanned[store] = [2]int64{blobs, bytes}
		}
		return rows.Err()
	})
	if err != nil {
		return "", err
	}
	first := ""
	note := func(store string) {
		if first == "" || store < first {
			first = store
		}
	}
	counted := map[string]bool{}
	for _, s := range stats {
		counted[s.Store] = true
		if scanned[s.Store] != [2]int64{s.Files, s.Bytes} {
			note(s.Store)
		}
	}
	for store, n := range scanned {
		if !counted[store] && n != [2]int64{} {
			note(store)
		}
	}
	return first, nil
}
