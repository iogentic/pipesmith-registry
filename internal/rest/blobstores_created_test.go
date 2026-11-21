package rest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// noIndex is an index without rows.
type noIndex struct{}

func (noIndex) Lookup(string, string) (*index.Asset, error) { return nil, nil }
func (noIndex) StoreStats() ([]index.StoreStat, error)      { return nil, nil }

// Finding 13: a store record is available when its folder lies below the
// stores folder of the data directory, not when its path holds the word
// stores somewhere.
func TestACreatedStoreIsAvailableByItsFolder(t *testing.T) {
	data := t.TempDir()
	inside := filepath.Join(data, "stores", "demo-made")
	outside := filepath.Join(data, "elsewhere", "stores", "demo-old")
	// the store outside holds blobs: an empty readable record is available
	// since ruling SR11, whatever its folder
	for _, dir := range []string{inside, filepath.Join(outside, "content", "vol-01")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	env := &Env{Index: noIndex{}, StoresRoot: filepath.Join(data, "stores"), Snapshot: &model.Snapshot{BlobStores: []model.BlobStore{
		{Name: "demo-made", Type: "File", Path: inside}, {Name: "demo-old", Type: "File", Path: outside}}}}
	list, err := env.blobStoreXOs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, xo := range list {
		got[xo.Name] = xo.Unavailable
	}
	if got["demo-made"] || !got["demo-old"] {
		t.Fatalf("unavailable: %v", got)
	}
}
