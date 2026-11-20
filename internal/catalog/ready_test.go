package catalog_test

import (
	"testing"
)

// R10: the catalog is behind while the rows of a store in scans differ from
// the counts of the index.
func TestBehindComparesScansWithTheIndex(t *testing.T) {
	f := newFixture(t)
	c := open(t, f.dir)
	stats, err := f.ix.StoreStats()
	if err != nil {
		t.Fatal(err)
	}
	if behind, err := c.Behind(stats); err != nil || behind != "" {
		t.Fatalf("empty: %q %v", behind, err)
	}
	f.scan(f.north)
	f.scan(f.south)
	if stats, err = f.ix.StoreStats(); err != nil {
		t.Fatal(err)
	}
	if behind, err := c.Behind(stats); err != nil || behind != "" {
		t.Fatalf("after the scans: %q %v", behind, err)
	}
	// a row the index took without the catalog: behind, naming the store
	if _, err := f.ix.DropRow(northSpecs[0].Repo, northSpecs[0].Path, "north"); err != nil {
		t.Fatal(err)
	}
	if stats, err = f.ix.StoreStats(); err != nil {
		t.Fatal(err)
	}
	if behind, err := c.Behind(stats); err != nil || behind != "north" {
		t.Fatalf("after a drop of the index alone: %q %v", behind, err)
	}
	// a store the index no longer has, with rows in the catalog
	if behind, err := c.Behind(stats[:0]); err != nil || behind == "" {
		t.Fatalf("without the stores of the index: %q %v", behind, err)
	}
}
