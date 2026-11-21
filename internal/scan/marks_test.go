package scan_test

import (
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Ruling SR14 of leg 6-stores-b: a scan of a blob store writes the count of
// its marked blobs and their bytes into store_marks of catalog.sqlite, equal
// to a walk of its marked blobs.
func TestAScanWritesTheMarkedCount(t *testing.T) {
	data := t.TempDir()
	live := testkit.NewBlobSpec("demo-raw", "a.txt")
	gone := testkit.NewBlobSpec("demo-raw", "b.txt")
	gone.Content, gone.Deleted = []byte("seven b"), true
	gone2 := testkit.NewBlobSpec("demo-raw", "c.txt")
	gone2.Content, gone2.Deleted = []byte("abc"), true
	root, err := testkit.MakeBlobStore(filepath.Join(data, "store-a"), []testkit.BlobSpec{live, gone, gone2})
	if err != nil {
		t.Fatal(err)
	}
	s, err := stores.NewBlobStore("store-a", root)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	result, err := scan.Run(s, ix, nil)
	if err != nil || result.Deleted != 2 || result.DeletedBytes != 10 {
		t.Fatalf("%+v %v", result, err)
	}
	c, err := catalog.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var count, size int64
	if err := s.Walk(nil, func(stores.Entry) error { return nil }, func(m stores.Marked) error {
		count++
		size += m.Size
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.StoreMarks("store-a")
	if err != nil || !found || got.Blobs != count || got.Bytes != size || count != 2 || got.Counted.IsZero() {
		t.Fatalf("row %+v %v %v, recount %d %d", got, found, err, count, size)
	}
}
