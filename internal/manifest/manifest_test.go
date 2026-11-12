package manifest_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/manifest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

type storeCase struct {
	Name          string            `json:"name"`
	Via           string            `json:"via"`
	Files         map[string]string `json:"files"`
	ManifestFiles []fileJSON        `json:"manifest_files"`
	Entries       *[]struct {
		Repo        string          `json:"repo"`
		Path        string          `json:"path"`
		Size        int64           `json:"size"`
		SHA1        *string         `json:"sha1"`
		ContentType *string         `json:"content_type"`
		CreatedMS   json.RawMessage `json:"created_ms"`
		Location    string          `json:"location"`
	} `json:"entries"`
	Error  *errorJSON `json:"error"`
	Tables int        `json:"tables"`
}

func storeKind(err error) string {
	var notFound *stores.NotFoundError
	var storeErr *stores.Error
	switch {
	case errors.As(err, &notFound):
		return "FileNotFoundError"
	case errors.As(err, &storeErr):
		return storeErr.Class
	}
	return "Exception"
}

func entries(t *testing.T, s stores.Store) ([]stores.Entry, error) {
	t.Helper()
	var out []stores.Entry
	err := s.Entries(nil, func(e stores.Entry) error {
		out = append(out, e)
		return nil
	})
	slices.SortFunc(out, func(a, b stores.Entry) int {
		if c := strings.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out, err
}

func digest(t *testing.T, root string) string {
	t.Helper()
	d, err := testkit.TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The directory store cases of tests/test_manifest.py, through the real
// lookup that internal/stores opens by default.
func TestDirectoryStoreVectors(t *testing.T) {
	v := load(t)
	if len(v.Stores) == 0 {
		t.Fatal("no store cases")
	}
	for _, c := range v.Stores {
		t.Run(c.Name, func(t *testing.T) {
			captureLogs(t)
			base := t.TempDir()
			files := map[string][]byte{}
			for path, data := range c.Files {
				files[path] = unb64(t, data)
			}
			root, err := testkit.MakeTree(filepath.Join(base, "tree"), files)
			if err != nil {
				t.Fatal(err)
			}
			materialize(t, base, c.ManifestFiles)
			manifestDir := filepath.Join(base, "tree-manifest")
			var s stores.Store
			cache := filepath.Join(base, "data", "manifests")
			if c.Via == "make" {
				s, err = stores.Make(config.StoreConfig{Name: "tree", Kind: "directory", Root: root, Manifest: &manifestDir},
					filepath.Join(base, "data"))
			} else {
				cache = filepath.Join(base, "cache")
				s, err = stores.NewDirectoryStore("tree", root, nil, &manifestDir, &cache)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := [2]string{digest(t, root), digest(t, manifestDir)}
			got, err := entries(t, s)
			if c.Error != nil {
				message := ""
				if err != nil {
					message = strings.ReplaceAll(err.Error(), base, "<base>")
				}
				if storeKind(err) != kind(c.Error) || message != c.Error.Message {
					t.Errorf("Entries error %s %q, want %s %q", storeKind(err), message, kind(c.Error), c.Error.Message)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				want := *c.Entries
				if len(got) != len(want) {
					t.Fatalf("%d entries, want %d", len(got), len(want))
				}
				for i, e := range got {
					w := want[i]
					created := string(w.CreatedMS)
					if created == `"mtime"` {
						info, err := os.Stat(filepath.Join(root, e.Location))
						if err != nil {
							t.Fatal(err)
						}
						created = fmt.Sprint(info.ModTime().UnixNano() / 1_000_000)
					}
					if e.Repo != w.Repo || e.Path != w.Path || e.Size != w.Size || show(e.SHA1) != show(w.SHA1) ||
						show(e.ContentType) != show(w.ContentType) || fmt.Sprint(e.CreatedMS) != created ||
						e.Location != w.Location {
						t.Errorf("entry %d:\n got %+v %s %s\nwant %+v %s %s", i, e, show(e.SHA1), show(e.ContentType),
							w, show(w.SHA1), show(w.ContentType))
					}
				}
				if c.Name == "test_manifest_never_writes" {
					if _, err := entries(t, s); err != nil {
						t.Fatal(err)
					}
				}
			}
			if after := [2]string{digest(t, root), digest(t, manifestDir)}; after != before {
				t.Error("the walk changed the tree or the manifest")
			}
			tables, _ := filepath.Glob(filepath.Join(cache, "*.sqlite"))
			if len(tables) != c.Tables {
				t.Errorf("%d tables in the cache directory, want %d", len(tables), c.Tables)
			}
		})
	}
}

// Native cases, written before the port.

func writeManifest(t *testing.T, path string, rows ...testkit.ManifestRow) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testkit.BlobManifest(rows, testkit.DefaultManifestStore), 0o666); err != nil {
		t.Fatal(err)
	}
	return path
}

func text(s string) *string { return &s }

// A line longer than the read buffer of a plain part is one row.
func TestReadRowsLineLongerThanTheBuffer(t *testing.T) {
	long := "long/" + strings.Repeat("y", manifest.ReadBuffer+100) + ".txt"
	source := writeManifest(t, filepath.Join(t.TempDir(), "part-00000.tsv"),
		testkit.ManifestRow{Repo: "demo-raw", Path: long, Size: 1, CreatedMS: 2},
		testkit.ManifestRow{Repo: "demo-raw", Path: "after.txt", Size: 3, CreatedMS: 4})
	var paths []string
	if err := manifest.ReadRows(source, func(r manifest.Row) error {
		paths = append(paths, r.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{long, "after.txt"}) {
		t.Fatalf("got %d rows, want the long row and after.txt", len(paths))
	}
}

// An error of the consumer stops the read and comes back unchanged.
func TestReadRowsStopsOnTheConsumerError(t *testing.T) {
	source := writeManifest(t, filepath.Join(t.TempDir(), "part-00000.tsv"),
		testkit.ManifestRow{Repo: "demo-raw", Path: "a.txt", Size: 1, CreatedMS: 1},
		testkit.ManifestRow{Repo: "demo-raw", Path: "b.txt", Size: 1, CreatedMS: 1})
	stop := errors.New("stop")
	calls := 0
	err := manifest.ReadRows(source, func(manifest.Row) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("ReadRows = %v after %d calls, want the consumer error after 1", err, calls)
	}
}

// Missing sources and a missing zstd program match fs.ErrNotExist, as a
// FileNotFoundError does.
func TestFileNotFoundErrorsMatchErrNotExist(t *testing.T) {
	base := t.TempDir()
	_, err := manifest.Open(filepath.Join(base, "none"), filepath.Join(base, "cache"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open of a missing source = %v, want fs.ErrNotExist", err)
	}
	if err := manifest.ReadRows(filepath.Join(base, "none"), func(manifest.Row) error { return nil }); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadRows of a missing source = %v, want fs.ErrNotExist", err)
	}
	var me *manifest.Error
	if !errors.As(err, &me) || me.Class != "FileNotFoundError" {
		t.Errorf("error %v is no FileNotFoundError", err)
	}
}

// Parts lists the part files of a directory in name order and the file
// itself otherwise.
func TestParts(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"part-1.tsv", "part-0.tsv.zst", "part-0.tsv.gz", "other.tsv", "part-2.txt"} {
		writeManifest(t, filepath.Join(base, "m", name))
	}
	got, err := manifest.Parts(filepath.Join(base, "m"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{base + "/m/part-0.tsv.gz", base + "/m/part-0.tsv.zst", base + "/m/part-1.tsv"}
	if !slices.Equal(got, want) {
		t.Errorf("Parts = %q, want %q", got, want)
	}
	single := filepath.Join(base, "m", "other.tsv")
	if got, err := manifest.Parts(single); err != nil || !slices.Equal(got, []string{single}) {
		t.Errorf("Parts(file) = %q, %v", got, err)
	}
}

// The lookup answers from many goroutines at once.
func TestLookupConcurrentGets(t *testing.T) {
	base := t.TempDir()
	var rows []testkit.ManifestRow
	for i := range 50 {
		rows = append(rows, testkit.ManifestRow{Repo: "demo-raw", Path: fmt.Sprintf("f/%02d.txt", i), Size: int64(i),
			CreatedMS: 1, SHA1: text(strings.Repeat("a", 40)), ContentType: text("text/plain")})
	}
	writeManifest(t, filepath.Join(base, "m", "part-00000.tsv"), rows...)
	l, err := manifest.Open(filepath.Join(base, "m"), filepath.Join(base, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			for i := range 50 {
				row, err := l.Get("demo-raw", fmt.Sprintf("f/%02d.txt", i))
				if err != nil || row == nil || row.Size != int64(i) {
					errs <- fmt.Errorf("Get %d = %+v, %v", i, row, err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// The table is opened read only: the lookup never writes into it.
func TestLookupTableIsReadOnly(t *testing.T) {
	base := t.TempDir()
	writeManifest(t, filepath.Join(base, "m", "part-00000.tsv"),
		testkit.ManifestRow{Repo: "demo-raw", Path: "a.txt", Size: 1, CreatedMS: 1})
	l, err := manifest.Open(filepath.Join(base, "m"), filepath.Join(base, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Exec("DELETE FROM rows"); err == nil {
		t.Fatal("a write through the lookup connection succeeded")
	}
	if row, err := l.Get("demo-raw", "a.txt"); err != nil || row == nil {
		t.Fatalf("Get after the refused write = %+v, %v", row, err)
	}
}

// The default lookup of internal/stores is this package; a store whose
// lookup is unset fails with stores.ErrNoManifestLookup.
func TestStoresUseTheLookup(t *testing.T) {
	if stores.OpenManifestLookup == nil {
		t.Fatal("stores.OpenManifestLookup is not set")
	}
	base := t.TempDir()
	root, err := testkit.MakeTree(filepath.Join(base, "tree"), map[string][]byte{"demo-raw/a.txt": []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	source := writeManifest(t, filepath.Join(base, "m", "part-00000.tsv"), testkit.ManifestRow{
		Repo: "demo-raw", Path: "a.txt", Size: 1, CreatedMS: 7, SHA1: text(strings.Repeat("b", 40))})
	s, err := stores.Make(config.StoreConfig{Name: "tree", Kind: "directory", Root: root, Manifest: &source},
		filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := entries(t, s)
	if err != nil || len(got) != 1 || show(got[0].SHA1) != show(text(strings.Repeat("b", 40))) || got[0].CreatedMS != 7 {
		t.Fatalf("entries %+v, %v; want the row of the manifest", got, err)
	}
	tables, _ := filepath.Glob(filepath.Join(base, "data", "manifests", "*.sqlite"))
	if len(tables) != 1 {
		t.Errorf("%d tables below data/manifests, want 1", len(tables))
	}
}
