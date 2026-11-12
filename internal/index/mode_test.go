package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/index"
)

// The directory that Open creates for the index gives no access to other
// users and no write access to the group.
func TestOpenCreatesDirectoryWithNarrowMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	ix, err := index.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		t.Fatalf("index directory mode %o, want no group write and no access for others", perm)
	}
}
