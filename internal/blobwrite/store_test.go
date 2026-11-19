package blobwrite

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// newStore is an empty store made by Create.
func newStore(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	if err := Create(root, fixedNow); err != nil {
		t.Fatal(err)
	}
	return root
}

func scanCounts(t *testing.T, root string) (stores.ScanCounts, []stores.Entry) {
	t.Helper()
	s, err := stores.NewBlobStore("scan", root)
	if err != nil {
		t.Fatal(err)
	}
	var counts stores.ScanCounts
	var live []stores.Entry
	if err := s.Entries(&counts, func(e stores.Entry) error { live = append(live, e); return nil }); err != nil {
		t.Fatal(err)
	}
	return counts, live
}

func treeDigest(t *testing.T, root string) string {
	t.Helper()
	digest, err := testkit.TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func readProperties(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	items, err := blobprops.Load(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// A new store has the layout of the old product and no live entry; a
// second Create changes nothing.
func TestCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stores", "local-write")
	if err := Create(root, fixedNow); err != nil {
		t.Fatal(err)
	}
	want, _ := Properties(map[string]string{"type": "file/1"}, fixedNow)
	if got, err := os.ReadFile(filepath.Join(root, "metadata.properties")); err != nil || string(got) != string(want) {
		t.Fatalf("metadata.properties %q %v", got, err)
	}
	for _, dir := range []string{"content", "content/tmp", "reconciliation"} {
		if info, err := os.Stat(filepath.Join(root, dir)); err != nil || !info.IsDir() {
			t.Fatalf("%s: %v", dir, err)
		}
	}
	if counts, live := scanCounts(t, root); counts != (stores.ScanCounts{}) || len(live) != 0 {
		t.Fatalf("%+v %v", counts, live)
	}
	before := treeDigest(t, root)
	if err := Create(root, fixedNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if treeDigest(t, root) != before {
		t.Fatal("a second Create changed the store")
	}
}

// A store without the writable flag is never written: Open refuses it
// before it touches a file.
func TestOpenRefusesAStoreWithoutTheWritableFlag(t *testing.T) {
	root, err := testkit.MakeBlobStore(filepath.Join(t.TempDir(), "old"), testkit.SkipCaseBlobs())
	if err != nil {
		t.Fatal(err)
	}
	before := treeDigest(t, root)
	s, err := stores.NewBlobStore("old", root)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := Open(s, Options{}); err == nil {
		w.Close()
		t.Fatal("Open wrote access to a store of the old product")
	}
	if treeDigest(t, root) != before {
		t.Fatal("the refused Open changed the store")
	}
	made, err := stores.Make(config.StoreConfig{Name: "old", Kind: stores.KindBlobStore, Root: root}, "")
	if err != nil {
		t.Fatal(err)
	}
	if stores.Writable(made) {
		t.Fatal("a store of config.json carries the writable flag")
	}
}

// One writer per store: the lock on content/tmp refuses a second one until
// the first closes.
func TestSecondWriterFails(t *testing.T) {
	root := newStore(t)
	first := openTestWriter(t, root, Options{})
	s, err := stores.NewWriteStore("local-write", root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(s, Options{}); err == nil {
		second.Close()
		t.Fatal("a second writer started on the same store")
	} else if !strings.Contains(err.Error(), "another writer") {
		t.Fatalf("error %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	third, err := Open(s, Options{})
	if err != nil {
		t.Fatalf("after the close: %v", err)
	}
	third.Close()
}

// Open removes the work files a stopped writer left in content/tmp, and
// nothing else.
func TestOpenRemovesWorkFiles(t *testing.T) {
	root := newStore(t)
	work := filepath.Join(root, "content", "tmp", "tmp$"+id1+"."+id2)
	for _, name := range []string{work + ".bytes", work + ".properties", filepath.Join(root, "content", "tmp", "keep.txt")} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	openTestWriter(t, root, Options{})
	left := files(t, filepath.Join(root, "content", "tmp"))
	if len(left) != 1 || left[0].Path != "keep.txt" {
		t.Fatalf("content/tmp holds %+v", left)
	}
}

// A delete adds the four keys and a copy of the properties in the
// directory of the current minute, and removes no file; the scan then
// counts one deleted blob and one copy. A second delete changes nothing.
func TestMarkDeleted(t *testing.T) {
	root := newStore(t)
	now := fixedNow
	w := openTestWriter(t, root, Options{Now: func() time.Time { return now }, NewID: idList(id1)})
	blob, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "docs/a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	original := readProperties(t, filepath.Join(root, "content/2026/11/18/10/15", id1+".properties"))
	before := files(t, root)
	now = fixedNow.Add(26 * time.Hour)
	if err := w.MarkDeleted(blob.Entry.Location, "Removed by a test"); err != nil {
		t.Fatal(err)
	}
	marked := readProperties(t, filepath.Join(root, "content/2026/11/18/10/15", id1+".properties"))
	want := map[string]string{"deleted": "true", "deletedReason": "Removed by a test",
		"deletedDateTime": "1795090530123", "softDeletedLocation": "2026-11-19T12:15"}
	for key, value := range original {
		want[key] = value
	}
	if !mapsEqual(marked, want) {
		t.Fatalf("marked %q\nwant %q", marked, want)
	}
	copied := readProperties(t, filepath.Join(root, "content/2026/11/19/12/15", id1+".properties"))
	want = map[string]string{"originalLocation": "2026-11-18T10:15"}
	for key, value := range original {
		want[key] = value
	}
	if !mapsEqual(copied, want) {
		t.Fatalf("copy %q\nwant %q", copied, want)
	}
	after := files(t, root)
	for _, f := range before {
		if !slices.ContainsFunc(after, func(g treeFile) bool { return g.Path == f.Path }) {
			t.Fatalf("%s is gone", f.Path)
		}
		if strings.HasSuffix(f.Path, ".bytes") && !slices.Contains(after, f) {
			t.Fatalf("%s changed", f.Path)
		}
	}
	if counts, live := scanCounts(t, root); counts != (stores.ScanCounts{Deleted: 1, Copies: 1}) || len(live) != 0 {
		t.Fatalf("scan %+v %v", counts, live)
	}
	marked1 := treeDigest(t, root)
	now = now.Add(time.Hour)
	if err := w.MarkDeleted(blob.Entry.Location, "again"); err != nil {
		t.Fatal(err)
	}
	if treeDigest(t, root) != marked1 {
		t.Fatal("a second delete changed the store")
	}
	if left := files(t, filepath.Join(root, "content", "tmp")); len(left) != 0 {
		t.Fatalf("work files %+v", left)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// A delete in the minute of the blob writes no copy; a blob of the old
// directory scheme gets a copy with an empty originalLocation.
func TestMarkDeletedCopyRules(t *testing.T) {
	root := newStore(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1)})
	blob, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.MarkDeleted(blob.Entry.Location, "same minute"); err != nil {
		t.Fatal(err)
	}
	if counts, _ := scanCounts(t, root); counts != (stores.ScanCounts{Deleted: 1}) {
		t.Fatalf("scan %+v", counts)
	}

	old := testkit.NewBlobSpec("demo-releases", "com/example/old/1.0/old-1.0.jar")
	old.Scheme, old.Content = "vol", []byte("old bytes")
	oldRoot, err := testkit.MakeBlobStore(filepath.Join(t.TempDir(), "taken"), []testkit.BlobSpec{old})
	if err != nil {
		t.Fatal(err)
	}
	ow := openTestWriter(t, oldRoot, Options{Now: fixedClock})
	if err := ow.MarkDeleted(testkit.BytesLocation(old), "old scheme"); err != nil {
		t.Fatal(err)
	}
	copied := readProperties(t, filepath.Join(oldRoot, "content/2026/11/18/10/15", testkit.BlobUUID(old)+".properties"))
	if value, ok := copied["originalLocation"]; !ok || value != "" {
		t.Fatalf("copy %q", copied)
	}
	if counts, _ := scanCounts(t, oldRoot); counts != (stores.ScanCounts{Deleted: 1, Copies: 1}) {
		t.Fatalf("scan %+v", counts)
	}
}

// Only the bytes file of a blob of the store can be marked.
func TestMarkDeletedRefusesOtherLocations(t *testing.T) {
	root := newStore(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1)})
	blob, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	before := treeDigest(t, root)
	for _, location := range []string{"", "../outside.bytes", "content/../../outside.bytes", "/etc/hosts.bytes",
		strings.TrimSuffix(blob.Entry.Location, ".bytes") + ".properties", "content/tmp/tmp$" + id1 + "." + id2 + ".bytes",
		"content/2026/11/18/10/15/" + id2 + ".bytes", "metadata.bytes"} {
		if err := w.MarkDeleted(location, "no"); err == nil {
			t.Errorf("%q: no error", location)
		}
	}
	if treeDigest(t, root) != before {
		t.Fatal("a refused delete changed the store")
	}
}

// A link is a second blob of the same bytes: one inode, its own properties.
func TestLink(t *testing.T) {
	root := newStore(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1, id2)})
	first, err := w.Write(strings.NewReader("manifest"), attributes("demo-docker", "v2/app/manifests/1.0"))
	if err != nil {
		t.Fatal(err)
	}
	a := attributes("demo-docker", "v2/app/manifests/sha256:0123")
	second, err := w.Link(first.Entry.Location, a)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != id2 || second.Entry.Path != a.Path || second.Entry.Size != first.Entry.Size ||
		*second.Entry.SHA1 != *first.Entry.SHA1 || second.Digests != (stores.Digests{SHA1: first.Digests.SHA1}) {
		t.Fatalf("%+v", second)
	}
	one, err := os.Stat(filepath.Join(root, first.Entry.Location))
	if err != nil {
		t.Fatal(err)
	}
	two, err := os.Stat(filepath.Join(root, second.Entry.Location))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(one, two) {
		t.Fatal("the link is a copy")
	}
	counts, live := scanCounts(t, root)
	if counts != (stores.ScanCounts{Live: 2}) || !equalEntry(live[1], second.Entry) {
		t.Fatalf("%+v %+v", counts, live)
	}
	if _, err := w.Link("content/2026/11/18/10/15/"+id3+".bytes", a); err == nil {
		t.Fatal("a link of a missing blob")
	}
}

// The id is checked again at the commit: a bytes file that appeared since
// the upload gives way to a new id.
func TestCommitChecksTheIDAgain(t *testing.T) {
	root := newStore(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1, id2)})
	u, err := w.Upload(strings.NewReader("payload"), -1)
	if err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(root, "content/2026/11/18/10/15", id1+".bytes")
	if err := os.MkdirAll(filepath.Dir(planted), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planted, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := u.Commit(attributes("demo-raw", "a.txt"))
	if err != nil || got.ID != id2 {
		t.Fatalf("%+v %v", got, err)
	}
	if data, err := os.ReadFile(planted); err != nil || string(data) != "planted" {
		t.Fatalf("%q %v", data, err)
	}
}
