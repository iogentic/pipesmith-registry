package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"
)

// Ruling SC1 of leg 6-stores-c (finding S13 of leg checks-b, option B): the
// count of a store whose folder does not exist answers 200 with no count,
// not counting, and a reason; it names no path. Each request within
// markedRetry gives the same answer without a second walk.
func TestTheCountOfAStoreWithoutItsFolderGivesAReason(t *testing.T) {
	w := startIngest(t, nil)
	if _, err := w.srv.Rescan(context.Background(), "local-write"); err != nil { // makes catalog.sqlite
		t.Fatal(err)
	}
	w.dropMarkedRow("store-a")
	if err := os.RemoveAll(w.old); err != nil {
		t.Fatal(err)
	}
	walks := &countingWalk{}
	w.srv.marked.walk = walks.walk
	for i := range 2 {
		got := w.deletedAnswer("store-a")
		if got["blobCount"] != nil || got["bytes"] != nil || got["countedAt"] != nil || got["counting"] != false ||
			got["reason"] != MissingFolderReason {
			t.Fatalf("answer %d: %v", i, got)
		}
	}
	if n := walks.calls.Load(); n != 1 {
		t.Fatalf("%d walks, want 1 within markedRetry", n)
	}
	if _, found := w.markedRow("store-a"); found {
		t.Fatal("a missing folder wrote a row")
	}
}

// SC1 at the counter: a walk that fails because the folder does not exist
// gives the reason and no error; any other failure stays an error.
func TestOnlyAMissingFolderGivesAReason(t *testing.T) {
	for _, c := range []struct {
		label  string
		err    error
		reason string
	}{
		{"missing folder", fmt.Errorf("%w: store demo-gone: %w", errMissingFolder, fs.ErrNotExist), MissingFolderReason},
		{"read error", errors.New("demo read error"), ""},
		{"other not exist", fmt.Errorf("a properties file: %w", fs.ErrNotExist), ""},
	} {
		t.Run(c.label, func(t *testing.T) {
			c2 := newTestCounter(t, &fakeWalks{}, time.Second)
			c2.walk = func(context.Context, string, string) (markedResult, error) { return markedResult{}, c.err }
			got := c2.get("demo-gone", "/gone")
			if got.reason != c.reason || (c.reason == "") != (got.err != nil) || got.have || got.counting {
				t.Fatalf("%+v", got)
			}
		})
	}
}

// SC1 at the walk: only the missing folder of the store is errMissingFolder.
func TestTheWalkNamesAMissingFolder(t *testing.T) {
	dir := t.TempDir()
	if _, err := walkMarked(context.Background(), "demo-gone", dir+"/gone"); !errors.Is(err, errMissingFolder) {
		t.Fatalf("a missing folder: %v", err)
	}
	if _, err := walkMarked(context.Background(), "demo-new", dir); err != nil {
		t.Fatalf("a store without content/: %v", err)
	}
}
