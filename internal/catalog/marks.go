package catalog

import (
	"database/sql"
	"errors"
	"maps"
	"slices"
	"time"
)

// The count of the blobs with the delete mark of each store and their
// bytes (rulings SR14 and SR15 of leg 6-stores-b), in store_marks of step 6.
// A scan writes the row of its store with its fill; the write path and the
// compaction move it by deltas; a store without a row takes no delta.

// Marks is the count of the marked blobs of a store with the time it was
// counted; as a delta Counted is not read.
type Marks struct {
	Blobs, Bytes int64
	Counted      time.Time
}

// StoreMarks is the row of a store; found is false for a store that was
// never counted.
func (c *Catalog) StoreMarks(store string) (Marks, bool, error) {
	var m Marks
	found := false
	err := c.read(func(tx *sql.Tx) error {
		var ms int64
		err := tx.QueryRowContext(bg, "SELECT blobs, bytes, counted_ms FROM store_marks WHERE store = ?", store).Scan(&m.Blobs, &m.Bytes, &ms)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		m.Counted, found = time.UnixMilli(ms).UTC(), true
		return nil
	})
	return m, found, err
}

func setMarks(tx *sql.Tx, store string, m Marks) error {
	_, err := tx.ExecContext(bg, "INSERT INTO store_marks (store, blobs, bytes, counted_ms) VALUES (?, ?, ?, ?)"+
		" ON CONFLICT (store) DO UPDATE SET blobs = excluded.blobs, bytes = excluded.bytes, counted_ms = excluded.counted_ms",
		store, m.Blobs, m.Bytes, m.Counted.UnixMilli())
	return err
}

// SetStoreMarks writes the row of a count of the store: a walk or an
// imported count.
func (c *Catalog) SetStoreMarks(store string, m Marks) error {
	return c.write(func(tx *sql.Tx) error { return setMarks(tx, store, m) })
}

// AddStoreMarks moves the rows of the stores by their deltas, never below
// zero; a store without a row takes none.
func (c *Catalog) AddStoreMarks(deltas map[string]Marks) error {
	return c.write(func(tx *sql.Tx) error {
		for _, store := range slices.Sorted(maps.Keys(deltas)) {
			d := deltas[store]
			if _, err := tx.ExecContext(bg, "UPDATE store_marks SET blobs = max(blobs + ?, 0), bytes = max(bytes + ?, 0) WHERE store = ?",
				d.Blobs, d.Bytes, store); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetMarks gives the fill the count of the marked blobs of its walk; Commit
// writes it with the rows of the store.
func (f *Fill) SetMarks(m Marks) { f.marks = &m }
