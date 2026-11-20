package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The keys an import takes over from the old product (decisions K4 to K10 of
// docs/plans/secrets-import.md): kind, user, source and a digest under the
// install key, never a value of a key. A row is found by its kind and
// digest. Every write but the last use raises the generation.

// schemaImportedKeys adds the table of the imported keys.
func schemaImportedKeys(tx *sql.Tx) error {
	_, err := tx.ExecContext(bg, "CREATE TABLE IF NOT EXISTS imported_keys (digest BLOB PRIMARY KEY, kind TEXT NOT NULL,"+
		" user_id TEXT NOT NULL, source TEXT NOT NULL, created INTEGER NOT NULL, last_used INTEGER) WITHOUT ROWID")
	return err
}

// WriteImport is Write plus the imported keys of an import in the same
// transaction: keys nil keeps the imported keys as they are, any other list
// replaces all of them. With keys, kinds may be empty: the import of the set
// secrets changes no other row.
func (st *Store) WriteImport(kinds []Kind, snap *model.Snapshot, keys []model.ImportedKey) error {
	if len(kinds) > 0 || keys == nil {
		if err := checkKinds(kinds); err != nil {
			return err
		}
	}
	if snap == nil {
		snap = &model.Snapshot{}
	}
	return st.identityWrite(func(tx *sql.Tx) error {
		if err := writeKinds(tx, kinds, snap); err != nil {
			return err
		}
		if keys == nil {
			return nil
		}
		return replaceImportedKeys(tx, keys)
	})
}

// UpdateImport is Update plus the imported keys of an import in the same
// transaction. keys gets the records as fn left them; a nil list keeps the
// imported keys as they are, any other list replaces all of them. With keys,
// kinds may be empty, as for WriteImport.
func (st *Store) UpdateImport(kinds []Kind, fn func(snap *model.Snapshot) error,
	keys func(snap *model.Snapshot) []model.ImportedKey) error {
	if len(kinds) > 0 || keys == nil {
		if err := checkKinds(kinds); err != nil {
			return err
		}
	}
	return st.identityWrite(func(tx *sql.Tx) error {
		snap, err := loadAll(tx)
		if err != nil {
			return err
		}
		if err := fn(snap); err != nil {
			return err
		}
		if err := writeKinds(tx, kinds, snap); err != nil {
			return err
		}
		if keys == nil {
			return nil
		}
		rows := keys(snap)
		if rows == nil {
			return nil
		}
		return replaceImportedKeys(tx, rows)
	})
}

func replaceImportedKeys(tx *sql.Tx, keys []model.ImportedKey) error {
	if err := exec(tx, "DELETE FROM imported_keys"); err != nil {
		return err
	}
	for _, k := range keys {
		if k.Kind == "" || k.UserID == "" || k.Source == "" {
			return errors.New("an imported key needs a kind, a user and a source")
		}
		if err := exec(tx, "INSERT INTO imported_keys VALUES (?, ?, ?, ?, ?, NULL)", k.Digest[:], k.Kind, k.UserID,
			k.Source, ms(k.Created)); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// ImportedKey is the imported key of a kind with the digest; false when
// there is none.
func (st *Store) ImportedKey(kind string, digest [32]byte) (model.ImportedKey, bool, error) {
	var k model.ImportedKey
	var raw []byte
	var createdMS int64
	var lastUsed sql.NullInt64
	err := st.readers.QueryRowContext(bg, "SELECT digest, kind, user_id, source, created, last_used FROM imported_keys"+
		" WHERE digest = ? AND kind = ?", digest[:], kind).Scan(&raw, &k.Kind, &k.UserID, &k.Source, &createdMS, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ImportedKey{}, false, nil
	}
	if err != nil {
		return model.ImportedKey{}, false, err
	}
	if len(raw) != len(k.Digest) {
		return model.ImportedKey{}, false, fmt.Errorf("%s: the digest has %d bytes, not %d", k, len(raw), len(k.Digest))
	}
	copy(k.Digest[:], raw)
	k.Created = fromMS(createdMS)
	if lastUsed.Valid {
		k.LastUsed = fromMS(lastUsed.Int64)
	}
	return k, true, nil
}

// ImportedKeyCounts are the imported keys per kind.
func (st *Store) ImportedKeyCounts() (map[string]int, error) {
	rows, err := st.readers.QueryContext(bg, "SELECT kind, count(*) FROM imported_keys GROUP BY kind")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		counts[kind] = n
	}
	return counts, rows.Err()
}

// RemoveImportedKey deletes the imported key of a digest; false when there
// was none, and then the generation stays.
func (st *Store) RemoveImportedKey(digest [32]byte) (bool, error) {
	n, err := st.deleteImported("DELETE FROM imported_keys WHERE digest = ?", digest[:])
	return n > 0, err
}

// ForgetImportedKeys deletes every imported key and returns their number
// (decision K10).
func (st *Store) ForgetImportedKeys() (int, error) {
	n, err := st.deleteImported("DELETE FROM imported_keys")
	return int(n), err
}

func (st *Store) deleteImported(query string, args ...any) (int64, error) {
	errNone := errors.New("no row")
	var n int64
	err := st.identityWrite(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(bg, query, args...)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err == nil && n == 0 {
			err = errNone
		}
		return err
	})
	if errors.Is(err, errNone) {
		return 0, nil
	}
	return n, err
}

// TouchImportedKey sets the time of the last use of an imported key. It is
// no identity write: the generation stays.
func (st *Store) TouchImportedKey(digest [32]byte, at time.Time) error {
	return st.write(func(tx *sql.Tx) error {
		return exec(tx, "UPDATE imported_keys SET last_used = ? WHERE digest = ?", ms(at), digest[:])
	})
}

// ImportedKeysOf are the imported keys of a user of a source, by kind.
func (st *Store) ImportedKeysOf(userID, source string) ([]model.ImportedKey, error) {
	rows, err := st.readers.QueryContext(bg, "SELECT kind, created, last_used FROM imported_keys WHERE user_id = ? AND source = ?"+
		" ORDER BY kind, created", userID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []model.ImportedKey{}
	for rows.Next() {
		k := model.ImportedKey{UserID: userID, Source: source}
		var createdMS int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&k.Kind, &createdMS, &lastUsed); err != nil {
			return nil, err
		}
		k.Created = fromMS(createdMS)
		if lastUsed.Valid {
			k.LastUsed = fromMS(lastUsed.Int64)
		}
		list = append(list, k)
	}
	return list, rows.Err()
}
