package state

import (
	"database/sql"
	"errors"
	"fmt"
)

// Setting reads one row of the table settings: its value as the JSON that
// encode wrote, and found false for a key without a row.
func (st *Store) Setting(key string) (value []byte, found bool, err error) {
	var text string
	err = st.readers.QueryRowContext(bg, "SELECT value FROM settings WHERE key = ?", key).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(text), true, nil
}

// PutSetting writes one row of the table settings, value encoded as JSON.
// The rows realms and anonymous belong to Write and are refused.
func (st *Store) PutSetting(key string, value any) error {
	if key == settingRealms || key == settingAnonymous {
		return fmt.Errorf("the setting %s is written with its record kind", key)
	}
	return st.write(func(tx *sql.Tx) error { return setting(tx, key, value) })
}
