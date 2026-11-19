package index_test

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

func walkPaths(t *testing.T, ix *index.Index, repo, directory string) []string {
	t.Helper()
	var paths []string
	err := ix.WalkPrefix(repo, directory, func(a index.Asset) error {
		paths = append(paths, a.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestWalkPrefixBounds(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "s1", slice(
		entry("a/one"), entry("a/b/two"), entry("a/b/c/three"),
		entry("a-b/no"), entry("ab/no"), entry("a"), entry("a0/no"), entry("a.b/no"),
		entry("p%q/in"), entry("p%q/r/in"), entry("pxq/no"), entry("p_q/in"), entry("paq/no"),
	))
	cases := []struct {
		directory string
		want      []string
	}{
		{"a", []string{"a/one", "a/b/two", "a/b/c/three"}},
		{"a/b", []string{"a/b/two", "a/b/c/three"}},
		{"/a/b/", []string{"a/b/two", "a/b/c/three"}},
		{"a/b/c", []string{"a/b/c/three"}},
		{"p%q", []string{"p%q/in", "p%q/r/in"}},
		{"p_q", []string{"p_q/in"}},
		{"a/one", nil}, // a file is no directory
		{"missing", nil},
	}
	for _, c := range cases {
		if got := walkPaths(t, ix, "demo-releases", c.directory); !slices.Equal(got, c.want) {
			t.Errorf("WalkPrefix(%q) = %q, want %q", c.directory, got, c.want)
		}
	}
	if got := walkPaths(t, ix, "other-repo", "a"); got != nil {
		t.Errorf("another repository gave %q", got)
	}
}

func TestWalkPrefixRootInParentOrder(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "s1", slice(entry("z"), entry("b/x"), entry("a"), entry("b/a/y"), entry("b-c/w")))
	want := []string{"a", "z", "b/x", "b-c/w", "b/a/y"} // parent "", then "b", "b-c", "b/a"
	for _, directory := range []string{"", "/"} {
		if got := walkPaths(t, ix, "demo-releases", directory); !slices.Equal(got, want) {
			t.Errorf("WalkPrefix(%q) = %q, want %q", directory, got, want)
		}
	}
}

func TestWalkPrefixWinnerOverStores(t *testing.T) {
	ix, _ := openIndex(t)
	row := func(path string, created int64, location string) stores.Entry {
		return stores.Entry{Repo: "demo-releases", Path: path, Size: created, CreatedMS: created, Location: location}
	}
	// "newer" is newer in s2; "tie" has the same time in both, and Lookup takes the store whose name sorts first
	mustReplace(t, ix, "s2", slice(row("d/newer", 20, "s2-newer"), row("d/tie", 5, "s2-tie"), row("d/only2", 1, "s2-only")))
	mustReplace(t, ix, "s1", slice(row("d/newer", 10, "s1-newer"), row("d/tie", 5, "s1-tie"), row("d/only1", 1, "s1-only")))
	var got []index.Asset
	if err := ix.WalkPrefix("demo-releases", "d", func(a index.Asset) error { got = append(got, a); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d rows, want one per path: %+v", len(got), got)
	}
	for _, a := range got {
		want := mustLookup(t, ix, "demo-releases", a.Path)
		if a.Store != want.Store || a.Location != want.Location || a.CreatedMS != want.CreatedMS || a.Size != want.Size {
			t.Errorf("%s: walk gave %+v, Lookup %+v", a.Path, a, *want)
		}
	}
	if got[0].Path != "d/newer" || got[0].Store != "s2" || got[3].Path != "d/tie" || got[3].Store != "s1" {
		t.Errorf("winners %+v", got)
	}
}

func TestWalkPrefixStops(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "s1", slice(entry("d/1"), entry("d/2"), entry("d/3")))
	calls := 0
	err := ix.WalkPrefix("demo-releases", "d", func(index.Asset) error {
		calls++
		if calls == 2 {
			return index.ErrStop
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err %v, calls %d", err, calls)
	}
	failure := errors.New("of the callback")
	if err := ix.WalkPrefix("demo-releases", "d", func(index.Asset) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("err %v, want the error of the callback", err)
	}
}

func TestWalkPrefixClosedIndex(t *testing.T) {
	ix, _ := openIndex(t)
	mustReplace(t, ix, "s1", slice(entry("d/1")))
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	err := ix.WalkPrefix("demo-releases", "d", func(index.Asset) error { return nil })
	if !errors.Is(err, index.ErrClosed) {
		t.Fatalf("err %v, want ErrClosed", err)
	}
}

func TestWalkPrefixEqualsRecursionOverChildren(t *testing.T) {
	ix, _ := openIndex(t)
	random := rand.New(rand.NewSource(7))
	var a, b []stores.Entry
	for i := 0; i < 2000; i++ {
		path := fmt.Sprintf("top/d%d/e%d/f%d.bin", random.Intn(7), random.Intn(11), i)
		if i%5 == 0 {
			path = fmt.Sprintf("top/d%d/f%d.txt", random.Intn(7), i)
		}
		e := stores.Entry{Repo: "demo-releases", Path: path, Size: int64(i), CreatedMS: int64(i), Location: "l" + path}
		if i%3 == 0 {
			b = append(b, e)
		} else {
			a = append(a, e)
		}
	}
	mustReplace(t, ix, "s1", slice(a...))
	mustReplace(t, ix, "s2", slice(b...))
	var recursed []string
	var walk func(directory string)
	walk = func(directory string) {
		children, err := ix.Children("demo-releases", directory, 1_000_000, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range children {
			full := directory + "/" + c.Name
			if c.IsDir {
				walk(full)
			} else {
				recursed = append(recursed, full)
			}
		}
	}
	walk("top")
	walked := walkPaths(t, ix, "demo-releases", "top")
	if len(walked) != 2000 {
		t.Fatalf("walk gave %d paths, want 2000", len(walked))
	}
	slices.Sort(recursed)
	sorted := slices.Clone(walked)
	slices.Sort(sorted)
	if !slices.Equal(sorted, recursed) {
		t.Fatal("the paths of the walk differ from the recursion over Children")
	}
}

func walkEnding(t *testing.T, ix *index.Index, repo, directory, ending string) []index.Asset {
	t.Helper()
	var out []index.Asset
	if err := ix.WalkPrefixEnding(repo, directory, ending, func(a index.Asset) error { out = append(out, a); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

// WalkPrefixEnding hands fn only the rows whose name ends with the ending,
// so a walk for npm tarballs does not pass every package document through
// Go: the export of a folder counts only its entries against the limit.
func TestWalkPrefixEndingLeavesOutOtherNames(t *testing.T) {
	ix, _ := openIndex(t)
	row := func(path string, created int64, location string) stores.Entry {
		return stores.Entry{Repo: "demo-npm", Path: path, Size: created, CreatedMS: created, Location: location}
	}
	mustReplace(t, ix, "s2", slice(row("widget/-/widget-1.0.0.tgz", 20, "s2-newer"), row("widget", 1, "s2-doc")))
	mustReplace(t, ix, "s1", slice(
		row("widget/-/widget-1.0.0.tgz", 10, "s1-older"), row("widget/-/widget-1.1.0.TGZ", 1, "upper"), row("widget/-/widget.tgz.bak", 1, "bak"),
		row("tgz", 1, "bare"), row("@demo/util/-/util-2.0.0.tgz", 1, "scoped"), row("notes/readme.txt", 1, "text"),
		row("v/v[1].tgz", 1, "bracket"), row("v/v1.tgz", 1, "plain"), row("v/a*b.tgz", 1, "star"),
	))
	var paths []string
	got := walkEnding(t, ix, "demo-npm", "", ".tgz")
	for _, a := range got {
		paths = append(paths, a.Path)
	}
	want := []string{"@demo/util/-/util-2.0.0.tgz", "v/a*b.tgz", "v/v1.tgz", "v/v[1].tgz", "widget/-/widget-1.0.0.tgz"}
	if !slices.Equal(paths, want) {
		t.Fatalf("WalkPrefixEnding(.tgz) = %q, want %q", paths, want)
	}
	if last := got[len(got)-1]; last.Location != "s2-newer" {
		t.Errorf("the winner of a path held in two stores is %+v", last)
	}
	// glob characters of the ending are plain characters
	for ending, want := range map[string][]string{"[1].tgz": {"v/v[1].tgz"}, "*b.tgz": {"v/a*b.tgz"}, "?.tgz": nil} {
		var paths []string
		for _, a := range walkEnding(t, ix, "demo-npm", "v", ending) {
			paths = append(paths, a.Path)
		}
		if !slices.Equal(paths, want) {
			t.Errorf("WalkPrefixEnding(%q) = %q, want %q", ending, paths, want)
		}
	}
	// the positive twin: the empty ending walks what WalkPrefix walks
	var all []string
	for _, a := range walkEnding(t, ix, "demo-npm", "", "") {
		all = append(all, a.Path)
	}
	if prefix := walkPaths(t, ix, "demo-npm", ""); !slices.Equal(all, prefix) || len(all) != 10 {
		t.Fatalf("WalkPrefixEnding(\"\") = %q, WalkPrefix %q", all, prefix)
	}
}
