package scan_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// A write of the catalog hook that follows the commit of the index during a
// scan waits for the catalog side of the scan, so the fill does not undo it
// (scan.RunIn holds the writer of the catalog before the index commits).
func TestAWriteAfterTheIndexOfAScanOutlivesTheFill(t *testing.T) {
	base := t.TempDir()
	created := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC).UnixMilli()
	root, err := testkit.MakeBlobStore(filepath.Join(base, "store"), []testkit.BlobSpec{
		{Repo: "demo-raw", Path: "docs/a.txt", Content: []byte("a"), Scheme: "date", CreatedMS: created},
		{Repo: "demo-raw", Path: "docs/b.txt", Content: []byte("b"), Scheme: "date", CreatedMS: created},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := stores.NewBlobStore("local-write", root)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "data")
	ix, err := index.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	file, err := catalog.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	late := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if row, err := ix.Lookup("demo-raw", "docs/a.txt"); err != nil || row != nil {
				if err == nil {
					// the index committed the scan: a write of the hook follows it
					err = file.Put(index.Asset{Repo: "demo-raw", Path: "docs/late.txt", Store: "local-write", Size: 4,
						CreatedMS: created + 1}, "raw", catalog.Digests{}, "demo-admin", "192.0.2.7")
				}
				late <- err
				return
			}
			time.Sleep(50 * time.Microsecond)
		}
		late <- nil
	}()
	if _, err := scan.RunIn(file, store, ix, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-late; err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+catalog.Path(data)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var paths []string
	rows, err := db.Query("SELECT path FROM assets ORDER BY path")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || paths[2] != "docs/late.txt" {
		t.Fatalf("the catalog after the scan holds %v; the write that followed the index is lost", paths)
	}
}
