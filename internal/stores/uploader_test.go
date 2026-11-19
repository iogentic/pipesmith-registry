package stores_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

func entriesOf(t *testing.T, s stores.Store) map[string]stores.Entry {
	t.Helper()
	found := map[string]stores.Entry{}
	err := s.Entries(nil, func(e stores.Entry) error {
		found[e.Repo+"/"+e.Path] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// An entry of a file blob store carries the uploader and its address of its
// properties file (decision R14 of the phase 1c plan), nil where the file
// has none; an entry of a directory store has neither.
func TestEntriesCarryTheUploader(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	with := testkit.NewBlobSpec("demo-raw", "docs/with.txt")
	without := testkit.NewBlobSpec("demo-raw", "docs/without.txt")
	without.Content = []byte("without")
	if _, err := testkit.MakeBlobStore(root, []testkit.BlobSpec{with, without}); err != nil {
		t.Fatal(err)
	}
	items := testkit.SpecProperties(without)
	delete(items, "@BlobStore.created-by")
	delete(items, "@BlobStore.created-by-ip")
	properties := filepath.Join(root, filepath.FromSlash(testkit.PropertiesLocation(without)))
	if err := os.WriteFile(properties, testkit.EncodeProperties(items, without.CreatedMS), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := stores.NewBlobStore("demo-store", root)
	if err != nil {
		t.Fatal(err)
	}
	found := entriesOf(t, s)
	if e := found["demo-raw/docs/with.txt"]; show(e.Uploader) != "demo-user" || show(e.UploaderIP) != "192.0.2.10" {
		t.Fatalf("entry with an uploader: %s %s, want demo-user 192.0.2.10", show(e.Uploader), show(e.UploaderIP))
	}
	if e, ok := found["demo-raw/docs/without.txt"]; !ok || e.Uploader != nil || e.UploaderIP != nil {
		t.Fatalf("entry without an uploader: %v %s %s", ok, show(e.Uploader), show(e.UploaderIP))
	}

	tree, err := testkit.MakeTree(filepath.Join(t.TempDir(), "tree"), map[string][]byte{"demo-raw/docs/a.txt": []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	d, err := stores.NewDirectoryStore("demo-tree", tree, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := entriesOf(t, d)["demo-raw/docs/a.txt"]; !ok || e.Uploader != nil || e.UploaderIP != nil {
		t.Fatalf("entry of a directory store: %v %s %s", ok, show(e.Uploader), show(e.UploaderIP))
	}
}
