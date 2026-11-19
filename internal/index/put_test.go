package index_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// tablesOf is the dump of every table but meta, whose generation counts
// the writes.
func tablesOf(t *testing.T, path string) any {
	t.Helper()
	dump := dumpFile(t, path).(map[string]any)
	var tables []any
	for _, table := range dump["tables"].([]any) {
		if table.([]any)[0] != "meta" {
			tables = append(tables, table)
		}
	}
	dump["tables"] = tables
	return dump
}

func schemaRow(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var schema int64
	if err := db.QueryRow("SELECT value FROM meta WHERE key = 'schema'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// After random puts and deletes through the blob writer, with the delete
// mark on every replaced or deleted blob, the index equals the index a
// scan of the store builds, table by table.
func TestPutAndDeleteEqualAScan(t *testing.T) {
	root := filepath.Join(t.TempDir(), "local-write")
	clock := time.Date(2026, 11, 18, 10, 15, 30, 0, time.UTC)
	rng := rand.New(rand.NewPCG(4, 1))
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(time.Duration(rng.IntN(3)) * time.Millisecond)
		return clock
	}
	if err := blobwrite.Create(root, now()); err != nil {
		t.Fatal(err)
	}
	s, err := stores.NewWriteStore("local-write", root)
	if err != nil {
		t.Fatal(err)
	}
	w, err := blobwrite.Open(s, blobwrite.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ix, path := openIndex(t)
	repos := []string{"demo-raw", "demo-releases", "demo-npm"}
	paths := []string{"a.txt", "b.txt", "docs/a.txt", "docs/b.txt", "docs/deep/c.txt", "docs/deep/deeper/d.txt",
		"x/y/z/e.txt", "x/y/f.txt", "x/g.txt", "h/i/j/k/l.txt"}
	puts, replaced, deletes := 0, 0, 0
	for range 400 {
		repo, p := repos[rng.IntN(len(repos))], paths[rng.IntN(len(paths))]
		if rng.IntN(10) < 7 {
			body := strings.Repeat("x", rng.IntN(50))
			written, err := w.Write(strings.NewReader(body), stores.Attributes{Repo: repo, Path: p, ContentType: "text/plain", CreatedBy: "demo-user"})
			if err != nil {
				t.Fatal(err)
			}
			old, err := ix.Put("local-write", written.Entry)
			if err != nil {
				t.Fatal(err)
			}
			puts++
			if old != nil {
				replaced++
				if err := w.MarkDeleted(old.Location, "replaced"); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		row, err := ix.Lookup(repo, p)
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			continue
		}
		if err := w.MarkDeleted(row.Location, "deleted"); err != nil {
			t.Fatal(err)
		}
		removed, err := ix.Delete(repo, p, "local-write")
		if err != nil || removed == nil || removed.Location != row.Location {
			t.Fatalf("Delete: %+v %v, want the row %+v", removed, err, row)
		}
		deletes++
	}
	if puts < 200 || replaced < 50 || deletes < 30 {
		t.Fatalf("%d puts, %d replaced, %d deletes: the run did not mix enough", puts, replaced, deletes)
	}
	scanned, scannedPath := openIndex(t)
	if _, err := scanned.ReplaceStore("local-write", func(yield func(stores.Entry) error) error {
		return s.Entries(nil, yield)
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := tablesOf(t, path), tablesOf(t, scannedPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("index after puts\n%v\nindex of a scan\n%v", got, want)
	}
	if schemaRow(t, path) != 1 {
		t.Fatal("the schema row changed")
	}
}

func put(t *testing.T, ix *index.Index, store string, e stores.Entry) *index.Asset {
	t.Helper()
	old, err := ix.Put(store, e)
	if err != nil {
		t.Fatal(err)
	}
	return old
}

// Put replaces the row of its store whatever the creation time, and
// returns it; a row of another store stays.
func TestPutReplacesTheRowOfItsStore(t *testing.T) {
	ix, _ := openIndex(t)
	first := sized("docs/a.txt", 3, 100, "content/one.bytes")
	if old := put(t, ix, "local-write", first); old != nil {
		t.Fatalf("a new path replaced %+v", old)
	}
	second := sized("docs/a.txt", 5, 100, "content/two.bytes")
	old := put(t, ix, "local-write", second)
	if old == nil || old.Location != first.Location || old.Size != 3 || old.Store != "local-write" {
		t.Fatalf("replaced %+v", old)
	}
	if got := mustLookup(t, ix, "demo-releases", "docs/a.txt"); got.Location != second.Location {
		t.Fatalf("lookup %+v after a put of the same millisecond", got)
	}
	older := sized("docs/a.txt", 7, 50, "content/three.bytes")
	put(t, ix, "local-write", older)
	if got := mustLookup(t, ix, "demo-releases", "docs/a.txt"); got.Location != older.Location {
		t.Fatalf("lookup %+v after a put with an older time", got)
	}
	if stats, err := ix.RepoStats(); err != nil || !reflect.DeepEqual(stats, []index.RepoStat{{Repo: "demo-releases", Files: 1, Bytes: 7, NewestMS: 50}}) {
		t.Fatalf("counts after a put with an older time: %+v %v", stats, err)
	}
	mustReplace(t, ix, "old-store", slice(sized("docs/a.txt", 9, 75, "old/a")))
	if got := mustLookup(t, ix, "demo-releases", "docs/a.txt"); got.Store != "old-store" {
		t.Fatalf("lookup %+v: the newer row of the other store wins", got)
	}
	put(t, ix, "local-write", sized("docs/a.txt", 11, 76, "content/four.bytes"))
	if got := mustLookup(t, ix, "demo-releases", "docs/a.txt"); got.Location != "content/four.bytes" {
		t.Fatalf("lookup %+v", got)
	}
	stats, err := ix.StoreStats()
	if err != nil {
		t.Fatal(err)
	}
	if want := []index.StoreStat{{Store: "local-write", Files: 1, Bytes: 11}, {Store: "old-store", Files: 1, Bytes: 9}}; !reflect.DeepEqual(stats, want) {
		t.Fatalf("%+v", stats)
	}
}

// Directories and counts follow every put and delete; a delete of a path
// without a row changes nothing and returns nil.
func TestPutAndDeleteKeepFoldersAndCounts(t *testing.T) {
	ix, _ := openIndex(t)
	put(t, ix, "w", sized("a/b/c.txt", 4, 10, "l1"))
	put(t, ix, "w", sized("a/d.txt", 6, 30, "l2"))
	children, err := ix.Children("demo-releases", "a", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || children[0].Name != "b" || !children[0].IsDir || children[1].Name != "d.txt" {
		t.Fatalf("%+v", children)
	}
	stats, err := ix.RepoStats()
	if err != nil {
		t.Fatal(err)
	}
	if want := []index.RepoStat{{Repo: "demo-releases", Files: 2, Bytes: 10, NewestMS: 30}}; !reflect.DeepEqual(stats, want) {
		t.Fatalf("%+v", stats)
	}
	removed, err := ix.Delete("demo-releases", "a/d.txt", "w")
	if err != nil || removed == nil || removed.Size != 6 || removed.CreatedMS != 30 {
		t.Fatalf("%+v %v", removed, err)
	}
	if stats, _ = ix.RepoStats(); !reflect.DeepEqual(stats, []index.RepoStat{{Repo: "demo-releases", Files: 1, Bytes: 4, NewestMS: 10}}) {
		t.Fatalf("%+v", stats)
	}
	if removed, err := ix.Delete("demo-releases", "a/d.txt", "w"); removed != nil || err != nil {
		t.Fatalf("second delete: %+v %v", removed, err)
	}
	if removed, err := ix.Delete("demo-releases", "a/b/c.txt", "w"); removed == nil || err != nil {
		t.Fatalf("%+v %v", removed, err)
	}
	if isDir, _ := ix.IsDir("demo-releases", "a"); isDir {
		t.Fatal("an empty folder is left")
	}
	if stats, _ = ix.RepoStats(); len(stats) != 0 {
		t.Fatalf("%+v", stats)
	}
	for _, bad := range []string{"", "/a.txt", "a/"} {
		if _, err := ix.Put("w", sized(bad, 1, 1, "l")); err == nil {
			t.Errorf("Put of %q", bad)
		}
	}
}

// An index file written by the port takes puts and deletes and keeps its
// schema row.
func TestPortIndexFileTakesPuts(t *testing.T) {
	data, err := os.ReadFile("../../testdata/vectors/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "index.sqlite")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	before, err := ix.StoreStats()
	if err != nil || len(before) == 0 {
		t.Fatalf("%+v %v", before, err)
	}
	store := before[0].Store
	e := sized("phase4/new/file.txt", 12, 1_800_000_000_000, "content/new.bytes")
	if old := put(t, ix, store, e); old != nil {
		t.Fatalf("%+v", old)
	}
	if got := mustLookup(t, ix, "demo-releases", "phase4/new/file.txt"); got == nil || got.Store != store || got.Size != 12 {
		t.Fatalf("%+v", got)
	}
	if _, err := ix.Delete("demo-releases", "phase4/new/file.txt", store); err != nil {
		t.Fatal(err)
	}
	after, err := ix.StoreStats()
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("%+v %v, want %+v", after, err, before)
	}
	if schemaRow(t, path) != 1 {
		t.Fatal("the schema row changed")
	}
}

// snapshotCheck reads every row of one store in one read transaction and
// reports a file without its folder, a folder without content or counts
// that differ from the rows.
func snapshotCheck(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow("SELECT id FROM stores WHERE name = 'w'").Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	type key struct{ repo, parent, name string }
	dirs := map[key]bool{}
	held := map[[2]string]bool{} // (repo, folder) with content
	rows, err := tx.Query(fmt.Sprintf("SELECT repo, parent, name FROM dirs_%d", id))
	if err != nil {
		return err
	}
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.repo, &k.parent, &k.name); err != nil {
			return err
		}
		dirs[k] = true
		held[[2]string{k.repo, k.parent}] = true
	}
	rows.Close()
	counts := map[string][2]int64{}
	rows, err = tx.Query(fmt.Sprintf("SELECT repo, parent, size FROM files_%d", id))
	if err != nil {
		return err
	}
	for rows.Next() {
		var repo, parent string
		var size int64
		if err := rows.Scan(&repo, &parent, &size); err != nil {
			return err
		}
		held[[2]string{repo, parent}] = true
		c := counts[repo]
		counts[repo] = [2]int64{c[0] + 1, c[1] + size}
		if parent != "" {
			up, leaf := "", parent
			if i := strings.LastIndex(parent, "/"); i >= 0 {
				up, leaf = parent[:i], parent[i+1:]
			}
			if !dirs[key{repo, up, leaf}] {
				return fmt.Errorf("file in %s/%s without its folder", repo, parent)
			}
		}
	}
	rows.Close()
	for k := range dirs {
		folder := k.name
		if k.parent != "" {
			folder = k.parent + "/" + k.name
		}
		if !held[[2]string{k.repo, folder}] {
			return fmt.Errorf("empty folder %s/%s", k.repo, folder)
		}
	}
	rows, err = tx.Query("SELECT repo, files, bytes FROM repo_counts WHERE store_id = ?", id)
	if err != nil {
		return err
	}
	seen := 0
	for rows.Next() {
		var repo string
		var files, bytes int64
		if err := rows.Scan(&repo, &files, &bytes); err != nil {
			return err
		}
		seen++
		if counts[repo] != [2]int64{files, bytes} {
			return fmt.Errorf("counts of %s: %d files, %d bytes; the rows say %v", repo, files, bytes, counts[repo])
		}
	}
	rows.Close()
	if seen != len(counts) {
		return fmt.Errorf("%d count rows for %d repositories", seen, len(counts))
	}
	return nil
}

// 8 writers and 8 readers: no reader sees a half written folder or counts
// that differ from the rows.
func TestPutWritersAndReaders(t *testing.T) {
	ix, path := openIndex(t)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)&_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stop atomic.Bool
	var writers, readers sync.WaitGroup
	errs := make(chan error, 32)
	checks := make([]int, 8)
	for n := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			rng := rand.New(rand.NewPCG(uint64(n), 7))
			for i := range 150 {
				p := fmt.Sprintf("d%d/e%d/f%d.txt", rng.IntN(3), rng.IntN(3), rng.IntN(4))
				var err error
				if rng.IntN(3) == 0 {
					_, err = ix.Delete("demo-releases", p, "w")
				} else {
					_, err = ix.Put("w", sized(p, int64(rng.IntN(100)), int64(i), fmt.Sprintf("l%d-%d", n, i)))
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for n := range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for !stop.Load() {
				var err error
				if n%2 == 0 {
					err = snapshotCheck(context.Background(), db)
				} else {
					_, err = ix.Children("demo-releases", "d1", 100, 0)
					if err == nil {
						_, err = ix.Lookup("demo-releases", "d1/e1/f1.txt")
					}
				}
				if err != nil {
					errs <- err
					return
				}
				checks[n]++
			}
		}()
	}
	writers.Wait()
	stop.Store(true)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for n, count := range checks {
		if count == 0 {
			t.Errorf("reader %d never ran", n)
		}
	}
	if err := snapshotCheck(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

// Rows gives the row of a path in every store, the winning row first.
func TestRowsOfAPath(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "b-store", slice(sized("a/x.txt", 1, 10, "loc-b")))
	mustReplace(t, ix, "a-store", slice(sized("a/x.txt", 2, 10, "loc-a")))
	put(t, ix, "c-store", sized("a/x.txt", 3, 20, "loc-c"))
	rows, err := ix.Rows("demo-releases", "a/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	var locations []string
	for _, r := range rows {
		locations = append(locations, r.Store+"="+r.Location)
	}
	if want := []string{"c-store=loc-c", "a-store=loc-a", "b-store=loc-b"}; !reflect.DeepEqual(locations, want) {
		t.Fatalf("%v, want %v", locations, want)
	}
	if winner := mustLookup(t, ix, "demo-releases", "a/x.txt"); !reflect.DeepEqual(*winner, rows[0]) {
		t.Fatalf("Lookup %+v, Rows %+v", winner, rows[0])
	}
	for _, missing := range []string{"a/y.txt", "", "/a/x.txt", "a/"} {
		if rows, err := ix.Rows("demo-releases", missing); err != nil || len(rows) != 0 {
			t.Fatalf("%q: %+v %v", missing, rows, err)
		}
	}
}
