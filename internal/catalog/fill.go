package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The fill of one store. A scan hands every entry it gives the index to a
// Fill, which stages them in sorted runs in a folder beside the catalog file
// (staging.go) with the rule of the index for a path a store holds twice:
// the newer blob wins, on a tie the first one. Commit merges the runs and
// compares the staged rows with the rows the catalog holds for the store, in
// one write transaction, and writes the difference. An unchanged blob keeps
// its row and the digests a write gave it.

const (
	// stagingMaxAge is the age after which a staging folder is taken for
	// the leftover of a process that ended during a fill, and removed.
	stagingMaxAge  = 24 * time.Hour
	chunkRows      = 4096
	componentCache = 100_000
)

// Formats gives the format of a repository; its coordinates follow it.
type Formats func(repo string) string

// ErrFillDone is the error of a Fill used after Commit or Discard.
var ErrFillDone = errors.New("the fill of the catalog is done")

// Fill takes the entries of one scan of one store. A Fill belongs to one
// goroutine.
type Fill struct {
	c     *Catalog
	store string
	stage *staging
	done  bool
	held  bool // Hold took the writer; Commit or Discard gives it back
}

func addressable(path string) bool {
	return path != "" && !strings.HasPrefix(path, "/") && !strings.HasSuffix(path, "/")
}

// Begin starts the fill of a store: a new staging folder beside the
// catalog file takes the entries. Staging folders older than a day are
// removed first.
func (c *Catalog) Begin(store string) (*Fill, error) {
	dir := filepath.Dir(c.path)
	removeStale(dir)
	stage, err := os.MkdirTemp(dir, FileName+".fill-*")
	if err != nil {
		return nil, err
	}
	return &Fill{c: c, store: store, stage: &staging{dir: stage}}, nil
}

// removeStale removes what fills of dir left whose last change is older
// than stagingMaxAge. One that cannot be removed stays.
func removeStale(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), FileName+".fill-") || entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > stagingMaxAge {
			_ = os.RemoveAll(filepath.Join(dir, entry.Name())) // a leftover; one that stays is tried again by the next fill
		}
	}
}

// Add stages one entry. An entry whose path is empty or starts or ends with
// a slash is skipped, as the index skips it.
func (f *Fill) Add(e stores.Entry) error {
	if f.done {
		return ErrFillDone
	}
	if !addressable(e.Path) {
		return nil
	}
	return f.stage.add(e)
}

// Choose names the blob of a key that the arbiter of the store kept
// (index.Arbiter): Commit takes the staged entry at location for the path
// instead of the newer one, and no entry when location is "", the row the
// arbiter dropped. A key the arbiter never decided keeps the rule of the
// index.
func (f *Fill) Choose(repo, path, location string) {
	if f.stage.chosen == nil {
		f.stage.chosen = map[key]string{}
	}
	f.stage.chosen[key{repo, path}] = location
}

// Hold takes the writer of the catalog for Commit: no other write of this
// opening runs from then on until the fill ends. A scan holds it before the
// index takes its rows, so that a write of the hook that follows the index
// waits for the fill and is not undone by it.
func (f *Fill) Hold() error {
	if f.done {
		return ErrFillDone
	}
	if f.held {
		return nil
	}
	if err := f.c.lock(); err != nil {
		return err
	}
	f.held = true
	return nil
}

// Discard ends the fill without a change of the catalog and removes the
// staging folder. A second call does nothing.
func (f *Fill) Discard() error {
	if f.done {
		return nil
	}
	f.done = true
	if f.held {
		f.held = false
		f.c.unlock()
	}
	return os.RemoveAll(f.stage.dir)
}

// Commit writes the staged entries as the rows of the store, in one write
// transaction, and ends the fill. held names the stores the index holds now:
// the rows of every other store leave the catalog in the same transaction.
// formats gives the format of each repository for the coordinates.
func (f *Fill) Commit(formats Formats, held []string) error {
	if f.done {
		return ErrFillDone
	}
	source, err := f.stage.source()
	if err == nil {
		write := f.c.write
		if f.held {
			write = f.c.writeLocked
		}
		err = write(func(tx *sql.Tx) error { return f.apply(tx, source, formats, held) })
		err = errors.Join(err, source.close())
	}
	return errors.Join(err, f.Discard())
}

func (f *Fill) apply(tx *sql.Tx, source rowSource, formats Formats, held []string) error {
	if _, err := tx.ExecContext(bg, fmt.Sprintf("PRAGMA cache_size = -%d", writeCacheKiB)); err != nil {
		return err
	}
	m, err := newMerger(tx, formats)
	if err != nil {
		return err
	}
	defer m.close()
	known, err := m.stores()
	if err != nil {
		return err
	}
	kept := []string{f.store}
	var gone []string
	for _, store := range known {
		switch {
		case store == f.store:
		case slices.Contains(held, store):
			kept = append(kept, store)
		default:
			gone = append(gone, store)
		}
	}
	for _, store := range gone {
		if _, err := m.merge(store, noRows{}, kept); err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, "DELETE FROM scans WHERE store = ?", store); err != nil {
			return err
		}
	}
	total, err := m.merge(f.store, source, slices.DeleteFunc(slices.Clone(kept), func(s string) bool { return s == f.store }))
	if err != nil {
		return err
	}
	if err := m.sweep(); err != nil {
		return err
	}
	_, err = tx.ExecContext(bg, "INSERT INTO scans VALUES (?, ?, ?, ?) ON CONFLICT (store) DO UPDATE SET"+
		" blobs = excluded.blobs, bytes = excluded.bytes, scanned_ms = excluded.scanned_ms",
		f.store, total.blobs, total.bytes, time.Now().UnixMilli())
	return err
}

// key is the key of an asset: repository and path.
type key struct{ repo, path string }

func (k key) less(o key) bool {
	return k.repo < o.repo || k.repo == o.repo && k.path < o.path
}

// blob is one row of blobs without its store.
type blob struct {
	key
	size, created                        int64
	sha1, contentType, uploader, address sql.NullString
	md5, sha256, sha512                  sql.NullString
}

// asset is one row of assets without its key.
type asset struct {
	store, format                string
	component                    sql.NullInt64
	extension, classifier        sql.NullString
	lastModified, lastDownloaded sql.NullInt64
}

func (b *blob) digests() bool { return b.md5.Valid || b.sha256.Valid || b.sha512.Valid }

// sameBlob tells whether two rows describe the same bytes, so that the
// digests of a write stay with the row.
func sameBlob(a, b *blob) bool {
	return a.created == b.created && a.size == b.size && a.sha1 == b.sha1
}

func sameRow(a, b *blob) bool {
	return sameBlob(a, b) && a.contentType == b.contentType && a.uploader == b.uploader && a.address == b.address &&
		a.md5 == b.md5 && a.sha256 == b.sha256 && a.sha512 == b.sha512
}

// rowSource gives the new rows of a store in the order of their key.
type rowSource interface {
	next() (*blob, error)
}

type noRows struct{}

func (noRows) next() (*blob, error) { return nil, nil }

// held is a row of a store as the catalog holds it, with the asset row of
// its key.
type held struct {
	blob
	asset *asset
}

// heldRows reads the rows of one store with their assets in the order of
// their key, a chunk at a time, so that no cursor stays open while the merge
// writes.
type heldRows struct {
	stmt  *sql.Stmt
	store string
	last  key
	rows  []held
	done  bool
}

const heldQuery = "SELECT b.repo, b.path, b.size, b.sha1, b.content_type, b.created_ms, b.uploader, b.uploader_ip," +
	" b.md5, b.sha256, b.sha512, a.store, a.format, a.component, a.extension, a.classifier, a.last_modified_ms," +
	" a.last_downloaded_ms FROM blobs b LEFT JOIN assets a ON a.repo = b.repo AND a.path = b.path" +
	" WHERE b.store = ? AND (b.repo, b.path) > (?, ?) ORDER BY b.repo, b.path LIMIT ?"

func (h *heldRows) next() (*held, error) {
	if len(h.rows) == 0 {
		if h.done {
			return nil, nil
		}
		if err := h.fetch(); err != nil || len(h.rows) == 0 {
			return nil, err
		}
	}
	row := h.rows[0]
	h.rows = h.rows[1:]
	return &row, nil
}

func (h *heldRows) fetch() error {
	rows, err := h.stmt.QueryContext(bg, h.store, h.last.repo, h.last.path, chunkRows)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r held
		var a asset
		var store, format sql.NullString
		if err := rows.Scan(&r.repo, &r.path, &r.size, &r.sha1, &r.contentType, &r.created, &r.uploader, &r.address,
			&r.md5, &r.sha256, &r.sha512, &store, &format, &a.component, &a.extension, &a.classifier, &a.lastModified,
			&a.lastDownloaded); err != nil {
			return err
		}
		if store.Valid {
			a.store, a.format = store.String, format.String
			r.asset = &a
		}
		h.rows = append(h.rows, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	h.done = len(h.rows) < chunkRows
	if len(h.rows) > 0 {
		h.last = h.rows[len(h.rows)-1].key
	}
	return nil
}
