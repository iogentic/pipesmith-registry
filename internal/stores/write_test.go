package stores_test

import (
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// Only NewWriteStore sets the writable flag; a store of config.json never
// carries it.
func TestWritableFlag(t *testing.T) {
	root := t.TempDir()
	read, err := stores.NewBlobStore("old", root)
	if err != nil {
		t.Fatal(err)
	}
	write, err := stores.NewWriteStore("local-write", root)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := stores.NewDirectoryStore("tree", root, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	made, err := stores.Make(config.StoreConfig{Name: "made", Kind: stores.KindBlobStore, Root: root}, "")
	if err != nil {
		t.Fatal(err)
	}
	if read.Writable() || stores.Writable(read) || stores.Writable(tree) || stores.Writable(made) {
		t.Fatal("a read only store carries the writable flag")
	}
	if !write.Writable() || !stores.Writable(write) || write.Kind() != stores.KindBlobStore || write.Name() != "local-write" {
		t.Fatalf("%+v", write)
	}
}
