package stores_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// TestDirectoryStoreKeepsALinkInsideItsRepository is sa-22: a link in the
// folder of one repository that names a file of another repository is no
// entry and does not open; a link inside its own repository does.
func TestDirectoryStoreKeepsALinkInsideItsRepository(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"open-raw/docs/readme.txt": "open text", "closed-raw/docs/secret.txt": "closed text"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "closed-raw", "docs", "secret.txt"), filepath.Join(root, "open-raw", "docs", "borrowed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("readme.txt", filepath.Join(root, "open-raw", "docs", "again.txt")); err != nil {
		t.Fatal(err)
	}
	s, err := stores.NewDirectoryStore("tree", root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	if err := s.Entries(nil, func(e stores.Entry) error { listed[e.Location] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if listed["open-raw/docs/borrowed.txt"] || !listed["open-raw/docs/again.txt"] || !listed["closed-raw/docs/secret.txt"] {
		t.Errorf("entries %v: want again.txt and secret.txt, not borrowed.txt", listed)
	}
	if f, err := s.Open("open-raw/docs/borrowed.txt"); err == nil {
		data, _ := io.ReadAll(f)
		f.Close()
		t.Errorf("open-raw/docs/borrowed.txt opened and holds %q", data)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("open-raw/docs/borrowed.txt: %v, want a not found error", err)
	}
	f, err := s.Open("open-raw/docs/again.txt")
	if err != nil {
		t.Fatalf("a link inside its repository: %v", err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != "open text" {
		t.Errorf("open-raw/docs/again.txt holds %q, %v", data, err)
	}
}
