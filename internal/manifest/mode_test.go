package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/manifest"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
)

// The cache directory that Open creates for the lookup table gives no access
// to other users and no write access to the group, also for the parents it
// creates.
func TestOpenCreatesCacheDirectoryWithNarrowMode(t *testing.T) {
	base := t.TempDir()
	writeManifest(t, filepath.Join(base, "m", "part-00000.tsv"),
		testkit.ManifestRow{Repo: "demo-raw", Path: "a.txt", Size: 1, CreatedMS: 1})
	cache := filepath.Join(base, "data", "manifests")
	l, err := manifest.Open(filepath.Join(base, "m"), cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	for _, dir := range []string{filepath.Dir(cache), cache} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if perm := info.Mode().Perm(); perm&0o027 != 0 {
			t.Fatalf("%s has mode %o, want no group write and no access for others", dir, perm)
		}
	}
}
