package scan_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The vectors come from tools/vectors/scan.py, which runs the steps of each
// case against scan_store and the index of the Python reference.
type vectorFile struct {
	ProgressEvery int          `json:"progress_every"`
	HintOrder     []string     `json:"hint_order"`
	Cases         []vectorCase `json:"cases"`
	GuessFormat   []struct {
		Path   string  `json:"path"`
		Expect *string `json:"expect"`
	} `json:"guess_format"`
}

type vectorCase struct {
	Name  string `json:"name"`
	Steps []step `json:"steps"`
}

type step struct {
	Op       string            `json:"op"`
	Root     string            `json:"root"`
	Path     string            `json:"path"`
	Src      string            `json:"src"`
	Dst      string            `json:"dst"`
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Store    string            `json:"store"`
	Repo     string            `json:"repo"`
	B64      string            `json:"b64"`
	Seconds  int64             `json:"seconds"`
	Progress bool              `json:"progress"`
	Specs    []specJSON        `json:"specs"`
	Files    map[string]string `json:"files"`
	Repos    *[]string         `json:"repos"`
	Result   json.RawMessage   `json:"result"`
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

type assetJSON struct {
	Repo        string  `json:"repo"`
	Path        string  `json:"path"`
	Store       string  `json:"store"`
	Location    string  `json:"location"`
	Size        int64   `json:"size"`
	SHA1        *string `json:"sha1"`
	ContentType *string `json:"content_type"`
	CreatedMS   int64   `json:"created_ms"`
}

type result struct {
	Error *struct {
		Class   string `json:"class"`
		Message string `json:"message"`
	} `json:"error"`
	Store      string      `json:"store"`
	Live       int         `json:"live"`
	Deleted    int         `json:"deleted"`
	Copies     int         `json:"copies"`
	NoBytes    int         `json:"no_bytes"`
	Unparsable int         `json:"unparsable"`
	Hints      [][2]string `json:"hints"`
	Progress   []int       `json:"progress"`
	Asset      *assetJSON  `json:"asset"`
	RawStats   [][3]any    `json:"stats"`
	Files      *int64      `json:"files"`
	Location   string      `json:"location"`
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/scan.json")
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
	t      *testing.T
	base   string
	ix     *index.Index
	stores map[string]stores.Store
}

func (r *runner) path(rel string) string {
	return filepath.Join(r.base, rel)
}

func (r *runner) result(s step) *result {
	r.t.Helper()
	var out result
	if err := json.Unmarshal(s.Result, &out); err != nil {
		r.t.Fatal(err)
	}
	return &out
}

func (r *runner) run(s step) {
	t := r.t
	t.Helper()
	label := s.Op + " " + s.Store + " " + s.Repo + " " + s.Path
	switch s.Op {
	case "index":
		ix, err := index.Open(filepath.Join(r.base, "index.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ix.Close() })
		r.ix = ix
	case "blob_store":
		specs := make([]testkit.BlobSpec, len(s.Specs))
		for i, p := range s.Specs {
			specs[i] = testkit.BlobSpec{Repo: p.Repo, Path: p.Path, Content: unb64(t, p.Content), Scheme: p.Scheme,
				CreatedMS: p.CreatedMS, Deleted: p.Deleted, CopyOnly: p.CopyOnly, NoBytes: p.NoBytes,
				ContentType: p.ContentType, LeadingSlash: p.LeadingSlash}
		}
		if _, err := testkit.MakeBlobStore(r.path(s.Root), specs); err != nil {
			t.Fatal(err)
		}
	case "unparsable":
		if got, err := testkit.AddUnparsable(r.path(s.Root), testkit.DefaultUnparsable); err != nil ||
			got != r.result(s).Location {
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
	case "touch_all":
		when := time.Unix(s.Seconds, 0)
		err := filepath.WalkDir(r.path(s.Root), func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			return os.Chtimes(path, when, when)
		})
		if err != nil {
			t.Fatal(err)
		}
	case "write":
		target := r.path(s.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, unb64(t, s.B64), 0o666); err != nil {
			t.Fatal(err)
		}
	case "mkdir":
		if err := os.MkdirAll(r.path(s.Path), 0o777); err != nil {
			t.Fatal(err)
		}
	case "unlink":
		if err := os.Remove(r.path(s.Path)); err != nil {
			t.Fatal(err)
		}
	case "rename":
		if err := os.Rename(r.path(s.Src), r.path(s.Dst)); err != nil {
			t.Fatal(err)
		}
	case "move_children":
		children, err := os.ReadDir(r.path(s.Src))
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range children {
			if err := os.Rename(filepath.Join(r.path(s.Src), child.Name()), filepath.Join(r.path(s.Dst), child.Name())); err != nil {
				t.Fatal(err)
			}
		}
	case "store":
		var made stores.Store
		var err error
		if s.Kind == "nexus-file" {
			made, err = stores.NewBlobStore(s.Name, r.path(s.Root))
		} else {
			made, err = stores.NewDirectoryStore(s.Name, r.path(s.Root), nil, nil, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		r.stores[s.ID] = made
	case "scan":
		var calls []int
		var progress func(int)
		if s.Progress {
			progress = func(live int) { calls = append(calls, live) }
		}
		got, err := scan.Run(r.stores[s.Store], r.ix, progress)
		want := r.result(s)
		if !slices.Equal(calls, want.Progress) && !(len(calls) == 0 && len(want.Progress) == 0) {
			t.Errorf("%s: progress %v, want %v", label, calls, want.Progress)
		}
		if want.Error != nil {
			r.checkError(label, err, want.Error.Class, want.Error.Message)
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got.Store != want.Store || got.Live != want.Live || got.Deleted != want.Deleted || got.Copies != want.Copies ||
			got.NoBytes != want.NoBytes || got.Unparsable != want.Unparsable || got.Seconds < 0 {
			t.Errorf("%s: result %+v, want %+v", label, got, *want)
		}
		if len(got.Hints) != len(want.Hints) {
			t.Errorf("%s: hints %v, want %v", label, got.Hints, want.Hints)
		}
		for _, pair := range want.Hints {
			if got.Hints[pair[0]] != pair[1] {
				t.Errorf("%s: hints %v, want %v", label, got.Hints, want.Hints)
			}
		}
	case "lookup":
		got, err := r.ix.Lookup(s.Repo, s.Path)
		if err != nil {
			t.Fatal(err)
		}
		want := r.result(s).Asset
		if (got == nil) != (want == nil) {
			t.Errorf("%s: asset %+v, want %+v", label, got, want)
			return
		}
		if got != nil && (got.Repo != want.Repo || got.Path != want.Path || got.Store != want.Store ||
			got.Location != want.Location || got.Size != want.Size || show(got.SHA1) != show(want.SHA1) ||
			show(got.ContentType) != show(want.ContentType) || got.CreatedMS != want.CreatedMS) {
			t.Errorf("%s: asset %+v, want %+v", label, *got, *want)
		}
	case "store_stats":
		stats, err := r.ix.StoreStats()
		if err != nil {
			t.Fatal(err)
		}
		want := r.result(s).RawStats
		if len(stats) != len(want) {
			t.Fatalf("%s: %+v, want %v", label, stats, want)
		}
		for i, w := range want {
			if stats[i].Store != w[0].(string) || stats[i].Files != int64(w[1].(float64)) ||
				stats[i].Bytes != int64(w[2].(float64)) {
				t.Errorf("%s: %+v, want %v", label, stats[i], w)
			}
		}
	case "repo_files":
		repoStats, err := r.ix.RepoStats()
		if err != nil {
			t.Fatal(err)
		}
		var files int64
		for _, st := range repoStats {
			files += st.Files
		}
		if files != *r.result(s).Files {
			t.Errorf("%s: %d files, want %d", label, files, *r.result(s).Files)
		}
	case "hints":
		hints, err := r.ix.Hints()
		if err != nil {
			t.Fatal(err)
		}
		want := r.result(s).Hints
		if len(hints) != len(want) {
			t.Errorf("%s: %v, want %v", label, hints, want)
		}
		for _, pair := range want {
			if hints[pair[0]] != pair[1] {
				t.Errorf("%s: %v, want %v", label, hints, want)
			}
		}
	default:
		t.Fatalf("unknown op %s", s.Op)
	}
}

func (r *runner) checkError(label string, err error, class, message string) {
	r.t.Helper()
	if err == nil {
		r.t.Errorf("%s: no error, want %s %q", label, class, message)
		return
	}
	message = strings.ReplaceAll(message, "<base>", r.base)
	if err.Error() != message {
		r.t.Errorf("%s: error %q, want %q", label, err.Error(), message)
	}
	var classErr *stores.Error
	var valueErr *blobprops.ValueError
	switch {
	case class == "FileNotFoundError":
		if !errors.Is(err, fs.ErrNotExist) {
			r.t.Errorf("%s: %v is not fs.ErrNotExist", label, err)
		}
	case errors.As(err, &classErr):
		if classErr.Class != class {
			r.t.Errorf("%s: class %s, want %s", label, classErr.Class, class)
		}
	case errors.As(err, &valueErr):
		if class != "ValueError" {
			r.t.Errorf("%s: a ValueError, want %s", label, class)
		}
	default:
		r.t.Errorf("%s: error %T, want %s", label, err, class)
	}
}

func TestVectorCases(t *testing.T) {
	v := loadVectors(t)
	if v.ProgressEvery != scan.ProgressEvery || !slices.Equal(v.HintOrder, scan.HintOrder[:]) {
		t.Fatalf("progress every %d and hint order %v, want %d %v", scan.ProgressEvery, scan.HintOrder,
			v.ProgressEvery, v.HintOrder)
	}
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
			r := &runner{t: t, base: base, stores: map[string]stores.Store{}}
			for _, s := range c.Steps {
				r.run(s)
			}
		})
	}
	if reference != 14 {
		t.Fatalf("%d reference cases, want 14 (13 test functions, one of them for two kinds)", reference)
	}
}

func TestGuessFormat(t *testing.T) {
	v := loadVectors(t)
	if len(v.GuessFormat) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range v.GuessFormat {
		if got := scan.GuessFormat(c.Path); got != show(c.Expect) && !(got == "" && c.Expect == nil) {
			t.Errorf("GuessFormat(%q) = %q, want %s", c.Path, got, show(c.Expect))
		}
	}
}

// Native cases, written before the port.

// fakeStore yields fixed entries and counts each as live.
type fakeStore struct {
	root    string
	entries []stores.Entry
	err     error
}

func (f *fakeStore) Name() string { return "fake" }
func (f *fakeStore) Kind() string { return stores.KindDirectory }
func (f *fakeStore) Root() string { return f.root }
func (f *fakeStore) Open(string) (*os.File, error) {
	return nil, fs.ErrNotExist
}
func (f *fakeStore) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	for _, e := range f.entries {
		counts.Live++
		if err := yield(e); err != nil {
			return err
		}
	}
	return f.err
}

func TestWalkErrorAfterEntriesKeepsTheRows(t *testing.T) {
	ix, err := index.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	root := t.TempDir()
	one := stores.Entry{Repo: "demo-raw", Path: "a.txt", Size: 1, CreatedMS: 1, Location: "demo-raw/a.txt"}
	if _, err := scan.Run(&fakeStore{root: root, entries: []stores.Entry{one}}, ix, nil); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("disk went away")
	two := one
	two.Path = "b.txt"
	_, err = scan.Run(&fakeStore{root: root, entries: []stores.Entry{two}, err: broken}, ix, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("Run = %v, want the walk error", err)
	}
	if asset, err := ix.Lookup("demo-raw", "a.txt"); err != nil || asset == nil {
		t.Fatalf("the old row is gone: %v %v", asset, err)
	}
	if asset, err := ix.Lookup("demo-raw", "b.txt"); err != nil || asset != nil {
		t.Fatalf("the new row is there: %v %v", asset, err)
	}
}

func TestHintRanks(t *testing.T) {
	ix, err := index.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var entries []stores.Entry
	for _, p := range [][2]string{
		{"demo-a", "a.pom"}, {"demo-a", "pkg/-/pkg-1.tgz"}, {"demo-b", "v2/x"}, {"demo-b", "a.pom"},
		{"demo-c", "readme.txt"}, {"demo-d", "pkg/-/pkg-1.tgz"}, {"demo-d", "v2/y"},
	} {
		entries = append(entries, stores.Entry{Repo: p[0], Path: p[1], Size: 1, CreatedMS: 1, Location: p[0] + "/" + p[1]})
	}
	got, err := scan.Run(&fakeStore{root: t.TempDir(), entries: entries}, ix, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"demo-a": "npm", "demo-b": "docker", "demo-d": "docker"}
	if len(got.Hints) != len(want) || got.Hints["demo-a"] != "npm" || got.Hints["demo-b"] != "docker" ||
		got.Hints["demo-d"] != "docker" {
		t.Fatalf("hints %v, want %v", got.Hints, want)
	}
}
