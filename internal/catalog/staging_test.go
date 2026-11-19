package catalog

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// kept is what the index keeps of the entries of one store: per path the
// entry it took first, replaced by a later one only when that is strictly
// newer, in the order of the keys.
func kept(entries []stores.Entry) []stores.Entry {
	byKey := map[key]stores.Entry{}
	for _, e := range entries {
		if !addressable(e.Path) {
			continue
		}
		k := key{e.Repo, e.Path}
		if old, ok := byKey[k]; !ok || e.CreatedMS > old.CreatedMS {
			byKey[k] = e
		}
	}
	out := make([]stores.Entry, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b stores.Entry) int {
		return strings.Compare(a.Repo+"\x00"+a.Path, b.Repo+"\x00"+b.Path)
	})
	return out
}

func textOf(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func same(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// The merge of the runs gives what the index keeps, with many runs on the
// disk: entries of one path in several runs, ties of the creation time,
// values that are absent, and paths the index skips.
func TestStagingMergesItsRuns(t *testing.T) {
	old := runEntries
	runEntries = 97
	t.Cleanup(func() { runEntries = old })
	r := rand.New(rand.NewPCG(20261118, 5))
	var entries []stores.Entry
	for i := range 10_000 {
		repo := fmt.Sprintf("demo-%d", r.IntN(3))
		path := fmt.Sprintf("docs/%03d.txt", r.IntN(1500))
		switch r.IntN(200) {
		case 0:
			path = ""
		case 1:
			path = "/" + path
		}
		e := stores.Entry{Repo: repo, Path: path, Size: int64(i), CreatedMS: int64(r.IntN(4)), Location: fmt.Sprint(i)}
		if r.IntN(3) > 0 {
			sha1 := fmt.Sprintf("%040d", i)
			e.SHA1 = &sha1
		}
		if r.IntN(2) == 0 {
			user := fmt.Sprintf("user-%d", i)
			e.Uploader = &user
		}
		entries = append(entries, e)
	}
	dir := t.TempDir()
	s := &staging{dir: dir}
	for _, e := range entries {
		if addressable(e.Path) {
			if err := s.add(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(s.runs) < 50 {
		t.Fatalf("%d runs on the disk, want many", len(s.runs))
	}
	m, err := s.source()
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	want := kept(entries)
	for i, w := range want {
		got, err := m.next()
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("the merge ended after %d of %d keys", i, len(want))
		}
		if got.repo != w.Repo || got.path != w.Path || got.size != w.Size || got.created != w.CreatedMS ||
			!same(textOf(got.sha1), w.SHA1) || !same(textOf(got.uploader), w.Uploader) || got.contentType.Valid || got.address.Valid {
			t.Fatalf("key %d: %+v, want %+v", i, *got, w)
		}
	}
	if extra, err := m.next(); err != nil || extra != nil {
		t.Fatalf("after the last key: %+v %v", extra, err)
	}
}

// A fill with its runs on the disk writes the rows the index keeps and
// leaves no run behind; Discard removes the runs of a fill it ends.
func TestFillWithRunsOnTheDisk(t *testing.T) {
	old := runEntries
	runEntries = 5
	t.Cleanup(func() { runEntries = old })
	dir := filepath.Join(t.TempDir(), "data")
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var entries []stores.Entry
	for i := range 60 {
		entries = append(entries, stores.Entry{Repo: "demo-raw", Path: fmt.Sprintf("docs/%02d.txt", (i*7)%23), Size: int64(i),
			CreatedMS: int64(i % 5)})
	}
	f, err := c.Begin("demo-store")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := f.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := filepath.Glob(filepath.Join(f.stage.dir, "run-*"))
	if err != nil || len(runs) != 12 {
		t.Fatalf("%d runs on the disk: %v", len(runs), err)
	}
	if err := f.Commit(func(string) string { return "raw" }, []string{"demo-store"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.stage.dir); !os.IsNotExist(err) {
		t.Fatalf("the staging folder is still there: %v", err)
	}
	rows, err := c.readers.Query("SELECT repo, path, size, created_ms FROM blobs ORDER BY repo, path")
	if err != nil {
		t.Fatal(err)
	}
	var got []stores.Entry
	for rows.Next() {
		var e stores.Entry
		if err := rows.Scan(&e.Repo, &e.Path, &e.Size, &e.CreatedMS); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if want := kept(entries); !slices.Equal(got, want) {
		t.Fatalf("rows %v\nwant %v", got, want)
	}

	discarded, err := c.Begin("demo-store")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := discarded.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := discarded.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(discarded.stage.dir); !os.IsNotExist(err) {
		t.Fatalf("Discard left the staging folder: %v", err)
	}
	if err := discarded.Add(entries[0]); err != ErrFillDone {
		t.Fatalf("Add after Discard: %v", err)
	}
}
