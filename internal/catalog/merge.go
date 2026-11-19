package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/coords"
)

// batchRows is the number of rows of one multi-row INSERT of the merge.
const batchRows = 128

// componentKey is the identity of a component row.
type componentKey struct {
	repo, format, namespace, name, version string
}

// inserter collects the rows of one table for multi-row INSERTs.
type inserter struct {
	head, marks string
	columns     int
	full        *sql.Stmt
	args        []any
}

func newInserter(tx *sql.Tx, table string, columns int) (*inserter, error) {
	in := &inserter{head: "INSERT INTO " + table + " VALUES ", columns: columns,
		marks: "(" + strings.TrimSuffix(strings.Repeat("?, ", columns), ", ") + ")"}
	full, err := tx.PrepareContext(bg, in.statement(batchRows))
	in.full = full
	return in, err
}

func (in *inserter) statement(rows int) string {
	return in.head + strings.TrimSuffix(strings.Repeat(in.marks+", ", rows), ", ")
}

func (in *inserter) pending() bool { return len(in.args) > 0 }

func (in *inserter) flush(tx *sql.Tx) error {
	rows := len(in.args) / in.columns
	if rows == 0 {
		return nil
	}
	var err error
	if rows == batchRows {
		_, err = in.full.ExecContext(bg, in.args...)
	} else {
		_, err = tx.ExecContext(bg, in.statement(rows), in.args...)
	}
	in.args = in.args[:0]
	return err
}

// merger writes the rows of stores and the assets of their keys inside one
// write transaction. New rows of blobs, components and assets wait in
// batches, which are written in that order, the order of their foreign
// keys, before any other write and before the transaction ends. A new
// component takes the next free id at once, so its assets can name it while
// it waits.
type merger struct {
	tx         *sql.Tx
	formats    Formats
	statements map[string]*sql.Stmt
	blobs      *inserter
	assets     *inserter
	comps      *inserter
	components map[componentKey]int64
	// fresh holds per repository seen whether it had no component when this
	// merger saw it first; the components of such a repository are all made
	// here, so the cache knows them and no lookup is needed.
	fresh   map[string]bool
	nextID  int64
	orphans map[int64]bool
	// holds tells per store and repository whether the store has a row of
	// the repository, so that only those stores are probed for a path.
	holds map[[2]string]bool
}

var mergeStatements = map[string]string{
	"held":        heldQuery,
	"probe":       "SELECT created_ms FROM blobs WHERE store = ? AND repo = ? AND path = ?",
	"blobUpdate":  "UPDATE blobs SET size = ?, sha1 = ?, content_type = ?, created_ms = ?, uploader = ?, uploader_ip = ?, md5 = ?, sha256 = ?, sha512 = ? WHERE store = ? AND repo = ? AND path = ?",
	"blobDelete":  "DELETE FROM blobs WHERE store = ? AND repo = ? AND path = ?",
	"assetGet":    "SELECT store, format, component, extension, classifier, last_modified_ms, last_downloaded_ms FROM assets WHERE repo = ? AND path = ?",
	"assetUpdate": "UPDATE assets SET store = ?, format = ?, component = ?, extension = ?, classifier = ?, last_modified_ms = ?, last_downloaded_ms = ? WHERE repo = ? AND path = ?",
	"assetDelete": "DELETE FROM assets WHERE repo = ? AND path = ?",
	"compGet":     "SELECT id FROM components WHERE repo = ? AND format = ? AND namespace = ? AND name = ? AND version = ?",
	"compAny":     "SELECT count(*) FROM (SELECT 1 FROM components WHERE repo = ? LIMIT 1)",
	"repoAny":     "SELECT count(*) FROM (SELECT 1 FROM blobs WHERE store = ? AND repo = ? LIMIT 1)",
	"compOrphan":  "DELETE FROM components WHERE id = ? AND NOT EXISTS (SELECT 1 FROM assets WHERE component = ?)",
}

func newMerger(tx *sql.Tx, formats Formats) (m *merger, err error) {
	m = &merger{tx: tx, formats: formats, statements: map[string]*sql.Stmt{}, components: map[componentKey]int64{},
		fresh: map[string]bool{}, orphans: map[int64]bool{}, holds: map[[2]string]bool{}}
	defer func() {
		if err != nil {
			m.close()
		}
	}()
	for name, query := range mergeStatements {
		stmt, err := tx.PrepareContext(bg, query)
		if err != nil {
			return nil, fmt.Errorf("statement %s: %w", name, err)
		}
		m.statements[name] = stmt
	}
	if m.blobs, err = newInserter(tx, "blobs", 12); err != nil {
		return nil, err
	}
	if m.comps, err = newInserter(tx, "components", 7); err != nil {
		return nil, err
	}
	if m.assets, err = newInserter(tx, "assets", 9); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(bg, "SELECT coalesce(max(id), 0) + 1 FROM components").Scan(&m.nextID); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *merger) close() {
	for _, stmt := range m.statements {
		_ = stmt.Close() // the transaction ends with them; an error of a statement was returned where it ran
	}
	for _, in := range []*inserter{m.blobs, m.comps, m.assets} {
		if in != nil && in.full != nil {
			_ = in.full.Close() // as above
		}
	}
}

// flush writes the waiting rows: components and blobs before the assets
// that name them.
func (m *merger) flush() error {
	for _, in := range []*inserter{m.comps, m.blobs, m.assets} {
		if err := in.flush(m.tx); err != nil {
			return err
		}
	}
	return nil
}

// exec runs a statement that is no INSERT, after the waiting rows.
func (m *merger) exec(name string, args ...any) error {
	if m.comps.pending() || m.blobs.pending() || m.assets.pending() {
		if err := m.flush(); err != nil {
			return err
		}
	}
	_, err := m.statements[name].ExecContext(bg, args...)
	return err
}

func (m *merger) queue(in *inserter, args ...any) error {
	in.args = append(in.args, args...)
	if len(in.args) == batchRows*in.columns && in == m.assets {
		return m.flush()
	}
	if len(in.args) == batchRows*in.columns {
		return in.flush(m.tx)
	}
	return nil
}

// stores are the stores the catalog holds rows of, by their rows in scans.
func (m *merger) stores() ([]string, error) {
	rows, err := m.tx.QueryContext(bg, "SELECT store FROM scans ORDER BY store")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []string
	for rows.Next() {
		var store string
		if err := rows.Scan(&store); err != nil {
			return nil, err
		}
		list = append(list, store)
	}
	return list, rows.Err()
}

type totals struct{ blobs, bytes int64 }

// merge makes the rows of store those of source and settles the asset of
// every key it touches; others are the stores whose rows compete for a path.
// It returns the rows and bytes of the store afterwards.
func (m *merger) merge(store string, source rowSource, others []string) (totals, error) {
	var sum totals
	old := &heldRows{stmt: m.statements["held"], store: store}
	cur, err := source.next()
	if err != nil {
		return sum, err
	}
	prev, err := old.next()
	if err != nil {
		return sum, err
	}
	for cur != nil || prev != nil {
		var k key
		switch {
		case prev == nil:
			k = cur.key
		case cur == nil:
			k = prev.key
		case cur.key.less(prev.key):
			k = cur.key
		default:
			k = prev.key
		}
		var now *blob
		var was *held
		if cur != nil && cur.key == k {
			now = cur
			if cur, err = source.next(); err != nil {
				return sum, err
			}
		}
		if prev != nil && prev.key == k {
			was = prev
			if prev, err = old.next(); err != nil {
				return sum, err
			}
		}
		var current *asset
		var before *blob
		if was != nil {
			current, before = was.asset, &was.blob
		}
		if err := m.settle(store, k, before, now, current, was == nil, others, sql.NullInt64{}); err != nil {
			return sum, fmt.Errorf("%s of %s in store %s: %w", k.path, k.repo, store, err)
		}
		if now != nil {
			sum.blobs++
			sum.bytes += now.size
		}
	}
	return sum, m.flush()
}

func (m *merger) assetOf(k key) (*asset, error) {
	var a asset
	err := m.statements["assetGet"].QueryRowContext(bg, k.repo, k.path).Scan(&a.store, &a.format, &a.component, &a.extension,
		&a.classifier, &a.lastModified, &a.lastDownloaded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// settle writes the row of store for k as now (nil: no row) where it was
// before (nil: no row), then the asset of k: the newest blob of the key over
// store and others, on a tie the store whose name sorts first. When unread
// is set, current was not read: the key had no row in store, so its asset
// can only be one of others. modified, when valid, is the last modified time
// of a write whose blob wins.
func (m *merger) settle(store string, k key, before, now *blob, current *asset, unread bool, others []string,
	modified sql.NullInt64) error {
	switch {
	case now != nil && before == nil:
		if err := m.queue(m.blobs, store, k.repo, k.path, now.size, now.sha1, now.contentType, now.created, now.uploader,
			now.address, now.md5, now.sha256, now.sha512); err != nil {
			return err
		}
	case now != nil && !now.digests() && sameBlob(before, now):
		now.md5, now.sha256, now.sha512 = before.md5, before.sha256, before.sha512
		fallthrough
	case now != nil:
		if !sameRow(before, now) {
			if err := m.exec("blobUpdate", now.size, now.sha1, now.contentType, now.created, now.uploader, now.address,
				now.md5, now.sha256, now.sha512, store, k.repo, k.path); err != nil {
				return err
			}
		}
	}
	changed := (before == nil) != (now == nil) || before != nil && now != nil && before.created != now.created
	winner := ""
	if current != nil && !changed && (current.store == store || slices.Contains(others, current.store)) {
		winner = current.store
	} else {
		var best int64
		if now != nil {
			winner, best = store, now.created
		}
		other := false
		for _, name := range others {
			if held, err := m.holdsRepo(name, k.repo); err != nil || !held {
				if err != nil {
					return err
				}
				continue
			}
			var created int64
			err := m.statements["probe"].QueryRowContext(bg, name, k.repo, k.path).Scan(&created)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			other = true
			if winner == "" || created > best || created == best && name < winner {
				winner, best = name, created
			}
		}
		if unread && other {
			var err error
			if current, err = m.assetOf(k); err != nil {
				return err
			}
		}
	}
	if winner == "" {
		if current != nil {
			if err := m.exec("assetDelete", k.repo, k.path); err != nil {
				return err
			}
			if err := m.orphan(current.component); err != nil {
				return err
			}
		}
	} else if err := m.place(store, k, winner, current, changed, modified); err != nil {
		return err
	}
	if now == nil && before != nil {
		return m.exec("blobDelete", store, k.repo, k.path)
	}
	return nil
}

// holdsRepo tells whether store has a row of repo. The stores of others
// keep their rows during a merge, so the answer stays true for it.
func (m *merger) holdsRepo(store, repo string) (bool, error) {
	if held, ok := m.holds[[2]string{store, repo}]; ok {
		return held, nil
	}
	var found int
	if err := m.statements["repoAny"].QueryRowContext(bg, store, repo).Scan(&found); err != nil {
		return false, err
	}
	m.holds[[2]string{store, repo}] = found == 1
	return found == 1, nil
}

// place writes the asset of k won by the blob of winner. The last modified
// time stays while the same blob wins; the last download time stays.
func (m *merger) place(store string, k key, winner string, current *asset, changed bool, modified sql.NullInt64) error {
	want := asset{store: winner, format: m.formats(k.repo)}
	if c, ok := coords.Of(want.format, k.path); ok {
		id, err := m.component(k.repo, want.format, c)
		if err != nil {
			return err
		}
		want.component = sql.NullInt64{Int64: id, Valid: true}
		want.extension = sql.NullString{String: c.Extension, Valid: c.Extension != ""}
		want.classifier = sql.NullString{String: c.Classifier, Valid: c.Classifier != ""}
	}
	if current != nil {
		want.lastDownloaded = current.lastDownloaded
		if current.store == winner && !(winner == store && changed) {
			want.lastModified = current.lastModified
		}
	}
	if winner == store && modified.Valid {
		want.lastModified = modified
	}
	if current == nil {
		return m.queue(m.assets, k.repo, k.path, want.store, want.format, want.component, want.extension, want.classifier,
			want.lastModified, want.lastDownloaded)
	}
	if *current == want {
		return nil
	}
	if err := m.exec("assetUpdate", want.store, want.format, want.component, want.extension, want.classifier, want.lastModified,
		want.lastDownloaded, k.repo, k.path); err != nil {
		return err
	}
	if current.component != want.component {
		return m.orphan(current.component)
	}
	return nil
}

// component is the id of the component row of the coordinates; a new row
// waits in its batch with the next free id.
func (m *merger) component(repo, format string, c coords.Coordinates) (int64, error) {
	ck := componentKey{repo, format, c.Group, c.Name, c.Version}
	if id, ok := m.components[ck]; ok {
		return id, nil
	}
	fresh, seen := m.fresh[repo]
	if !seen {
		var found int
		if err := m.statements["compAny"].QueryRowContext(bg, repo).Scan(&found); err != nil {
			return 0, err
		}
		fresh = found == 0
		m.fresh[repo] = fresh
	}
	var id int64
	if !fresh {
		err := m.statements["compGet"].QueryRowContext(bg, ck.repo, ck.format, ck.namespace, ck.name, ck.version).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	if id == 0 {
		id = m.nextID
		m.nextID++
		prerelease := 0
		if c.Prerelease {
			prerelease = 1
		}
		if err := m.queue(m.comps, id, ck.repo, ck.format, ck.namespace, ck.name, ck.version, prerelease); err != nil {
			return 0, err
		}
	}
	if len(m.components) >= componentCache {
		// Only a cache: the keys of one component lie side by side in the
		// order of paths.
		if err := m.forget(); err != nil {
			return 0, err
		}
	}
	m.components[ck] = id
	return id, nil
}

// forget empties the cache of components. From then on no repository is
// fresh and every component is looked up, after the waiting ones are
// written.
func (m *merger) forget() error {
	clear(m.components)
	for name := range m.fresh {
		m.fresh[name] = false
	}
	return m.comps.flush(m.tx)
}

// orphan notes a component that lost an asset; sweep removes it when no
// asset names it any more. Many of them are swept at once.
func (m *merger) orphan(id sql.NullInt64) error {
	if !id.Valid {
		return nil
	}
	m.orphans[id.Int64] = true
	if len(m.orphans) < componentCache {
		return nil
	}
	return m.sweep()
}

// sweep removes the components that lost their last asset, and forgets
// the cache, which may name one of them.
func (m *merger) sweep() error {
	if err := m.flush(); err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(m.orphans)) {
		if err := m.exec("compOrphan", id, id); err != nil {
			return err
		}
	}
	clear(m.orphans)
	return m.forget()
}
