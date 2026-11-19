package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/index"
)

// The rows of single writes, for the catalog hook of the write path
// (seam Catalog hook and decision E5 of the execution plan): Put after a
// write, Delete after a delete. Drop follows index.Drop. A later leg wires
// them; nothing calls them in this one.

// Digests are the lower case hex digests of the bytes of one blob as the
// write path computes them; an empty one is unknown. The fields are those of
// stores.Digests of the write path, in its order, so that the one converts
// into the other.
type Digests struct {
	MD5, SHA1, SHA256, SHA512 string
}

func text(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func pointer(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// Put keeps the row of one blob the write path wrote: a is the row the
// index took, d the digests of its bytes, uploader and address those the
// properties file of the blob names, format the format of the repository
// for the coordinates. The blob replaces the row of its store for the path,
// and the asset of the path takes the winning blob as a scan does. When the
// new blob wins, the last modified time of the asset is its creation time.
func (c *Catalog) Put(a index.Asset, format string, d Digests, uploader, address string) error {
	if !addressable(a.Path) {
		return fmt.Errorf("catalog: %q of %s is no asset path", a.Path, a.Repo)
	}
	now := &blob{key: key{a.Repo, a.Path}, size: a.Size, created: a.CreatedMS, sha1: pointer(a.SHA1),
		contentType: pointer(a.ContentType), uploader: text(uploader), address: text(address), md5: text(d.MD5),
		sha256: text(d.SHA256), sha512: text(d.SHA512)}
	if !now.sha1.Valid {
		now.sha1 = text(d.SHA1)
	}
	return c.write(func(tx *sql.Tx) error {
		m, err := newMerger(tx, func(string) string { return format })
		if err != nil {
			return err
		}
		defer m.close()
		if _, err := tx.ExecContext(bg, "INSERT INTO scans VALUES (?, 0, 0, NULL) ON CONFLICT (store) DO NOTHING", a.Store); err != nil {
			return err
		}
		before, current, others, err := m.rowOf(a.Store, now.key)
		if err != nil {
			return err
		}
		if err := m.settle(a.Store, now.key, before, now, current, false, others, sql.NullInt64{Int64: a.CreatedMS, Valid: true}); err != nil {
			return err
		}
		added, size := int64(1), now.size
		if before != nil {
			added, size = 0, now.size-before.size
		}
		return m.finish(a.Store, added, size)
	})
}

// Delete removes the asset of a path and the row of every store for it,
// after the write path deleted it.
func (c *Catalog) Delete(repo, path string) error {
	return c.write(func(tx *sql.Tx) error {
		m, err := newMerger(tx, nil)
		if err != nil {
			return err
		}
		defer m.close()
		k := key{repo, path}
		current, err := m.assetOf(k)
		if err != nil {
			return err
		}
		if current != nil {
			if err := m.exec("assetDelete", repo, path); err != nil {
				return err
			}
			if err := m.orphan(current.component); err != nil {
				return err
			}
		}
		all, err := m.stores()
		if err != nil {
			return err
		}
		for _, store := range all {
			var size int64
			err := m.tx.QueryRowContext(bg, "SELECT size FROM blobs WHERE store = ? AND repo = ? AND path = ?", store, repo,
				path).Scan(&size)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if err := m.exec("blobDelete", store, repo, path); err != nil {
				return err
			}
			if err := m.count(store, -1, -size); err != nil {
				return err
			}
		}
		return m.sweep()
	})
}

// Drop removes the row of one store for a path, as index.Drop does in the
// index; the asset of the path takes the winning blob of the rows that
// remain.
func (c *Catalog) Drop(repo, path, store string) error {
	return c.write(func(tx *sql.Tx) error {
		var format string
		m, err := newMerger(tx, func(string) string { return format })
		if err != nil {
			return err
		}
		defer m.close()
		k := key{repo, path}
		before, current, others, err := m.rowOf(store, k)
		if err != nil || before == nil {
			return err
		}
		format = "unknown"
		if current != nil {
			format = current.format
		}
		if err := m.settle(store, k, before, nil, current, false, others, sql.NullInt64{}); err != nil {
			return err
		}
		return m.finish(store, -1, -before.size)
	})
}

// rowOf is the row of store for k, nil for none, the asset of k and the
// other stores of the catalog.
func (m *merger) rowOf(store string, k key) (*blob, *asset, []string, error) {
	all, err := m.stores()
	if err != nil {
		return nil, nil, nil, err
	}
	others := slices.DeleteFunc(all, func(s string) bool { return s == store })
	var b blob
	var a asset
	var winner, format sql.NullString
	err = m.tx.QueryRowContext(bg, "SELECT b.size, b.sha1, b.content_type, b.created_ms, b.uploader, b.uploader_ip, b.md5,"+
		" b.sha256, b.sha512, a.store, a.format, a.component, a.extension, a.classifier, a.last_modified_ms,"+
		" a.last_downloaded_ms FROM blobs b LEFT JOIN assets a ON a.repo = b.repo AND a.path = b.path"+
		" WHERE b.store = ? AND b.repo = ? AND b.path = ?", store, k.repo, k.path).Scan(&b.size, &b.sha1, &b.contentType,
		&b.created, &b.uploader, &b.address, &b.md5, &b.sha256, &b.sha512, &winner, &format, &a.component, &a.extension,
		&a.classifier, &a.lastModified, &a.lastDownloaded)
	if errors.Is(err, sql.ErrNoRows) {
		current, err := m.assetOf(k)
		return nil, current, others, err
	}
	if err != nil {
		return nil, nil, nil, err
	}
	b.key = k
	if !winner.Valid {
		return &b, nil, others, nil
	}
	a.store, a.format = winner.String, format.String
	return &b, &a, others, nil
}

// count moves the rows and bytes of store in scans.
func (m *merger) count(store string, rows, bytes int64) error {
	_, err := m.tx.ExecContext(bg, "UPDATE scans SET blobs = blobs + ?, bytes = bytes + ? WHERE store = ?", rows, bytes, store)
	return err
}

// finish writes what waits, removes components without assets and moves
// the counts of store.
func (m *merger) finish(store string, rows, bytes int64) error {
	if err := m.sweep(); err != nil {
		return err
	}
	return m.count(store, rows, bytes)
}
