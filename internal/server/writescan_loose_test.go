package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// N2 of the findings of 0.2.0: a kill between the rename of the bytes and
// the rename of the properties of a write leaves a bytes file without
// properties that nothing removed. A scan of the write store with writes on
// removes it; a scan with writes off leaves it.

// looseBytes writes two blobs into demo-raw and removes the properties of
// the second, as such a kill leaves it; it returns the first and the bytes
// file of the second.
func (w *writing) looseBytes() (kept *ingestResult, loose string) {
	w.t.Helper()
	kept, err := w.ingest("demo-raw", "docs/kept.txt", "kept")
	if err != nil {
		w.t.Fatal(err)
	}
	gone, err := w.ingest("demo-raw", "docs/gone.txt", "gone")
	if err != nil {
		w.t.Fatal(err)
	}
	loose = filepath.Join(w.root(), filepath.FromSlash(gone.asset.Location))
	if err := os.Remove(strings.TrimSuffix(loose, stores.BytesSuffix) + stores.PropertiesSuffix); err != nil {
		w.t.Fatal(err)
	}
	return kept, loose
}

// expectKept checks that the blob of kept keeps both files and is served.
func (w *writing) expectKept(kept *ingestResult) {
	w.t.Helper()
	if _, err := os.Stat(filepath.Join(w.root(), filepath.FromSlash(kept.asset.Location))); err != nil {
		w.t.Fatalf("the bytes of the kept blob: %v", err)
	}
	if props := w.properties(kept.asset.Location); props["@BlobStore.blob-name"] != "/docs/kept.txt" {
		w.t.Fatalf("the properties of the kept blob: %q", props)
	}
	if r := w.fetch("demo-raw", "docs/kept.txt"); r.status != 200 || string(r.body) != "kept" {
		w.t.Fatalf("%d %q", r.status, r.body)
	}
}

func TestAScanRemovesABytesFileWithoutProperties(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	kept, loose := w.looseBytes()
	w.scanWrites()
	if _, err := os.Lstat(loose); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the bytes file without properties is still there: %v", err)
	}
	w.expectKept(kept)
	if row, err := w.srv.rp.index.Lookup("demo-raw", "docs/gone.txt"); err != nil || row != nil {
		t.Fatalf("the path of the loose bytes has a row: %+v %v", row, err)
	}
}

func TestAScanWithWritesOffKeepsABytesFileWithoutProperties(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	kept, loose := w.looseBytes()
	w.restart(false)
	w.scanWrites()
	if _, err := os.Lstat(loose); err != nil {
		t.Fatalf("the scan with writes off removed the loose bytes file: %v", err)
	}
	w.expectKept(kept)
}
