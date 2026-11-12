package stores_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/blobprops"
	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
)

// The vectors come from tools/vectors/stores.py. A case is a list of steps
// that the script ran against the Python module; each step that answers
// holds the answer in "result".
type vectorFile struct {
	Cases      []vectorCase `json:"cases"`
	MediaTypes []struct {
		Input  string  `json:"input_b64"`
		Expect *string `json:"expect"`
	} `json:"media_types"`
	ManifestPaths []struct {
		Path   string `json:"path"`
		Expect bool   `json:"expect"`
	} `json:"manifest_paths"`
	Reprs []struct {
		Input  string `json:"input_b64"`
		Expect string `json:"expect"`
	} `json:"reprs"`
	PurePaths []struct {
		Path   *string    `json:"path"`
		Join   *[2]string `json:"join"`
		Expect string     `json:"expect"`
	} `json:"pure_paths"`
}

type vectorCase struct {
	Name  string `json:"name"`
	Steps []step `json:"steps"`
}

type step struct {
	Op        string            `json:"op"`
	Root      string            `json:"root"`
	Path      string            `json:"path"`
	Link      string            `json:"link"`
	Target    string            `json:"target"`
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Name      *string           `json:"name"`
	Store     string            `json:"store"`
	Location  string            `json:"location"`
	B64       string            `json:"b64"`
	PrefixB64 string            `json:"prefix_b64"`
	FillB64   string            `json:"fill_b64"`
	Size      int               `json:"size"`
	Seconds   int64             `json:"seconds"`
	Specs     []specJSON        `json:"specs"`
	Files     map[string]string `json:"files"`
	Repos     *[]string         `json:"repos"`
	Relative  bool              `json:"relative"`
	Counts    bool              `json:"counts"`
	Config    *makeConfig       `json:"config"`
	DataDir   *string           `json:"data_dir"`
	Home      *string           `json:"HOME"`
	Result    json.RawMessage   `json:"result"`
}

type specJSON struct {
	Repo         string  `json:"repo"`
	Path         string  `json:"path"`
	Content      string  `json:"content_b64"`
	Scheme       string  `json:"scheme"`
	CreatedMS    int64   `json:"created_ms"`
	Deleted      bool    `json:"deleted"`
	CopyOnly     bool    `json:"copy_only"`
	NoBytes      bool    `json:"no_bytes"`
	ContentType  *string `json:"content_type"`
	LeadingSlash bool    `json:"leading_slash"`
}

type makeConfig struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Root     string    `json:"root"`
	Repos    *[]string `json:"repos"`
	Manifest *string   `json:"manifest"`
}

type pyError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

type entryJSON struct {
	Repo        string          `json:"repo"`
	Path        string          `json:"path"`
	Size        int64           `json:"size"`
	SHA1        *string         `json:"sha1"`
	ContentType *string         `json:"content_type"`
	CreatedMS   json.RawMessage `json:"created_ms"`
	Location    string          `json:"location"`
}

type attrsJSON struct {
	Type     string    `json:"type"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Root     string    `json:"root"`
	Repos    *[]string `json:"repos"`
	Manifest *string   `json:"manifest"`
	CacheDir *string   `json:"cache_dir"`
}

type result struct {
	Error    *pyError    `json:"error"`
	Entries  []entryJSON `json:"entries"`
	RawCount *struct {
		Live       int `json:"live"`
		Deleted    int `json:"deleted"`
		Copies     int `json:"copies"`
		NoBytes    int `json:"no_bytes"`
		Unparsable int `json:"unparsable"`
	} `json:"counts"`
	B64   *string `json:"b64"`
	Files []struct {
		Location string `json:"location"`
		B64      string `json:"b64"`
	} `json:"files"`
	Digest   string `json:"digest"`
	Value    *bool  `json:"value"`
	Location string `json:"location"`
	Rows     *int   `json:"rows"`
	Lookup   []struct {
		Repo        string  `json:"repo"`
		Path        string  `json:"path"`
		Size        int64   `json:"size"`
		SHA1        *string `json:"sha1"`
		ContentType *string `json:"content_type"`
		CreatedMS   int64   `json:"created_ms"`
	} `json:"lookup"`
	attrsJSON
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/stores.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func unb64(t *testing.T, text string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func show(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

type runner struct {
	t       *testing.T
	base    string
	stores  map[string]stores.Store
	touched map[string]bool
}

func (r *runner) sub(text string) string {
	return strings.ReplaceAll(text, "<base>", r.base)
}

func (r *runner) message(text string) string {
	return strings.ReplaceAll(r.sub(text), "<home>", os.Getenv("HOME"))
}

func (r *runner) path(rel string) string {
	if strings.HasPrefix(rel, "<base>") {
		return r.sub(rel)
	}
	return filepath.Join(r.base, rel)
}

func (r *runner) result(s step) *result {
	r.t.Helper()
	var out result
	if len(s.Result) == 0 || string(s.Result) == "null" {
		r.t.Fatalf("step %s has no result", s.Op)
	}
	if err := json.Unmarshal(s.Result, &out); err != nil {
		r.t.Fatal(err)
	}
	return &out
}

// checkError compares err with the exception the Python step raised.
func (r *runner) checkError(label string, err error, want *pyError) {
	r.t.Helper()
	if err == nil {
		r.t.Errorf("%s: no error, want %s %q", label, want.Class, want.Message)
		return
	}
	message := r.message(want.Message)
	if err.Error() != message {
		r.t.Errorf("%s: error %q, want %q", label, err.Error(), message)
	}
	var classErr *stores.Error
	var valueErr *blobprops.ValueError
	switch {
	case want.Class == "FileNotFoundError":
		if !errors.Is(err, fs.ErrNotExist) {
			r.t.Errorf("%s: error %v is not fs.ErrNotExist", label, err)
		}
	case errors.As(err, &classErr):
		if classErr.Class != want.Class {
			r.t.Errorf("%s: error class %s, want %s", label, classErr.Class, want.Class)
		}
	case errors.As(err, &valueErr):
		if want.Class != "ValueError" {
			r.t.Errorf("%s: a ValueError, want %s", label, want.Class)
		}
	default:
		r.t.Errorf("%s: error %T %v, want %s", label, err, err, want.Class)
	}
}

func (r *runner) checkAttrs(label string, s stores.Store, want attrsJSON) {
	r.t.Helper()
	gotType := ""
	switch s.(type) {
	case *stores.BlobStore:
		gotType = "NexusFileStore" // the class name of the reference
	case *stores.DirectoryStore:
		gotType = "DirectoryStore"
	}
	if gotType != want.Type || s.Name() != want.Name || s.Kind() != want.Kind || s.Root() != r.message(want.Root) {
		r.t.Errorf("%s: store %s %q %q %q, want %s %q %q %q", label, gotType, s.Name(), s.Kind(), s.Root(),
			want.Type, want.Name, want.Kind, r.message(want.Root))
	}
	d, ok := s.(*stores.DirectoryStore)
	if !ok {
		return
	}
	repos := d.Repos()
	if (repos == nil) != (want.Repos == nil) || (want.Repos != nil && strings.Join(repos, "\x00") != strings.Join(*want.Repos, "\x00")) {
		r.t.Errorf("%s: repos %q (nil %v), want %v", label, repos, repos == nil, want.Repos)
	}
	sub := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return r.message(*p)
	}
	if show(d.Manifest()) != sub(want.Manifest) || show(d.CacheDir()) != sub(want.CacheDir) {
		r.t.Errorf("%s: manifest %s cache %s, want %s %s", label, show(d.Manifest()), show(d.CacheDir()),
			sub(want.Manifest), sub(want.CacheDir))
	}
}

func (r *runner) checkEntries(label string, s stores.Store, got []stores.Entry, want []entryJSON) {
	r.t.Helper()
	if len(got) != len(want) {
		paths := make([]string, len(got))
		for i, e := range got {
			paths[i] = e.Repo + ":" + e.Path
		}
		r.t.Errorf("%s: %d entries %q, want %d", label, len(got), paths, len(want))
		return
	}
	for i, w := range want {
		g := got[i]
		var created int64
		if string(w.CreatedMS) == `"mtime"` {
			info, err := os.Stat(filepath.Join(s.Root(), filepath.FromSlash(w.Location)))
			if err != nil {
				r.t.Fatal(err)
			}
			mtime := info.ModTime()
			created = mtime.Unix()*1000 + int64(mtime.Nanosecond())/1_000_000
		} else if err := json.Unmarshal(w.CreatedMS, &created); err != nil {
			r.t.Fatal(err)
		}
		if g.Repo != w.Repo || g.Path != w.Path || g.Size != w.Size || show(g.SHA1) != show(w.SHA1) ||
			show(g.ContentType) != show(w.ContentType) || g.CreatedMS != created || g.Location != w.Location {
			r.t.Errorf("%s: entry %d = %+v sha1=%s type=%s, want %+v sha1=%s type=%s created=%d", label, i, g,
				show(g.SHA1), show(g.ContentType), w, show(w.SHA1), show(w.ContentType), created)
		}
	}
}

func collect(s stores.Store, counts *stores.ScanCounts) ([]stores.Entry, error) {
	var got []stores.Entry
	err := s.Entries(counts, func(e stores.Entry) error {
		got = append(got, e)
		return nil
	})
	return got, err
}

func readAll(s stores.Store, location string) ([]byte, error) {
	f, err := s.Open(location)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

type fakeLookup struct {
	rows map[[2]string]stores.ManifestRow
}

func (f *fakeLookup) Get(repo, path string) (*stores.ManifestRow, error) {
	row, ok := f.rows[[2]string{repo, path}]
	if !ok {
		return nil, nil
	}
	return &row, nil
}

func (f *fakeLookup) Close() error { return nil }

func toSpecs(t *testing.T, items []specJSON) []testkit.BlobSpec {
	specs := make([]testkit.BlobSpec, len(items))
	for i, s := range items {
		specs[i] = testkit.BlobSpec{
			Repo: s.Repo, Path: s.Path, Content: unb64(t, s.Content), Scheme: s.Scheme, CreatedMS: s.CreatedMS,
			Deleted: s.Deleted, CopyOnly: s.CopyOnly, NoBytes: s.NoBytes, ContentType: s.ContentType,
			LeadingSlash: s.LeadingSlash,
		}
	}
	return specs
}

func (r *runner) run(s step) {
	t := r.t
	t.Helper()
	label := s.Op + " " + s.ID + s.Store + " " + s.Location
	switch s.Op {
	case "blob_store":
		if _, err := testkit.MakeBlobStore(r.path(s.Root), toSpecs(t, s.Specs)); err != nil {
			t.Fatal(err)
		}
	case "unparsable":
		name := testkit.DefaultUnparsable
		got, err := testkit.AddUnparsable(r.path(s.Root), name)
		if err != nil || got != r.result(s).Location {
			t.Fatalf("unparsable: %q %v", got, err)
		}
	case "tree":
		files := map[string][]byte{}
		for k, v := range s.Files {
			files[k] = unb64(t, v)
		}
		if _, err := testkit.MakeTree(r.path(s.Root), files); err != nil {
			t.Fatal(err)
		}
	case "write", "pad":
		data := unb64(t, s.B64)
		if s.Op == "pad" {
			data = unb64(t, s.PrefixB64)
			fill := unb64(t, s.FillB64)
			data = append(data, bytes.Repeat(fill, s.Size-len(data))...)
		}
		target := r.path(s.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o666); err != nil {
			t.Fatal(err)
		}
	case "mkdir":
		if err := os.MkdirAll(r.path(s.Path), 0o777); err != nil {
			t.Fatal(err)
		}
	case "utime":
		when := time.Unix(s.Seconds, 0)
		if err := os.Chtimes(r.path(s.Path), when, when); err != nil {
			t.Fatal(err)
		}
		_, rest, _ := strings.Cut(s.Path, "/")
		r.touched[rest] = true
	case "symlink":
		link := r.path(s.Link)
		if err := os.MkdirAll(filepath.Dir(link), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(r.sub(s.Target), link); err != nil {
			t.Fatal(err)
		}
	case "unlink":
		if err := os.Remove(r.path(s.Path)); err != nil {
			t.Fatal(err)
		}
	case "chdir":
		t.Chdir(r.path(s.Path))
	case "env":
		t.Setenv("HOME", r.sub(*s.Home))
	case "store":
		root := s.Root
		if !s.Relative {
			root = r.path(s.Root)
		}
		var made stores.Store
		var err error
		if s.Kind == "nexus-file" {
			made, err = stores.NewBlobStore(*s.Name, root)
		} else {
			var repos []string
			if s.Repos != nil {
				repos = append([]string{}, *s.Repos...)
			}
			made, err = stores.NewDirectoryStore(*s.Name, root, repos, nil, nil)
		}
		if len(s.Result) > 0 && string(s.Result) != "null" {
			r.checkError(label, err, r.result(s).Error)
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		r.stores[s.ID] = made
	case "manifest_store":
		source, cache := filepath.Join(r.base, "manifest.tsv"), filepath.Join(r.base, "cache")
		fake := &fakeLookup{rows: map[[2]string]stores.ManifestRow{}}
		for _, row := range r.result(s).Lookup {
			fake.rows[[2]string{row.Repo, row.Path}] = stores.ManifestRow{
				Repo: row.Repo, Path: row.Path, Size: row.Size, SHA1: row.SHA1, ContentType: row.ContentType,
				CreatedMS: row.CreatedMS,
			}
		}
		saved := stores.OpenManifestLookup
		t.Cleanup(func() { stores.OpenManifestLookup = saved })
		stores.OpenManifestLookup = func(gotSource, gotCache string) (stores.ManifestLookup, error) {
			if gotSource != source || gotCache != cache {
				t.Errorf("lookup opened on %q %q, want %q %q", gotSource, gotCache, source, cache)
			}
			return fake, nil
		}
		made, err := stores.NewDirectoryStore(*s.Name, r.path(s.Root), *s.Repos, &source, &cache)
		if err != nil {
			t.Fatal(err)
		}
		r.stores[s.ID] = made
	case "make":
		cfg := config.StoreConfig{Name: s.Config.Name, Kind: s.Config.Kind, Root: r.sub(s.Config.Root)}
		if s.Config.Repos != nil {
			cfg.Repos = append([]string{}, *s.Config.Repos...)
		}
		if s.Config.Manifest != nil {
			manifest := r.sub(*s.Config.Manifest)
			cfg.Manifest = &manifest
		}
		dataDir := ""
		if s.DataDir != nil {
			dataDir = r.sub(*s.DataDir)
		}
		made, err := stores.Make(cfg, dataDir)
		want := r.result(s)
		if want.Error != nil {
			r.checkError(label, err, want.Error)
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		r.checkAttrs(label, made, want.attrsJSON)
		r.stores[s.ID] = made
	case "attrs":
		r.checkAttrs(label, r.stores[s.Store], r.result(s).attrsJSON)
	case "entries":
		st := r.stores[s.Store]
		var counts *stores.ScanCounts
		if s.Counts {
			counts = &stores.ScanCounts{}
		}
		got, err := collect(st, counts)
		want := r.result(s)
		if want.Error != nil {
			r.checkError(label, err, want.Error)
		} else if err != nil {
			t.Errorf("%s: %v", label, err)
		}
		r.checkEntries(label, st, got, want.Entries)
		if counts != nil {
			c := want.RawCount
			if c == nil {
				t.Fatalf("%s: no counts in the vector", label)
			}
			if *counts != (stores.ScanCounts{Live: c.Live, Deleted: c.Deleted, Copies: c.Copies, NoBytes: c.NoBytes,
				Unparsable: c.Unparsable}) {
				t.Errorf("%s: counts %+v, want %+v", label, *counts, *c)
			}
		}
	case "index":
		got, err := collect(r.stores[s.Store], nil)
		want := r.result(s)
		if want.Error != nil {
			r.checkError(label, err, want.Error)
			return
		}
		if err != nil {
			t.Errorf("%s: %v", label, err)
			return
		}
		rows := map[string]bool{}
		for _, e := range got {
			if e.Path != "" && !strings.HasPrefix(e.Path, "/") && !strings.HasSuffix(e.Path, "/") {
				rows[e.Repo+"\x00"+e.Path] = true
			}
		}
		if want.Rows == nil || len(rows) != *want.Rows {
			t.Errorf("%s: %d rows, want %v", label, len(rows), want.Rows)
		}
	case "open":
		data, err := readAll(r.stores[s.Store], r.sub(s.Location))
		want := r.result(s)
		if want.Error != nil {
			r.checkError(label, err, want.Error)
			return
		}
		if err != nil || !bytes.Equal(data, unb64(t, *want.B64)) {
			t.Errorf("%s: %q %v, want %q", label, data, err, unb64(t, *want.B64))
		}
	case "open_entries":
		st := r.stores[s.Store]
		got, err := collect(st, nil)
		want := r.result(s).Files
		if err != nil || len(got) != len(want) {
			t.Fatalf("%s: %d entries %v, want %d", label, len(got), err, len(want))
		}
		for i, e := range got {
			data, err := readAll(st, e.Location)
			if err != nil || e.Location != want[i].Location || !bytes.Equal(data, unb64(t, want[i].B64)) {
				t.Errorf("%s: %s %q %v, want %s", label, e.Location, data, err, want[i].Location)
			}
		}
	case "digest":
		got, err := testkit.TreeDigest(r.path(s.Path))
		if err != nil || got != r.result(s).Digest {
			t.Errorf("%s: digest %s %v, want %s", label, got, err, r.result(s).Digest)
		}
	case "unmounted":
		if got := stores.LooksUnmounted(r.stores[s.Store]); got != *r.result(s).Value {
			t.Errorf("%s: LooksUnmounted %v, want %v", label, got, *r.result(s).Value)
		}
	default:
		t.Fatalf("unknown op %s", s.Op)
	}
}

func TestVectorCases(t *testing.T) {
	v := loadVectors(t)
	reference := 0
	for _, c := range v.Cases {
		if strings.HasPrefix(c.Name, "test_") {
			reference++
		}
		t.Run(c.Name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			r := &runner{t: t, base: base, stores: map[string]stores.Store{}, touched: map[string]bool{}}
			for _, s := range c.Steps {
				r.run(s)
			}
		})
	}
	if reference != 25 {
		t.Fatalf("%d reference cases, want 25 (22 test functions, three of them for two kinds)", reference)
	}
}

func TestManifestMediaType(t *testing.T) {
	v := loadVectors(t)
	if len(v.MediaTypes) == 0 {
		t.Fatal("no media type cases")
	}
	for _, c := range v.MediaTypes {
		data := unb64(t, c.Input)
		want := ""
		if c.Expect != nil {
			want = *c.Expect
		}
		if got := stores.ManifestMediaType(data); got != want {
			t.Errorf("ManifestMediaType(%.60q) = %q, want %q", data, got, want)
		}
	}
}

func TestDockerManifestPath(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.ManifestPaths {
		if got := stores.DockerManifestPath(c.Path); got != c.Expect {
			t.Errorf("dockerManifestPath(%q) = %v, want %v", c.Path, got, c.Expect)
		}
	}
}

func TestRepr(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Reprs {
		in := string(unb64(t, c.Input))
		if got := stores.Repr(in); got != c.Expect {
			t.Errorf("repr(%q) = %s, want %s", in, got, c.Expect)
		}
	}
	// A byte that is not UTF-8 is the surrogate surrogateescape gives it.
	if got := stores.Repr("a\xffb"); got != `'a\udcffb'` {
		t.Errorf("repr of a raw byte = %s", got)
	}
}

func TestPurePath(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.PurePaths {
		var got string
		if c.Path != nil {
			got = stores.PurePath(*c.Path)
		} else {
			got = stores.JoinPath(c.Join[0], c.Join[1])
		}
		if got != c.Expect {
			t.Errorf("%v %v = %q, want %q", show(c.Path), c.Join, got, c.Expect)
		}
	}
}

// Native cases, written before the port.

func TestDirectoryStoreWithoutManifestLookup(t *testing.T) {
	saved := stores.OpenManifestLookup
	stores.OpenManifestLookup = nil
	t.Cleanup(func() { stores.OpenManifestLookup = saved })
	root, err := testkit.MakeTree(filepath.Join(t.TempDir(), "tree"), map[string][]byte{"demo-raw/a.txt": []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	manifest, data := filepath.Join(root, "m.tsv"), t.TempDir()
	s, err := stores.Make(config.StoreConfig{Name: "demo", Kind: "directory", Root: root, Manifest: &manifest}, data)
	if err != nil {
		t.Fatal(err)
	}
	_, err = collect(s, nil)
	if !errors.Is(err, stores.ErrNoManifestLookup) {
		t.Fatalf("Entries with a manifest and no lookup = %v, want ErrNoManifestLookup", err)
	}
	if data, err := readAll(s, "demo-raw/a.txt"); err != nil || string(data) != "a" {
		t.Fatalf("Open = %q, %v", data, err)
	}
}

func TestYieldErrorStopsTheWalk(t *testing.T) {
	root, err := testkit.MakeTree(filepath.Join(t.TempDir(), "tree"), map[string][]byte{
		"demo-raw/a.txt": []byte("a"), "demo-raw/b.txt": []byte("b"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := stores.NewDirectoryStore("t", root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop")
	calls := 0
	err = s.Entries(nil, func(stores.Entry) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("Entries = %v after %d calls, want stop after 1", err, calls)
	}
}

// Linux file systems take names that are not UTF-8; APFS does not, so the
// Python reference cannot record these cases on the machine that generates
// the vectors. Python reads such a name with surrogateescape: a directory
// store counts it as unparsable, and a live blob of a file blob store gets
// a location its index cannot encode.
func TestNamesThatAreNotUTF8(t *testing.T) {
	base := t.TempDir()
	probe := filepath.Join(base, "probe-\xff")
	if err := os.WriteFile(probe, nil, 0o666); err != nil {
		t.Skipf("the file system refuses a name that is not UTF-8: %v", err)
	}
	root, err := testkit.MakeTree(filepath.Join(base, "tree"), map[string][]byte{
		"demo-raw/a.txt": []byte("a"), "demo-raw/b\xff.txt": []byte("b"), "demo-\xfe/c.txt": []byte("c"),
		"demo-raw/sub\xfd/d.txt": []byte("d"),
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := stores.NewDirectoryStore("t", root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	counts := &stores.ScanCounts{}
	got, err := collect(d, counts)
	if err != nil || len(got) != 1 || got[0].Path != "a.txt" ||
		*counts != (stores.ScanCounts{Live: 1, Unparsable: 3}) {
		t.Fatalf("directory store: %+v %+v %v", got, *counts, err)
	}

	live := testkit.NewBlobSpec("demo-raw", "x/live.txt")
	blobStore, err := testkit.MakeBlobStore(filepath.Join(base, "blob-store"), []testkit.BlobSpec{live})
	if err != nil {
		t.Fatal(err)
	}
	props := testkit.EncodeProperties(testkit.SpecProperties(testkit.NewBlobSpec("demo-raw", "y/odd.txt")), 1)
	deleted := testkit.NewBlobSpec("demo-raw", "z/gone.txt")
	deleted.Deleted = true
	for name, data := range map[string][]byte{
		"content/vol-01/\xff.properties":    props,
		"content/vol-01/\xff.bytes":         []byte("x"),
		"content/vol-02/\xfe.properties":    testkit.EncodeProperties(testkit.SpecProperties(deleted), 1),
		"content/vol-02/\xfe.bytes":         []byte("x"),
		"content/vol-03/d\xfc/a.properties": testkit.EncodeProperties(testkit.SpecProperties(deleted), 1),
	} {
		target := filepath.Join(blobStore, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	n, err := stores.NewBlobStore("n", blobStore)
	if err != nil {
		t.Fatal(err)
	}
	counts = &stores.ScanCounts{}
	got, err = collect(n, counts)
	var classErr *stores.Error
	if !errors.As(err, &classErr) || classErr.Class != "UnicodeEncodeError" ||
		err.Error() != `'utf-8' codec can't encode character '\udcff' in position 15: surrogates not allowed` {
		t.Fatalf("blob store: error %v, want a UnicodeEncodeError", err)
	}
	if len(got) != 1 || got[0].Path != "x/live.txt" {
		t.Fatalf("blob store: entries before the error %+v", got)
	}
}

func TestOpenRefusesWhatIsNotAFile(t *testing.T) {
	root, err := testkit.MakeTree(filepath.Join(t.TempDir(), "tree"), map[string][]byte{"demo-raw/a.txt": []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := stores.NewDirectoryStore("t", root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, location := range []string{"", "demo-raw", "demo-raw/b.txt", "../tree/demo-raw/b.txt", "a\x00b"} {
		if f, err := s.Open(location); err == nil || !errors.Is(err, fs.ErrNotExist) {
			if f != nil {
				f.Close()
			}
			t.Errorf("Open(%q) = %v, want fs.ErrNotExist", location, err)
		}
	}
}
