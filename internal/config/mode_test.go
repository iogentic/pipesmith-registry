package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/config"
)

// The data directory that Save creates gives no access to other users and
// no write access to the group.
func TestSaveCreatesDataDirectoryWithNarrowMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	if err := config.Save(config.New(dir)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		t.Fatalf("data directory mode %o, want no group write and no access for others", perm)
	}
}
