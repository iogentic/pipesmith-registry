package catalog

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func ms(v int64) *int64 { return &v }

func state(t *testing.T, c *Catalog, repo, path string) ProxyState {
	t.Helper()
	s, err := c.ProxyState(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The table has the columns of section State of the phase 5 plan, with
// repository and path as its primary key.
func TestProxyStateTableColumns(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	rows, err := c.readers.Query("SELECT name, type, pk FROM pragma_table_info('proxy_state') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var name, kind string
		var pk int
		if err := rows.Scan(&name, &kind, &pk); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %s %d", name, kind, pk))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"repo TEXT 1", "path TEXT 2", "checked_ms INTEGER 0", "etag TEXT 0", "last_modified TEXT 0",
		"not_found_until_ms INTEGER 0"}
	if !slices.Equal(got, want) {
		t.Fatalf("columns %v, want %v", got, want)
	}
}

// One row is written and read back whole, a second write of the path
// replaces it, and a path without a row has none.
func TestProxyStateRow(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	if s := state(t, c, "demo-proxy", "com/example/demo-lib/1.0/demo-lib-1.0.jar"); s.Row != nil || s.ResetMS != nil {
		t.Fatalf("a path without a row: %+v", s)
	}
	row := ProxyRow{Repo: "demo-proxy", Path: "com/example/demo-lib/1.0/demo-lib-1.0.jar", CheckedMS: ms(1_700_000_000_000),
		ETag: `"v1"`, LastModified: "Tue, 14 Nov 2023 22:13:20 GMT"}
	if err := c.PutProxyRow(row); err != nil {
		t.Fatal(err)
	}
	if s := state(t, c, row.Repo, row.Path); s.Row == nil || !reflect.DeepEqual(*s.Row, row) || s.ResetMS != nil {
		t.Fatalf("%+v", s)
	}
	// a 404 of the remote: no validators, a not found mark
	missing := ProxyRow{Repo: "demo-proxy", Path: "com/example/gone/1.0/gone-1.0.pom", CheckedMS: ms(1_700_000_000_500),
		NotFoundUntilMS: ms(1_700_086_400_500)}
	if err := c.PutProxyRow(missing); err != nil {
		t.Fatal(err)
	}
	changed := row
	changed.CheckedMS, changed.ETag, changed.LastModified = ms(1_700_000_060_000), `"v2"`, ""
	if err := c.PutProxyRow(changed); err != nil {
		t.Fatal(err)
	}
	if s := state(t, c, row.Repo, row.Path); !reflect.DeepEqual(*s.Row, changed) {
		t.Fatalf("after the second write %+v, want %+v", *s.Row, changed)
	}
	if s := state(t, c, missing.Repo, missing.Path); !reflect.DeepEqual(*s.Row, missing) {
		t.Fatalf("the other path %+v", *s.Row)
	}
	// a stale row: no time of a check
	stale := ProxyRow{Repo: "demo-proxy", Path: "stale.txt"}
	if err := c.PutProxyRow(stale); err != nil {
		t.Fatal(err)
	}
	if s := state(t, c, stale.Repo, stale.Path); s.Row == nil || s.Row.CheckedMS != nil || s.Row.NotFoundUntilMS != nil {
		t.Fatalf("%+v", s.Row)
	}
	for _, bad := range []ProxyRow{{Repo: "demo-proxy"}, {Path: "a.txt"}} {
		if err := c.PutProxyRow(bad); err == nil {
			t.Errorf("%+v was written", bad)
		}
	}
	if _, err := c.ProxyState("demo-proxy", ""); err == nil {
		t.Error("the empty path was read as a path")
	}
}

// The bulk reset of a repository (decision 5.17) clears its not found marks
// and makes every path stale: rows lose the time of their check, a row that
// held only a not found mark goes, and the time of the reset counts for the
// paths without a row. Other repositories keep their rows.
func TestProxyStateReset(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	kept := ProxyRow{Repo: "demo-proxy", Path: "a.jar", CheckedMS: ms(100), ETag: `"a"`, LastModified: "Mon, 13 Nov 2023 10:00:00 GMT",
		NotFoundUntilMS: nil}
	marked := ProxyRow{Repo: "demo-proxy", Path: "gone.pom", CheckedMS: ms(110), NotFoundUntilMS: ms(9_000)}
	both := ProxyRow{Repo: "demo-proxy", Path: "b.jar", CheckedMS: ms(120), ETag: `"b"`, NotFoundUntilMS: ms(9_500)}
	other := ProxyRow{Repo: "demo-other", Path: "a.jar", CheckedMS: ms(130), ETag: `"o"`, NotFoundUntilMS: ms(9_900)}
	for _, r := range []ProxyRow{kept, marked, both, other} {
		if err := c.PutProxyRow(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.ResetProxyState("demo-proxy", 5_000); err != nil {
		t.Fatal(err)
	}
	s := state(t, c, "demo-proxy", "a.jar")
	if want := (ProxyRow{Repo: "demo-proxy", Path: "a.jar", ETag: `"a"`, LastModified: kept.LastModified}); s.Row == nil ||
		!reflect.DeepEqual(*s.Row, want) || s.ResetMS == nil || *s.ResetMS != 5_000 {
		t.Fatalf("a row with validators after the reset: %+v %v", s.Row, s.ResetMS)
	}
	if s := state(t, c, "demo-proxy", "gone.pom"); s.Row != nil || s.ResetMS == nil || *s.ResetMS != 5_000 {
		t.Fatalf("a not found mark after the reset: %+v", s)
	}
	if s := state(t, c, "demo-proxy", "b.jar"); s.Row == nil || s.Row.NotFoundUntilMS != nil || s.Row.CheckedMS != nil || s.Row.ETag != `"b"` {
		t.Fatalf("a row with a validator and a mark after the reset: %+v", s.Row)
	}
	if s := state(t, c, "demo-proxy", "never/fetched.jar"); s.Row != nil || s.ResetMS == nil || *s.ResetMS != 5_000 {
		t.Fatalf("a path without a row after the reset: %+v", s)
	}
	if s := state(t, c, "demo-other", "a.jar"); !reflect.DeepEqual(*s.Row, other) || s.ResetMS != nil {
		t.Fatalf("another repository after the reset: %+v %v", s.Row, s.ResetMS)
	}
	// a check after the reset counts again, and a second reset moves the time
	fresh := kept
	fresh.CheckedMS = ms(6_000)
	if err := c.PutProxyRow(fresh); err != nil {
		t.Fatal(err)
	}
	if s := state(t, c, "demo-proxy", "a.jar"); *s.Row.CheckedMS != 6_000 || *s.ResetMS != 5_000 {
		t.Fatalf("%+v %v", s.Row, s.ResetMS)
	}
	if err := c.ResetProxyState("demo-proxy", 7_000); err != nil {
		t.Fatal(err)
	}
	if s := state(t, c, "demo-proxy", "a.jar"); s.Row.CheckedMS != nil || *s.ResetMS != 7_000 {
		t.Fatalf("after the second reset %+v %v", s.Row, s.ResetMS)
	}
	if err := c.ResetProxyState("", 1); err == nil {
		t.Fatal("a reset without a repository")
	}
}

// Removal takes every row of a repository, its reset time among them, and
// leaves the rows of the others.
func TestProxyStateDrop(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	for _, r := range []ProxyRow{{Repo: "demo-proxy", Path: "a", CheckedMS: ms(1)}, {Repo: "demo-proxy", Path: "b", ETag: "x"},
		{Repo: "demo-other", Path: "a", CheckedMS: ms(2)}} {
		if err := c.PutProxyRow(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.ResetProxyState("demo-proxy", 3); err != nil {
		t.Fatal(err)
	}
	if err := c.DropProxyState("demo-proxy"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a", "b"} {
		if s := state(t, c, "demo-proxy", path); s.Row != nil || s.ResetMS != nil {
			t.Fatalf("%s after the removal: %+v", path, s)
		}
	}
	if s := state(t, c, "demo-other", "a"); s.Row == nil || *s.Row.CheckedMS != 2 {
		t.Fatalf("another repository after the removal: %+v", s)
	}
	if n := scalar[int](t, c.writer, "SELECT count(*) FROM proxy_state WHERE repo = 'demo-proxy'"); n != 0 {
		t.Fatalf("%d rows of the removed repository", n)
	}
}

// parallelTime is how long 8 writers and 8 readers share the file.
const parallelTime = 5 * time.Second

// 8 writers and 8 readers for 5 s under -race: every write succeeds, every
// reader sees the time of the check of a path grow and never shrink, and at
// the end each path holds the last write of its writer.
func TestProxyStateEightWritersEightReaders(t *testing.T) {
	c := mustOpen(t, filepath.Join(t.TempDir(), "data"))
	const writers, readers, paths = 8, 8, 16
	deadline := time.Now().Add(parallelTime)
	last := make([][]int64, writers)
	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)
	writes := make([]int, writers)
	for w := range writers {
		last[w] = make([]int64, paths)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(1); time.Now().Before(deadline); n++ {
				p := int(n) % paths
				row := ProxyRow{Repo: fmt.Sprintf("demo-proxy-%d", w), Path: fmt.Sprintf("p/%d.jar", p), CheckedMS: ms(n),
					ETag: fmt.Sprintf(`"%d"`, n)}
				if err := c.PutProxyRow(row); err != nil {
					errs <- err
					return
				}
				last[w][p] = n
				writes[w]++
			}
		}()
	}
	reads := make([]int, readers)
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			random := rand.New(rand.NewPCG(uint64(r), 7))
			seen := map[[2]int]int64{}
			for time.Now().Before(deadline) {
				w, p := random.IntN(writers), random.IntN(paths)
				s, err := c.ProxyState(fmt.Sprintf("demo-proxy-%d", w), fmt.Sprintf("p/%d.jar", p))
				if err != nil {
					errs <- err
					return
				}
				reads[r]++
				if s.Row == nil {
					continue
				}
				checked := *s.Row.CheckedMS
				if checked < seen[[2]int{w, p}] {
					errs <- fmt.Errorf("writer %d path %d went back from %d to %d", w, p, seen[[2]int{w, p}], checked)
					return
				}
				if s.Row.ETag != fmt.Sprintf(`"%d"`, checked) {
					errs <- fmt.Errorf("writer %d path %d: a row of two writes: %+v", w, p, *s.Row)
					return
				}
				seen[[2]int{w, p}] = checked
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for w := range writers {
		if writes[w] == 0 {
			t.Fatalf("writer %d wrote nothing", w)
		}
		for p := range paths {
			if last[w][p] == 0 {
				continue
			}
			s := state(t, c, fmt.Sprintf("demo-proxy-%d", w), fmt.Sprintf("p/%d.jar", p))
			if s.Row == nil || *s.Row.CheckedMS != last[w][p] {
				t.Fatalf("writer %d path %d holds %+v, want %d", w, p, s.Row, last[w][p])
			}
		}
	}
	for r := range readers {
		if reads[r] == 0 {
			t.Fatalf("reader %d read nothing", r)
		}
	}
	t.Logf("writes per writer %v, reads per reader %v", writes, reads)
}
