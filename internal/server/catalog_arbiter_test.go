package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// assetRow is the line of the asset of demo-raw docs/a.txt and the line of
// its blob in the write store, "" for none.
func (w *writing) assetRow() (asset, blob string) {
	w.t.Helper()
	for _, line := range w.catalogRows() {
		switch {
		case strings.HasPrefix(line, "asset demo-raw docs/a.txt "):
			asset = line
		case strings.HasPrefix(line, "blob local-write demo-raw docs/a.txt "):
			blob = line
		}
	}
	return asset, blob
}

// The catalog follows the arbiter of the write store, not every live blob a
// scan meets: of two live blobs of one millisecond the blob the log names
// as written last, whichever the walk meets first.
func TestTheCatalogTakesTheBlobTheWriteOrderServes(t *testing.T) {
	for _, c := range walkOrders {
		t.Run(c.name, func(t *testing.T) {
			w := startCataloged(t, c.ids...)
			first, second := w.replaceAfter(0)
			if first.asset.CreatedMS != second.asset.CreatedMS {
				t.Fatalf("creation times %d and %d", first.asset.CreatedMS, second.asset.CreatedMS)
			}
			w.restart(true)
			w.scanWrites()
			w.expectServed(second, first, reasonReplaced)
			_, blob := w.assetRow()
			if want := fmt.Sprintf("blob local-write demo-raw docs/a.txt 6 %s ", *second.asset.SHA1); !strings.HasPrefix(blob, want) {
				t.Fatalf("the catalog holds %q, the index serves %q", blob, want)
			}
			w.expectAsScanned()
		})
	}
}

// A delete of the path after the start marks the blob of the row; the
// older blob whose mark was lost is still live in the files. The scan keeps
// the path deleted in the index, and the catalog with it.
func TestTheCatalogKeepsAPathDeletedUnderALaterMark(t *testing.T) {
	for _, step := range []time.Duration{time.Millisecond, 0} {
		t.Run(step.String(), func(t *testing.T) {
			w := startCataloged(t, lowID, highID)
			w.replaceAfter(step)
			w.restart(true)
			if removed, err := w.srv.erase(w.repo("demo-raw"), "docs/a.txt"); err != nil || removed == nil {
				t.Fatalf("%+v %v", removed, err)
			}
			w.scanWrites()
			if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row != nil {
				t.Fatalf("the scan brings the deleted path back: %+v", row)
			}
			if asset, blob := w.assetRow(); asset != "" || blob != "" {
				t.Fatalf("the catalog brings the deleted path back:\n%s\n%s", asset, blob)
			}
			if rows := w.catalogRows(); !hasLine(rows, "scan local-write 0 0") {
				t.Fatalf("%s", strings.Join(rows, "\n"))
			}
		})
	}
}
