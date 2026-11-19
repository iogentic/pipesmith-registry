package blobwrite

import (
	"bytes"
	"crypto/md5"  // #nosec G501 -- the MD5 digest the formats publish, not a security use.
	"crypto/sha1" // #nosec G505 -- the SHA-1 of the blob format, not a security use.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// writerVectors is the section writer of testdata/vectors/blobwrite.json.
type writerVectors struct {
	Now   time.Time `json:"now"`
	IDs   []string  `json:"ids"`
	Blobs []struct {
		Repo        string `json:"repo"`
		Path        string `json:"path"`
		Content     string `json:"content"`
		ContentType string `json:"content_type"`
		CreatedBy   string `json:"created_by"`
		CreatedByIP string `json:"created_by_ip"`
	} `json:"blobs"`
	Tree []treeFile `json:"tree"`
}

type treeFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func loadWriterVectors(t *testing.T) *writerVectors {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/blobwrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Writer writerVectors `json:"writer"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Writer.Blobs) != 3 || len(v.Writer.Tree) != 7 {
		t.Fatalf("writer vectors: %d blobs, %d files", len(v.Writer.Blobs), len(v.Writer.Tree))
	}
	return &v.Writer
}

var fixedNow = time.Date(2026, 11, 18, 10, 15, 30, 123_000_000, time.UTC)

const (
	id1 = "00000000-0000-4000-8000-000000000001"
	id2 = "00000000-0000-4000-8000-000000000002"
	id3 = "00000000-0000-4000-8000-000000000003"
)

// idList hands out the ids in order, then the last one again.
func idList(ids ...string) func() string {
	var mu sync.Mutex
	next := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		id := ids[min(next, len(ids)-1)]
		next++
		return id
	}
}

func fixedClock() time.Time { return fixedNow }

// layout makes the directories of an empty file blob store.
func layout(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	for _, dir := range []string{"content/tmp", "reconciliation"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func openTestWriter(t *testing.T, root string, opts Options) *Writer {
	t.Helper()
	s, err := stores.NewWriteStore("demo-write", root)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(s, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// files lists every regular file below root, relative and sorted.
func files(t *testing.T, root string) []treeFile {
	t.Helper()
	var out []treeFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out = append(out, treeFile{Path: filepath.ToSlash(rel), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(out, func(a, b treeFile) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func attributes(repo, path string) stores.Attributes {
	return stores.Attributes{Repo: repo, Path: path, ContentType: "text/plain", CreatedBy: "demo-user", CreatedByIP: "192.0.2.10"}
}

func digestsOf(data []byte) stores.Digests {
	m := md5.Sum(data)   // #nosec G401 -- see the import.
	s1 := sha1.Sum(data) // #nosec G401 -- see the import.
	s256 := sha256.Sum256(data)
	s512 := sha512.Sum512(data)
	return stores.Digests{MD5: hex.EncodeToString(m[:]), SHA1: hex.EncodeToString(s1[:]),
		SHA256: hex.EncodeToString(s256[:]), SHA512: hex.EncodeToString(s512[:])}
}

// Three blobs at a fixed clock and fixed ids give the golden tree, and a
// scan of the store reads them back.
func TestWriterGoldenTree(t *testing.T) {
	v := loadWriterVectors(t)
	if !v.Now.Equal(fixedNow) {
		t.Fatalf("vector clock %s", v.Now)
	}
	root := layout(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(v.IDs...)})
	var written []stores.Written
	for i, b := range v.Blobs {
		a := stores.Attributes{Repo: b.Repo, Path: b.Path, ContentType: b.ContentType, CreatedBy: b.CreatedBy, CreatedByIP: b.CreatedByIP}
		got, err := w.Write(strings.NewReader(b.Content), a)
		if err != nil {
			t.Fatal(err)
		}
		content := []byte(b.Content)
		sha := digestsOf(content).SHA1
		contentType := b.ContentType
		want := stores.Written{ID: v.IDs[i], Digests: digestsOf(content), Entry: stores.Entry{Repo: b.Repo, Path: b.Path,
			Size: int64(len(content)), SHA1: &sha, ContentType: &contentType, CreatedMS: fixedNow.UnixMilli(),
			Location: "content/2026/11/18/10/15/" + v.IDs[i] + ".bytes"}}
		if !equalWritten(got, want) {
			t.Fatalf("blob %d: %+v, want %+v", i, got, want)
		}
		written = append(written, got)
	}
	if got := files(t, root); !slices.Equal(got, v.Tree) {
		t.Fatalf("tree\n%+v\nwant\n%+v", got, v.Tree)
	}
	s, err := stores.NewBlobStore("demo-write", root)
	if err != nil {
		t.Fatal(err)
	}
	var counts stores.ScanCounts
	var scanned []stores.Entry
	if err := s.Entries(&counts, func(e stores.Entry) error { scanned = append(scanned, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if counts != (stores.ScanCounts{Live: 3}) || len(scanned) != 3 {
		t.Fatalf("scan counts %+v, %d entries", counts, len(scanned))
	}
	for i := range scanned {
		if !equalEntry(scanned[i], written[i].Entry) {
			t.Fatalf("scanned %+v, written %+v", scanned[i], written[i].Entry)
		}
	}
}

func equalEntry(a, b stores.Entry) bool {
	same := func(x, y *string) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return a.Repo == b.Repo && a.Path == b.Path && a.Size == b.Size && same(a.SHA1, b.SHA1) &&
		same(a.ContentType, b.ContentType) && a.CreatedMS == b.CreatedMS && a.Location == b.Location
}

func equalWritten(a, b stores.Written) bool {
	return a.ID == b.ID && a.Digests == b.Digests && equalEntry(a.Entry, b.Entry)
}

// After every step: the properties file of the blob never exists without
// its bytes file, and the steps run in their order.
func TestWriterBytesBeforeProperties(t *testing.T) {
	root := layout(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1)})
	stem := filepath.Join(root, "content/2026/11/18/10/15", id1)
	var seen []Step
	w.after = func(step Step) {
		seen = append(seen, step)
		_, propsErr := os.Stat(stem + ".properties")
		_, bytesErr := os.Stat(stem + ".bytes")
		if propsErr == nil && bytesErr != nil {
			t.Errorf("after %s the properties exist without the bytes", step)
		}
		if step >= StepRenameBytes && bytesErr != nil {
			t.Errorf("after %s no bytes file: %v", step, bytesErr)
		}
		if step >= StepRenameProperties && propsErr != nil {
			t.Errorf("after %s no properties file: %v", step, propsErr)
		}
		if step < StepRenameProperties && propsErr == nil {
			t.Errorf("after %s the properties exist already", step)
		}
	}
	if _, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "a/b.txt")); err != nil {
		t.Fatal(err)
	}
	want := []Step{StepCreate, StepStream, StepSyncBytes, StepRenameBytes, StepProperties, StepRenameProperties, StepLog}
	if !slices.Equal(seen, want) {
		t.Fatalf("steps %v, want %v", seen, want)
	}
}

// An error at any of the 7 steps leaves no file outside content/tmp and
// none inside, and the writer goes on writing.
func TestWriterFaultAtEachStep(t *testing.T) {
	for step := StepCreate; step <= StepLog; step++ {
		root := layout(t)
		w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1, id2)})
		injected := errors.New("injected at " + step.String())
		w.fault = func(s Step) error {
			if s == step {
				return injected
			}
			return nil
		}
		_, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "a/b.txt"))
		if !errors.Is(err, injected) {
			t.Fatalf("%s: error %v", step, err)
		}
		if left := files(t, root); len(left) != 0 {
			t.Fatalf("%s: files left %+v", step, left)
		}
		w.fault = nil
		got, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "a/b.txt"))
		if err != nil {
			t.Fatalf("%s: the next write: %v", step, err)
		}
		if left := files(t, filepath.Join(root, "content", "tmp")); len(left) != 0 {
			t.Fatalf("%s: work files after the next write %+v", step, left)
		}
		if got.ID != id1 && got.ID != id2 {
			t.Fatalf("%s: id %s", step, got.ID)
		}
	}
}

// An id whose bytes file exists already gives way to the next one; eight
// collisions in a row fail the write and leave no file.
func TestWriterIDCollision(t *testing.T) {
	taken := func() (root, path string) {
		root = layout(t)
		path = filepath.Join(root, "content/2026/11/18/10/15", id1+".bytes")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("taken"), 0o600); err != nil {
			t.Fatal(err)
		}
		return root, path
	}
	root, path := taken()
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1, id2)})
	got, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "a/b.txt"))
	if err != nil || got.ID != id2 {
		t.Fatalf("%+v %v", got, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "taken" {
		t.Fatalf("the taken file: %q %v", data, err)
	}
	root, _ = taken()
	before := files(t, root)
	drawn := 0
	stuck := openTestWriter(t, root, Options{Now: fixedClock, NewID: func() string { drawn++; return id1 }})
	if _, err := stuck.Write(strings.NewReader("payload"), attributes("demo-raw", "c.txt")); err == nil {
		t.Fatal("no error after eight collisions")
	}
	if drawn != 8 {
		t.Fatalf("%d ids drawn, want 8", drawn)
	}
	if after := files(t, root); !slices.Equal(before, after) {
		t.Fatalf("files changed: %+v", after)
	}
}

// The line of the reconciliation log, with the real clock and real ids.
func TestReconciliationLine(t *testing.T) {
	root := layout(t)
	w := openTestWriter(t, root, Options{})
	before := time.Now().UTC()
	got, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(got.ID) {
		t.Fatalf("id %q is no random lower case UUID", got.ID)
	}
	day := before.Format("2006-01-02")
	data, err := os.ReadFile(filepath.Join(root, "reconciliation", day))
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`^(\d{4}-\d\d-\d\d) \d\d:\d\d:\d\d,([0-9a-f-]{36}),true\n$`)
	m := line.FindStringSubmatch(string(data))
	if m == nil || m[1] != day || m[2] != got.ID {
		t.Fatalf("log %q", data)
	}
	if !strings.HasPrefix(got.Entry.Location, "content/"+before.Format("2006/01/02/15/")) {
		t.Fatalf("location %s for %s", got.Entry.Location, before)
	}
}

// A body above the limit is refused and leaves no file; the four digests
// are those of the body; a discarded upload leaves nothing.
func TestUpload(t *testing.T) {
	root := layout(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1, id2, id3)})
	if _, err := w.Upload(strings.NewReader("12345"), 4); !errors.Is(err, stores.ErrTooLarge) {
		t.Fatalf("error %v, want ErrTooLarge", err)
	}
	if left := files(t, root); len(left) != 0 {
		t.Fatalf("files left %+v", left)
	}
	body := bytes.Repeat([]byte("0123456789abcdef"), 70_000)
	u, err := w.Upload(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if u.Size() != int64(len(body)) || u.Digests() != digestsOf(body) {
		t.Fatalf("size %d, digests %+v", u.Size(), u.Digests())
	}
	if err := u.Discard(); err != nil {
		t.Fatal(err)
	}
	if left := files(t, root); len(left) != 0 {
		t.Fatalf("files left after discard %+v", left)
	}
	if _, err := u.Commit(attributes("demo-raw", "x.txt")); err == nil {
		t.Fatal("commit after discard")
	}
	u, err = w.Upload(strings.NewReader("abc"), -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []stores.Attributes{{Path: "x", CreatedBy: "u"}, {Repo: "r", CreatedBy: "u"}, {Repo: "r", Path: "x"}} {
		if _, err := u.Commit(a); err == nil {
			t.Fatalf("commit of %+v", a)
		}
	}
	a := attributes("demo-raw", "x.txt")
	a.NewerThan = fixedNow.UnixMilli() + 5000
	got, err := u.Commit(a)
	if err != nil || got.Entry.CreatedMS != a.NewerThan+1 {
		t.Fatalf("%+v %v", got, err)
	}
	if err := u.Discard(); err != nil {
		t.Fatalf("discard after commit: %v", err)
	}
}
