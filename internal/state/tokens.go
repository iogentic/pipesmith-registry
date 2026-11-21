package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The records of schema step 2: the tokens this server issues and the
// generation, the value of the meta row generation. Every identity write
// raises the generation in its own transaction: a Write of any kind, a token
// issued or revoked, a password form replaced. A running server reads the
// value to see that another process changed the identity state. The last
// use of a token is no identity write and leaves it.

const generationKey = "generation"

// Generation is the value of the generation row.
func (st *Store) Generation() (int64, error) {
	var g int64
	err := st.readers.QueryRowContext(bg, "SELECT value FROM meta WHERE key = ?", generationKey).Scan(&g)
	if err != nil {
		return 0, fmt.Errorf("%s: generation: %w", st.path, err)
	}
	return g, nil
}

// identityWrite runs fn and raises the generation in one transaction.
func (st *Store) identityWrite(fn func(tx *sql.Tx) error) error {
	return st.write(func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO meta VALUES (?, 1) ON CONFLICT (key) DO UPDATE SET value = value + 1", generationKey)
	})
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// AddToken writes a new token row, then revokes the oldest rows with the
// empty name, the npm logins, of the same user, source and kind beyond keep
// of them, in the order of their issue (decision N10 of the phase 3 plan).
// A named row is never revoked here: a named row of a user, source and kind
// that holds keep named rows is refused with model.ErrTokenLimit. The order
// of issue is the rowid, not the created time, so a clock that steps back
// never revokes the row just written. It returns the public halves it
// revoked. A name that another row of the same user, source and kind holds
// is refused with model.ErrTokenName (step 6).
func (st *Store) AddToken(t model.Token, keep int) ([]string, error) {
	switch {
	case t.Public == "", t.Kind == "", t.UserID == "", t.Source == "":
		return nil, errors.New("a token row needs a public half, a kind, a user and a source")
	case keep < 1:
		return nil, fmt.Errorf("a user keeps at least one token, not %d", keep)
	}
	var revoked []string
	err := st.identityWrite(func(tx *sql.Tx) error {
		if t.Name != "" {
			var n int
			if err := tx.QueryRowContext(bg, "SELECT count(*) FROM tokens WHERE user_id = ? AND source = ? AND kind = ? AND name = ?",
				t.UserID, t.Source, t.Kind, t.Name).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return model.ErrTokenName
			}
			if err := tx.QueryRowContext(bg, "SELECT count(*) FROM tokens WHERE user_id = ? AND source = ? AND kind = ? AND name != ''",
				t.UserID, t.Source, t.Kind).Scan(&n); err != nil {
				return err
			}
			if n >= keep {
				return model.ErrTokenLimit
			}
		}
		if err := exec(tx, "INSERT INTO tokens (public, kind, user_id, source, digest, created, last_used, name) VALUES (?, ?, ?, ?, ?, ?, NULL, ?)",
			t.Public, t.Kind, t.UserID, t.Source, t.Digest[:], ms(t.Created), t.Name); err != nil {
			return fmt.Errorf("token %s: %w", t.Public, err)
		}
		rows, err := tx.QueryContext(bg, "SELECT public FROM tokens WHERE user_id = ? AND source = ? AND kind = ? AND name = ''"+
			" ORDER BY rowid DESC LIMIT -1 OFFSET ?", t.UserID, t.Source, t.Kind, keep)
		if err != nil {
			return err
		}
		for rows.Next() {
			var public string
			if err := rows.Scan(&public); err != nil {
				_ = rows.Close() // the scan error counts
				return err
			}
			revoked = append(revoked, public)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		// oldest first, the order of their issue
		for i, j := 0, len(revoked)-1; i < j; i, j = i+1, j-1 {
			revoked[i], revoked[j] = revoked[j], revoked[i]
		}
		for _, public := range revoked {
			if err := exec(tx, "DELETE FROM tokens WHERE public = ?", public); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

const tokenColumns = "public, kind, user_id, source, digest, created, last_used, name"

func scanToken(scan func(dest ...any) error) (model.Token, error) {
	var t model.Token
	var digest []byte
	var created int64
	var lastUsed sql.NullInt64
	if err := scan(&t.Public, &t.Kind, &t.UserID, &t.Source, &digest, &created, &lastUsed, &t.Name); err != nil {
		return model.Token{}, err
	}
	if len(digest) != len(t.Digest) {
		return model.Token{}, fmt.Errorf("token %s: the digest has %d bytes, not %d", t.Public, len(digest), len(t.Digest))
	}
	copy(t.Digest[:], digest)
	t.Created = fromMS(created)
	if lastUsed.Valid {
		t.LastUsed = fromMS(lastUsed.Int64)
	}
	return t, nil
}

// Token is the row of a public half; false when there is none.
func (st *Store) Token(public string) (model.Token, bool, error) {
	row := st.readers.QueryRowContext(bg, "SELECT "+tokenColumns+" FROM tokens WHERE public = ?", public)
	t, err := scanToken(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Token{}, false, nil
	}
	if err != nil {
		return model.Token{}, false, err
	}
	return t, true, nil
}

// Tokens are the rows of a user of a source, of every kind, in the order of
// their issue.
func (st *Store) Tokens(userID, source string) ([]model.Token, error) {
	rows, err := st.readers.QueryContext(bg, "SELECT "+tokenColumns+" FROM tokens WHERE user_id = ? AND source = ?"+
		" ORDER BY created, rowid", userID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []model.Token{}
	for rows.Next() {
		t, err := scanToken(rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// RemoveToken deletes the row of a public half; false when there was none,
// and then the generation stays.
func (st *Store) RemoveToken(public string) (bool, error) {
	errNone := errors.New("no row")
	err := st.identityWrite(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(bg, "DELETE FROM tokens WHERE public = ?", public)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			err = errNone
		}
		return err
	})
	if errors.Is(err, errNone) {
		return false, nil
	}
	return err == nil, err
}

// TouchToken sets the time of the last use of a row. It is no identity
// write: the generation stays.
func (st *Store) TouchToken(public string, at time.Time) error {
	return st.write(func(tx *sql.Tx) error {
		return exec(tx, "UPDATE tokens SET last_used = ? WHERE public = ?", ms(at), public)
	})
}

// SetPasswordHash replaces the stored form of a local user when the row
// still holds the form old, and raises the generation then. false means the
// row holds another form or the user is gone, and nothing changed.
func (st *Store) SetPasswordHash(userID, old, hash string) (bool, error) {
	if hash == "" {
		return false, errors.New("an empty password form is no password")
	}
	errSame := errors.New("no row changed")
	err := st.identityWrite(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(bg, "UPDATE users SET password_hash = ? WHERE id = ? AND password_hash = ?", hash, userID, old)
		if err != nil {
			return fmt.Errorf("password form of user %q: %w", userID, err)
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			err = errSame
		}
		return err
	})
	if errors.Is(err, errSame) {
		return false, nil
	}
	return err == nil, err
}

// ImportedKeyRows are the imported keys of a user of a source, digests
// included, by kind and time of creation; every imported key when userID is
// empty. The digests stay in the process: GET /api/tokens names a key by an
// id made from its digest (auth.ImportedKeyID).
func (st *Store) ImportedKeyRows(userID, source string) ([]model.ImportedKey, error) {
	query, args := "SELECT digest, kind, user_id, source, created, last_used FROM imported_keys", []any{}
	if userID != "" {
		query, args = query+" WHERE user_id = ? AND source = ?", []any{userID, source}
	}
	rows, err := st.readers.QueryContext(bg, query+" ORDER BY kind, created", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []model.ImportedKey{}
	for rows.Next() {
		var k model.ImportedKey
		var raw []byte
		var createdMS int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&raw, &k.Kind, &k.UserID, &k.Source, &createdMS, &lastUsed); err != nil {
			return nil, err
		}
		if len(raw) != len(k.Digest) {
			return nil, fmt.Errorf("%s: the digest has %d bytes, not %d", k, len(raw), len(k.Digest))
		}
		copy(k.Digest[:], raw)
		k.Created = fromMS(createdMS)
		if lastUsed.Valid {
			k.LastUsed = fromMS(lastUsed.Int64)
		}
		list = append(list, k)
	}
	return list, rows.Err()
}
