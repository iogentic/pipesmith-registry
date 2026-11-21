package admin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// A created store below <data>/stores/ is a write store; an imported one,
// one config.json attaches, one reached through a link out of the folder
// and local-write itself are not (SR1, SR5).
func TestWritableStore(t *testing.T) {
	data := t.TempDir()
	stores := filepath.Join(data, StoresDir)
	outside := t.TempDir()
	if err := os.MkdirAll(stores, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stores, "link")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		label    string
		store    model.BlobStore
		attached []string
		want     bool
	}{
		{"created", model.BlobStore{Name: "team-a", Path: filepath.Join(stores, "team-a")}, nil, true},
		{"created in a subfolder", model.BlobStore{Name: "team-b", Path: filepath.Join(stores, "group", "b")}, nil, true},
		{"imported", model.BlobStore{Name: "old", Path: filepath.Join(outside, "old")}, nil, false},
		{"attached by config.json", model.BlobStore{Name: "team-a", Path: filepath.Join(stores, "team-a")}, []string{"team-a"}, false},
		{"through a link out", model.BlobStore{Name: "team-c", Path: filepath.Join(stores, "link", "c")}, nil, false},
		{"the stores folder itself", model.BlobStore{Name: "team-d", Path: stores}, nil, false},
		{"local-write", model.BlobStore{Name: WriteStore, Path: filepath.Join(stores, WriteStore)}, nil, false},
		{"the folder of local-write", model.BlobStore{Name: "other", Path: filepath.Join(stores, "Local-Write")}, nil, false},
		{"no path", model.BlobStore{Name: "team-e"}, nil, false},
		// review R7: no write store inside or around local-write or another record
		{"inside local-write", model.BlobStore{Name: "team-f", Path: filepath.Join(stores, "local-write", "content", "x")}, nil, false},
		{"inside local-write in another case", model.BlobStore{Name: "team-g", Path: filepath.Join(stores, "LOCAL-WRITE", "x")}, nil, false},
		{"inside another record", model.BlobStore{Name: "team-h", Path: filepath.Join(stores, "team-a", "inner")}, nil, false},
		{"around another record", model.BlobStore{Name: "team-i", Path: filepath.Join(stores, "group")}, nil, false},
	}
	// the other records; a record compares with every one but itself
	others := []model.BlobStore{{Name: "team-a", Path: filepath.Join(stores, "team-a")}, {Name: "team-b", Path: filepath.Join(stores, "group", "b")}}
	for _, c := range cases {
		if got := WritableStore(data, c.store, c.attached, others); got != c.want {
			t.Errorf("%s: writable %v, want %v", c.label, got, c.want)
		}
	}
}
