package repos_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/repos"
)

// The directory that WriteDefinitions creates gives no access to other
// users and no write access to the group.
func TestWriteDefinitionsCreatesDirectoryWithNarrowMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	target := filepath.Join(dir, "repositories.json")
	if err := repos.WriteDefinitions(target, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		t.Fatalf("directory mode %o, want no group write and no access for others", perm)
	}
}
