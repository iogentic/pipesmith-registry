package blobwrite

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Prepare writes and syncs the properties work file before Commit, so that
// Commit, which a write runs under the write lock of the index, only
// renames the two files and appends the log line. The blob is the one a
// Commit without Prepare writes.
func TestPrepareTakesThePropertiesOutOfCommit(t *testing.T) {
	root := layout(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1)})
	var seen []Step
	w.after = func(step Step) { seen = append(seen, step) }
	u, err := w.Upload(strings.NewReader("payload"), -1)
	if err != nil {
		t.Fatal(err)
	}
	a := attributes("demo-raw", "a/b.txt")
	seen = nil
	if err := u.Prepare(a); err != nil {
		t.Fatal(err)
	}
	if want := []Step{StepProperties}; !slices.Equal(seen, want) {
		t.Fatalf("steps of Prepare %v, want %v", seen, want)
	}
	stem := filepath.Join(root, "content/2026/11/18/10/15", id1)
	if _, err := os.Stat(stem + ".properties"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the properties are placed before Commit: %v", err)
	}
	seen = nil
	got, err := u.Commit(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Step{StepRenameBytes, StepRenameProperties, StepLog}; !slices.Equal(seen, want) {
		t.Fatalf("steps of Commit %v, want %v", seen, want)
	}
	if got.Entry.CreatedMS != fixedNow.UnixMilli() || got.Entry.Path != "a/b.txt" {
		t.Fatalf("written %+v", got.Entry)
	}
	props, err := os.ReadFile(stem + ".properties") // #nosec G304 -- a file of the test store.
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"@BlobStore.blob-name=/a/b.txt", "@Bucket.repo-name=demo-raw", "size=7"} {
		if !strings.Contains(string(props), line+"\n") {
			t.Errorf("properties miss %q:\n%s", line, props)
		}
	}
	if left := files(t, filepath.Join(root, "content", "tmp")); len(left) != 0 {
		t.Fatalf("work files left %+v", left)
	}
}

// Commit with attributes other than those of Prepare is refused and leaves
// no file; a Discard after Prepare removes the properties work file too.
func TestPrepareBindsTheAttributes(t *testing.T) {
	root := layout(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1, id2)})
	u, err := w.Upload(strings.NewReader("payload"), -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Prepare(attributes("demo-raw", "a/b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Commit(attributes("demo-raw", "a/other.txt")); err == nil {
		t.Fatal("commit with other attributes")
	}
	if left := files(t, root); len(left) != 0 {
		t.Fatalf("files left %+v", left)
	}
	u, err = w.Upload(strings.NewReader("payload"), -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Prepare(attributes("demo-raw", "a/b.txt")); err != nil {
		t.Fatal(err)
	}
	if left := files(t, filepath.Join(root, "content", "tmp")); len(left) != 2 {
		t.Fatalf("work files after Prepare %+v, want bytes and properties", left)
	}
	if err := u.Discard(); err != nil {
		t.Fatal(err)
	}
	if left := files(t, root); len(left) != 0 {
		t.Fatalf("files left after discard %+v", left)
	}
}
