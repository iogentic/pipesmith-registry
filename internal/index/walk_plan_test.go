package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The walk below a folder reads the folder only: each part of its statement
// is a search on the primary key (repo, parent, name) with parent bound,
// never a scan of the whole repository, and the order needs no sort of its
// own. EXPLAIN QUERY PLAN of SQLite names how it reads each table.
func TestWalkPrefixSearchesTheFolderOnly(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	rows := func(yield func(stores.Entry) error) error {
		for _, p := range []string{"a/one", "a/b/two", "z/other"} {
			if err := yield(stores.Entry{Repo: "demo", Path: p, Size: 1, CreatedMS: 1, Location: p}); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := ix.ReplaceStore("s1", rows); err != nil {
		t.Fatal(err)
	}
	for _, ending := range []string{"", ".tgz"} {
		var plan []string
		err := ix.read(func(tx *sql.Tx) error {
			list, err := storeList(tx)
			if err != nil {
				return err
			}
			query, args := walkQuery(filesTable(list[0].id), "demo", "a", ending)
			result, err := tx.QueryContext(bg, "EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				return err
			}
			defer result.Close()
			for result.Next() {
				var id, parent, unused int
				var detail string
				if err := result.Scan(&id, &parent, &unused, &detail); err != nil {
					return err
				}
				plan = append(plan, detail)
			}
			return result.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(plan, "\n")
		t.Logf("ending %q:\n%s", ending, text)
		searches := 0
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN") {
				t.Errorf("ending %q: the walk scans: %q", ending, step)
			}
			if strings.HasPrefix(step, "SEARCH") {
				if !strings.Contains(step, "parent") {
					t.Errorf("ending %q: a search without a bound on parent: %q", ending, step)
				}
				searches++
			}
			if strings.Contains(step, "TEMP B-TREE") {
				t.Errorf("ending %q: the walk sorts on its own: %q", ending, step)
			}
		}
		if searches == 0 {
			t.Errorf("ending %q: no search in the plan:\n%s", ending, text)
		}
	}
}

// orStatement is the statement of the walk below a folder as it stood at
// 5f79545, kept to measure the walk against it.
func orStatement(table string) string {
	return "SELECT parent, name, location, size, sha1, content_type, created_ms FROM " + table +
		" WHERE repo = ? AND (parent = ? OR (parent >= ? AND parent < ?)) ORDER BY parent, name"
}

// The time of the walk statement below a folder at 1,000,000 rows of one
// repository, against the statement of 5f79545, run in pairs (old, new,
// old, new) and the best of three of each logged. It runs only with
// PSR_WALK_MEASURE=1, since filling the index takes a minute.
func TestWalkPrefixMeasure(t *testing.T) {
	if os.Getenv("PSR_WALK_MEASURE") != "1" {
		t.Skip("set PSR_WALK_MEASURE=1 to measure")
	}
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	rows := func(yield func(stores.Entry) error) error {
		for _, p := range []string{"aaa/f", "zzz/f"} {
			if err := yield(stores.Entry{Repo: "demo", Path: p, Size: 1, CreatedMS: 1, Location: p}); err != nil {
				return err
			}
		}
		for d := 0; d < 10000; d++ {
			for f := 0; f < 100; f++ {
				p := fmt.Sprintf("m/%05d/f%03d", d, f)
				if err := yield(stores.Entry{Repo: "demo", Path: p, Size: 1, CreatedMS: 1, Location: p}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	written, err := ix.ReplaceStore("s1", rows)
	if err != nil {
		t.Fatal(err)
	}
	count := func(query string, args []any) (int, time.Duration) {
		started := time.Now()
		n := 0
		err := ix.read(func(tx *sql.Tx) error {
			result, err := tx.QueryContext(bg, query, args...)
			if err != nil {
				return err
			}
			defer result.Close()
			for result.Next() {
				n++
			}
			return result.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		return n, time.Since(started)
	}
	var table string
	if err := ix.read(func(tx *sql.Tx) error {
		list, err := storeList(tx)
		table = filesTable(list[0].id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"aaa", "m/00000", "m/05000", "zzz", "missing-at-the-end-zzzz"} {
		bestOld, bestNew := time.Hour, time.Hour
		var nOld, nNew int
		for i := 0; i < 3; i++ {
			n, d := count(orStatement(table), []any{"demo", folder, folder + "/", folder + "0"})
			nOld, bestOld = n, min(bestOld, d)
			query, args := walkQuery(table, "demo", folder, "")
			n, d = count(query, args)
			nNew, bestNew = n, min(bestNew, d)
		}
		t.Logf("rows %d, folder %q: statement of 5f79545 %d rows, best of 3 %v; walk now %d rows, best of 3 %v", written, folder, nOld, bestOld, nNew, bestNew)
		if nOld != nNew {
			t.Errorf("folder %q: %d rows now, %d before", folder, nNew, nOld)
		}
	}
}
