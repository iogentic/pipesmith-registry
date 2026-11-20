package catalog_test

import (
	"strings"
	"testing"
)

// Detail gives the side data of the blob a store holds for a path, for the
// REST asset answer: uploader, address, the digests a write computed and
// the last modified time of the asset while that blob wins. A blob of
// another creation time than the one asked for, and a path the catalog does
// not hold, give nil.
func TestDetailOfAWrittenBlob(t *testing.T) {
	cat := open(t, t.TempDir())
	s := writtenSpecs[0]
	put(t, cat, "write", s)
	d, err := cat.Detail(s.Repo, s.Path, "write", s.CreatedMS)
	if err != nil || d == nil {
		t.Fatalf("%+v %v", d, err)
	}
	if d.Uploader == nil || *d.Uploader != "demo-user" || d.Address == nil || *d.Address != "192.0.2.10" {
		t.Fatalf("uploader %v address %v", d.Uploader, d.Address)
	}
	if d.MD5 != strings.Repeat("a", 32) || d.SHA256 != strings.Repeat("b", 64) || d.SHA512 != strings.Repeat("c", 128) {
		t.Fatalf("digests %+v", d)
	}
	if d.LastModifiedMS == nil || *d.LastModifiedMS != s.CreatedMS {
		t.Fatalf("last modified %v, the write created the blob at %d", d.LastModifiedMS, s.CreatedMS)
	}
	for _, c := range []struct {
		name, store, path string
		created           int64
	}{{"another creation time", "write", s.Path, s.CreatedMS + 1}, {"another store", "north", s.Path, s.CreatedMS},
		{"another path", "write", "docs/none.txt", s.CreatedMS}} {
		if d, err := cat.Detail(s.Repo, c.path, c.store, c.created); err != nil || d != nil {
			t.Errorf("%s: %+v %v", c.name, d, err)
		}
	}
}
