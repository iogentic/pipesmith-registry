package index_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
)

func sized(path string, size, created int64, location string) stores.Entry {
	e := entry(path)
	e.Size, e.CreatedMS, e.Location = size, created, location
	return e
}

// TestReadersDuringReplace is test_readers_during_replace of the reference,
// with readers of Children beside the readers of Lookup. Old and new rows
// each have a directory of their own, which sorts before the files, so a
// page that mixes the two states shows.
func TestReadersDuringReplace(t *testing.T) {
	ix, _ := openIndex(t)
	var oldRows, newRows []stores.Entry
	for i := range 2_000 {
		oldRows = append(oldRows, sized(fmt.Sprintf("p/f%05d.txt", i), 1, 1, fmt.Sprintf("old/%d", i)))
	}
	oldRows = append(oldRows, sized("p/a-old/x.txt", 1, 1, "old/dir"))
	for i := range 20_000 {
		newRows = append(newRows, sized(fmt.Sprintf("p/f%05d.txt", i), 2, 2, fmt.Sprintf("new/%d", i)))
	}
	newRows = append(newRows, sized("p/a-new/x.txt", 2, 2, "new/dir"))
	mustReplace(t, ix, "a", slice(oldRows...))

	var stop atomic.Bool
	var ready, done sync.WaitGroup
	errs := make(chan error, 16)
	lookups := make([]int, 4)
	pages := make([]int, 2)
	ready.Add(len(lookups) + len(pages))
	for slot := range lookups {
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			ready.Wait()
			seenNew := false
			i := slot
			for !stop.Load() {
				i = (i + 7) % 2_000
				a, err := ix.Lookup("demo-releases", fmt.Sprintf("p/f%05d.txt", i))
				lookups[slot]++
				switch {
				case err != nil:
					errs <- err
					return
				case a == nil:
					errs <- fmt.Errorf("row %d missing during replace", i)
					return
				case a.Size == 1:
					if a.Location != fmt.Sprintf("old/%d", i) || a.CreatedMS != 1 || a.Store != "a" {
						errs <- fmt.Errorf("old row %d mixed: %+v", i, a)
						return
					}
					if seenNew {
						errs <- fmt.Errorf("old row %d seen after a new row", i)
						return
					}
				default:
					if a.Size != 2 || a.Location != fmt.Sprintf("new/%d", i) || a.CreatedMS != 2 {
						errs <- fmt.Errorf("new row %d mixed: %+v", i, a)
						return
					}
					seenNew = true
				}
			}
		}()
	}
	for slot := range pages {
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			ready.Wait()
			seenNew := false
			for !stop.Load() {
				page, err := ix.Children("demo-releases", "p", 3_000, 0)
				pages[slot]++
				if err != nil {
					errs <- err
					return
				}
				if err := samePageState(page, &seenNew); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	ready.Wait()
	written, err := ix.ReplaceStore("a", slice(newRows...))
	stop.Store(true)
	done.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if err != nil {
		t.Fatal(err)
	}
	if written != 20_001 {
		t.Fatalf("written %d, want 20001", written)
	}
	t.Logf("calls during the replace: lookups %v, pages %v", lookups, pages)
	for slot, n := range lookups {
		if n == 0 {
			t.Errorf("lookup reader %d made no call", slot)
		}
	}
	for slot, n := range pages {
		if n == 0 {
			t.Errorf("children reader %d made no call", slot)
		}
	}
	if a := mustLookup(t, ix, "demo-releases", "p/f19999.txt"); a == nil || a.Location != "new/19999" {
		t.Fatalf("after replace: %+v", a)
	}
}

// samePageState checks that a first page of p/ shows one state: the old
// rows (a-old and 2,000 files of size 1) or the new rows (a-new and the
// first 2,999 files of size 2), and not the old state after the new one.
func samePageState(page []index.Child, seenNew *bool) error {
	if len(page) == 0 {
		return fmt.Errorf("empty page")
	}
	var size int64
	var want int
	switch page[0].Name {
	case "a-old":
		size, want = 1, 2_001
		if *seenNew {
			return fmt.Errorf("old page seen after a new page")
		}
	case "a-new":
		size, want = 2, 3_000
		*seenNew = true
	default:
		return fmt.Errorf("first child %+v", page[0])
	}
	if len(page) != want || !page[0].IsDir {
		return fmt.Errorf("page of %s has %d rows, want %d", page[0].Name, len(page), want)
	}
	for n, c := range page[1:] {
		if c.IsDir || c.Name != fmt.Sprintf("f%05d.txt", n) || c.Size == nil || *c.Size != size || *c.CreatedMS != size {
			return fmt.Errorf("page of %s mixes in %+v", page[0].Name, c)
		}
	}
	return nil
}

// TestReadsNeverMixTwoStates switches a store between two states many times
// while readers check that every call answers from one state.
func TestReadsNeverMixTwoStates(t *testing.T) {
	ix, _ := openIndex(t)
	state := func(tag string, size int64) []stores.Entry {
		rows := []stores.Entry{sized("d/"+tag+"-dir/x.txt", size, size, tag)}
		for i := range 200 {
			rows = append(rows, sized(fmt.Sprintf("d/f%03d.txt", i), size, size, tag))
		}
		return rows
	}
	stateA, stateB := state("a", 1), state("b", 2)
	mustReplace(t, ix, "s", slice(stateA...))

	var stop atomic.Bool
	var done sync.WaitGroup
	errs := make(chan error, 16)
	for range 4 {
		done.Add(1)
		go func() {
			defer done.Done()
			for !stop.Load() {
				page, err := ix.Children("demo-releases", "d", index.DefaultLimit, 0)
				if err != nil {
					errs <- err
					return
				}
				if len(page) != 201 {
					errs <- fmt.Errorf("page of %d rows", len(page))
					return
				}
				var size int64 = 1
				if page[0].Name == "b-dir" {
					size = 2
				}
				for _, c := range page[1:] {
					if *c.Size != size {
						errs <- fmt.Errorf("page of %s mixes in %+v", page[0].Name, c)
						return
					}
				}
				stats, err := ix.RepoStats()
				if err != nil {
					errs <- err
					return
				}
				if len(stats) != 1 || stats[0].Files != 201 || stats[0].Bytes != 201*stats[0].NewestMS {
					errs <- fmt.Errorf("stats mixed: %+v", stats)
					return
				}
			}
		}()
	}
	for round := range 30 {
		rows := stateA
		if round%2 == 0 {
			rows = stateB
		}
		if _, err := ix.ReplaceStore("s", slice(rows...)); err != nil {
			t.Error(err)
			break
		}
	}
	stop.Store(true)
	done.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// slowEntries yields rows, then signals inside and waits for release.
func slowEntries(inside chan<- struct{}, release <-chan struct{}, rows ...stores.Entry) func(yield func(stores.Entry) error) error {
	return func(yield func(stores.Entry) error) error {
		for _, e := range rows {
			if err := yield(e); err != nil {
				return err
			}
		}
		close(inside)
		<-release
		return nil
	}
}

func TestDropDoesNotWaitForARunningReplace(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "a", slice(entry("keep.txt")))
	mustReplace(t, ix, "b", slice(entry("gone.txt")))
	inside, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	time.AfterFunc(2*time.Second, free) // a Drop that waits fails instead of hanging
	worker := make(chan error, 1)
	go func() {
		_, err := ix.ReplaceStore("a", slowEntries(inside, release, entry("keep.txt")))
		worker <- err
	}()
	select {
	case <-inside:
	case <-time.After(10 * time.Second):
		t.Fatal("the replace did not start")
	}
	started := time.Now()
	err := ix.Drop("demo-releases", "gone.txt", "b")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed >= 200*time.Millisecond {
		t.Errorf("drop waited %s", elapsed)
	}
	if mustLookup(t, ix, "demo-releases", "gone.txt") == nil {
		t.Error("drop removed the row while a write ran")
	}
	free()
	if err := <-worker; err != nil {
		t.Fatal(err)
	}
	if err := ix.Drop("demo-releases", "gone.txt", "b"); err != nil {
		t.Fatal(err)
	}
	if mustLookup(t, ix, "demo-releases", "gone.txt") != nil {
		t.Fatal("drop kept the row after the write")
	}
}

// TestDropGivesUpOnAnotherWriter: a write of a second Index on the same
// file holds the lock; Drop waits its short busy timeout, keeps the row and
// reports no error. Later writes wait the long busy timeout again.
func TestDropGivesUpOnAnotherWriter(t *testing.T) {
	ix, path := openIndex(t)
	mustReplace(t, ix, "b", slice(entry("gone.txt")))
	other, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	inside, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free() // runs before other.Close, which waits for the held write
	worker := make(chan error, 1)
	go func() {
		_, err := other.ReplaceStore("a", slowEntries(inside, release, entry("keep.txt")))
		worker <- err
	}()
	<-inside
	started := time.Now()
	err = ix.Drop("demo-releases", "gone.txt", "b")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("drop against a busy file: %v", err)
	}
	if elapsed < 150*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("drop took %s, want about the 250 ms busy timeout", elapsed)
	}
	if mustLookup(t, ix, "demo-releases", "gone.txt") == nil {
		t.Error("drop removed the row while another index wrote")
	}
	// A write now waits for the other writer instead of failing.
	go func() {
		time.Sleep(700 * time.Millisecond)
		free()
	}()
	started = time.Now()
	if _, err := ix.ReplaceStore("c", slice(entry("c.txt"))); err != nil {
		t.Fatalf("write after the drop: %v", err)
	}
	if waited := time.Since(started); waited < 400*time.Millisecond {
		t.Errorf("the write waited %s, the other writer held the lock for 700 ms", waited)
	}
	if err := <-worker; err != nil {
		t.Fatal(err)
	}
	if err := ix.Drop("demo-releases", "gone.txt", "b"); err != nil {
		t.Fatal(err)
	}
	if mustLookup(t, ix, "demo-releases", "gone.txt") != nil {
		t.Fatal("drop kept the row once the file was free")
	}
}

func TestGenerationChangesWithEveryWrite(t *testing.T) {
	ix, path := openIndex(t)
	seen := []int64{mustGeneration(t, ix)}
	mustReplace(t, ix, "a", slice(entry("x/1.txt"), entry("x/2.txt")))
	seen = append(seen, mustGeneration(t, ix))
	if err := ix.Drop("demo-releases", "x/1.txt", "a"); err != nil {
		t.Fatal(err)
	}
	seen = append(seen, mustGeneration(t, ix))
	other, err := index.Open(path) // another process writes the same file
	if err != nil {
		t.Fatal(err)
	}
	raw := entry("y.txt")
	raw.Repo = "demo-raw"
	_, err = other.ReplaceStore("b", slice(raw))
	other.Close()
	if err != nil {
		t.Fatal(err)
	}
	seen = append(seen, mustGeneration(t, ix))
	if err := ix.RemoveStore("b"); err != nil {
		t.Fatal(err)
	}
	seen = append(seen, mustGeneration(t, ix))
	if err := ix.SetHints("a", map[string]string{"demo-releases": "raw"}); err != nil {
		t.Fatal(err)
	}
	seen = append(seen, mustGeneration(t, ix))
	unique := map[int64]bool{}
	for _, g := range seen {
		unique[g] = true
	}
	if len(unique) != len(seen) {
		t.Fatalf("generations repeat: %v", seen)
	}
	if again := mustGeneration(t, ix); again != seen[len(seen)-1] {
		t.Fatalf("a read changed the generation: %d, then %d", seen[len(seen)-1], again)
	}
}

// TestManyReadersAndWriters runs every call at once; -race checks the
// shared state of the index.
func TestManyReadersAndWriters(t *testing.T) {
	ix, path := openIndex(t)
	mustReplace(t, ix, "a", slice(entry("d/one.txt"), entry("d/two.txt")))
	// A store that only has hints has no tables, and reads fail on it, as
	// in Python; so every store gets its rows first.
	mustReplace(t, ix, "b", slice(entry("d/b.txt")))
	mustReplace(t, ix, "c", slice(entry("d/c.txt")))
	other, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var done sync.WaitGroup
	errs := make(chan error, 64)
	report := func(err error) {
		if err != nil {
			errs <- err
		}
	}
	for n := range 8 {
		done.Add(1)
		go func() {
			defer done.Done()
			for i := range 25 {
				_, err := ix.Lookup("demo-releases", "d/one.txt")
				report(err)
				_, err = ix.Children("demo-releases", "d", 10, 0)
				report(err)
				_, err = ix.IsDir("demo-releases", "d")
				report(err)
				_, err = ix.RepoStats()
				report(err)
				_, err = ix.StoreStats()
				report(err)
				_, err = ix.RepoNames()
				report(err)
				_, err = ix.Hints()
				report(err)
				_, err = ix.Generation()
				report(err)
				switch (n + i) % 4 {
				case 0:
					_, err = ix.ReplaceStore("b", slice(entry(fmt.Sprintf("d/b%d.txt", i))))
				case 1:
					err = ix.Drop("demo-releases", fmt.Sprintf("d/b%d.txt", i-1), "b")
				case 2:
					err = ix.SetHints("b", map[string]string{"demo-releases": "raw"})
				case 3:
					_, err = other.ReplaceStore("c", slice(entry(fmt.Sprintf("d/c%d.txt", i))))
				}
				report(err)
			}
		}()
	}
	done.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestCloseWaitsForARunningWrite: Close lets a running write finish, as
// close() in Python waits for the write lock.
func TestCloseWaitsForARunningWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	worker := make(chan error, 1)
	go func() {
		_, err := ix.ReplaceStore("a", slowEntries(inside, release, entry("kept.txt")))
		worker <- err
	}()
	<-inside
	closed := make(chan error, 1)
	go func() { closed <- ix.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned during a write: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-worker; err != nil {
		t.Fatalf("the running write: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	again, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if mustLookup(t, again, "demo-releases", "kept.txt") == nil {
		t.Fatal("the write that ran during Close is lost")
	}
}

// TestJournalModeIsWAL checks the mode of the file and of a second writer.
func TestJournalModeIsWAL(t *testing.T) {
	_, path := openIndex(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode %q", mode)
	}
}
