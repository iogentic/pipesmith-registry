package server

import (
	"os"
	"path/filepath"
	"testing"
)

// A row the read path drops because the bytes of its blob are gone leaves
// the catalog in the same step (index.Drop coupled to catalog.Drop).
func TestARowTheReadPathDropsLeavesTheCatalog(t *testing.T) {
	w := startCataloged(t)
	written, err := w.ingest("demo-raw", "docs/a.txt", "one")
	if err != nil {
		t.Fatal(err)
	}
	if asset, blob := w.assetRow(); asset == "" || blob == "" {
		t.Fatalf("no catalog rows after the write: %q %q", asset, blob)
	}
	if err := os.Remove(filepath.Join(w.root(), filepath.FromSlash(written.asset.Location))); err != nil {
		t.Fatal(err)
	}
	if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 404 {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row != nil {
		t.Fatalf("the read path kept the row: %+v", row)
	}
	if asset, blob := w.assetRow(); asset != "" || blob != "" {
		t.Fatalf("the catalog keeps the dropped row:\n%s\n%s", asset, blob)
	}
}
