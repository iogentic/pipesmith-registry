package catalog

import (
	"errors"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/index"
)

// A fill that holds the writer (Fill.Hold) keeps every other write of the
// opening waiting until it ends, and a write gives up after writeWait with
// ErrBusy, as a write of another opening does after the busy timeout.
func TestAHeldFillKeepsOtherWritesWaiting(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	before := writeWait
	writeWait = 50 * time.Millisecond
	defer func() { writeWait = before }()
	fill, err := c.Begin("local-write")
	if err != nil {
		t.Fatal(err)
	}
	if err := fill.Hold(); err != nil {
		t.Fatal(err)
	}
	row := index.Asset{Repo: "demo-raw", Path: "docs/a.txt", Store: "local-write", Size: 1, CreatedMS: 1}
	started := time.Now()
	if err := c.Put(row, "raw", Digests{}, "", ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("a write beside a held fill: %v", err)
	}
	if waited := time.Since(started); waited < writeWait {
		t.Fatalf("the write gave up after %s", waited)
	}
	released := make(chan error, 1)
	writeWait = 10 * time.Second
	go func() { released <- c.Put(row, "raw", Digests{}, "", "") }()
	time.Sleep(20 * time.Millisecond)
	if err := fill.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatalf("the write after the fill ended: %v", err)
	}
}
