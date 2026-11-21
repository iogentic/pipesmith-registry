package blobwrite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// Compaction of a blob with the delete mark (rulings CL4 and CL5, decision
// A8): the properties file goes first, then the bytes file, then the copy
// of the mark; a young mark and a blob without the mark stay.

// markedBlob writes one blob at fixedNow and marks it a day later.
func markedBlob(t *testing.T) (root, location string, w *Writer) {
	t.Helper()
	root = newStore(t)
	now := fixedNow
	w = openTestWriter(t, root, Options{Now: func() time.Time { return now }, NewID: idList(id1, id2)})
	blob, err := w.Write(strings.NewReader("payload"), attributes("demo-raw", "docs/a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	now = fixedNow.Add(24 * time.Hour)
	if err := w.MarkDeleted(blob.Entry.Location, "Removed by a test"); err != nil {
		t.Fatal(err)
	}
	return root, blob.Entry.Location, w
}

func gone(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func TestCompactRemovesAnOldMarkPropertiesFirst(t *testing.T) {
	root, location, w := markedBlob(t)
	bytesFile := filepath.Join(root, filepath.FromSlash(location))
	propsFile := strings.TrimSuffix(bytesFile, stores.BytesSuffix) + stores.PropertiesSuffix
	copyFile := filepath.Join(root, "content/2026/11/19/10/15", id1+stores.PropertiesSuffix)
	if gone(t, copyFile) {
		t.Fatal("the mark wrote no copy")
	}
	mark, ok, err := ReadMark(root, location)
	if err != nil || !ok || mark.Repo != "demo-raw" || mark.Path != "docs/a.txt" || mark.Size != 7 ||
		!mark.At.Equal(fixedNow.Add(24*time.Hour)) {
		t.Fatalf("read mark %+v %v %v", mark, ok, err)
	}
	var between []bool
	compactBetween = func(string) error {
		between = []bool{gone(t, copyFile), gone(t, propsFile), gone(t, bytesFile)}
		return nil
	}
	t.Cleanup(func() { compactBetween = nil })
	got, removed, err := w.Compact(location, fixedNow.Add(25*time.Hour))
	if err != nil || !removed || got != mark {
		t.Fatalf("compact %+v %v %v", got, removed, err)
	}
	if len(between) != 3 || !between[0] || !between[1] || between[2] {
		t.Fatalf("between the unlinks copy, properties, bytes gone: %v", between)
	}
	if !gone(t, bytesFile) || !gone(t, propsFile) || !gone(t, copyFile) {
		t.Fatal("a file of the blob stays")
	}
	if counts, live := scanCounts(t, root); counts != (stores.ScanCounts{}) || len(live) != 0 {
		t.Fatalf("scan %+v %v", counts, live)
	}
}

func TestCompactKeepsAYoungMarkAndALiveBlob(t *testing.T) {
	root, location, w := markedBlob(t)
	before := treeDigest(t, root)
	if _, removed, err := w.Compact(location, fixedNow.Add(24*time.Hour)); err != nil || removed {
		t.Fatalf("a mark as old as the cutoff: %v %v", removed, err)
	}
	if _, removed, err := CompactAt(root, location, fixedNow.Add(time.Hour)); err != nil || removed {
		t.Fatalf("a young mark: %v %v", removed, err)
	}
	if treeDigest(t, root) != before {
		t.Fatal("a young mark changed the store")
	}
	live, err := w.Write(strings.NewReader("live"), attributes("demo-raw", "docs/b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadMark(root, live.Entry.Location); err != nil || ok {
		t.Fatalf("a live blob has a mark: %v %v", ok, err)
	}
	if _, removed, err := CompactAt(root, live.Entry.Location, fixedNow.Add(1000*time.Hour)); err != nil || removed {
		t.Fatalf("a live blob: %v %v", removed, err)
	}
	if counts, _ := scanCounts(t, root); counts != (stores.ScanCounts{Live: 1, Deleted: 1, Copies: 1}) {
		t.Fatalf("scan %+v", counts)
	}
	for _, bad := range []string{"content/tmp/x.bytes", "../x.bytes", "content/2026/11/18/10/15/" + id1 + ".properties"} {
		if _, removed, err := CompactAt(root, bad, fixedNow.Add(1000*time.Hour)); err == nil || removed {
			t.Errorf("%s: %v %v", bad, removed, err)
		}
	}
}

// A kill between the two unlinks leaves the bytes file without its
// properties file: a scan has no error and sees no blob, and a second
// compaction of the location finds nothing to do.
func TestAScanAfterAKillBetweenTheUnlinksHasNoError(t *testing.T) {
	root, location, _ := markedBlob(t)
	compactBetween = func(string) error { return errors.New("killed") }
	t.Cleanup(func() { compactBetween = nil })
	if _, removed, err := CompactAt(root, location, fixedNow.Add(48*time.Hour)); err == nil || removed {
		t.Fatalf("a kill: %v %v", removed, err)
	}
	compactBetween = nil
	s, err := stores.NewBlobStore("scan", root)
	if err != nil {
		t.Fatal(err)
	}
	var counts stores.ScanCounts
	var loose []string
	err = s.WalkLoose(&counts, func(stores.Entry) error { return nil }, nil, func(l string) error { loose = append(loose, l); return nil })
	if err != nil || counts != (stores.ScanCounts{}) || len(loose) != 1 || loose[0] != location {
		t.Fatalf("scan %+v %v %v", counts, loose, err)
	}
	if _, ok, err := ReadMark(root, location); err != nil || ok {
		t.Fatalf("a mark after the kill: %v %v", ok, err)
	}
	if _, removed, err := CompactAt(root, location, fixedNow.Add(48*time.Hour)); err != nil || removed {
		t.Fatalf("a second compaction: %v %v", removed, err)
	}
}
