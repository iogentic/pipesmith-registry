package blobwrite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// Only regular files named as work files and last written before the
// cutoff are old, and only those are removed; a second removal removes
// nothing.
func TestOldWorkFiles(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "content", "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	const (
		oldBytes = "tmp$0b7c6f0e-1d8e-4c1a-9b0e-2f3a4b5c6d7e.1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.bytes"
		oldProps = "tmp$0b7c6f0e-1d8e-4c1a-9b0e-2f3a4b5c6d7e.1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.properties"
		young    = "tmp$2b7c6f0e-1d8e-4c1a-9b0e-2f3a4b5c6d7e.1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.bytes"
		other    = "demo-notes.txt"
	)
	for name, age := range map[string]time.Duration{oldBytes: 48 * time.Hour, oldProps: 25 * time.Hour, young: time.Hour, other: 48 * time.Hour} {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	oldDir := filepath.Join(tmp, "tmp$4b7c6f0e-1d8e-4c1a-9b0e-2f3a4b5c6d7e.1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.bytes")
	if err := os.Mkdir(oldDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	old, youngCount, err := OldWorkFiles(context.Background(), root, cutoff)
	slices.Sort(old)
	if err != nil || !slices.Equal(old, []string{oldBytes, oldProps}) || youngCount != 1 {
		t.Fatalf("%v %d %v", old, youngCount, err)
	}
	for _, name := range []string{oldBytes, young, other, "../" + oldProps} {
		removed, err := RemoveOldWorkFile(root, name, cutoff)
		if err != nil || removed != (name == oldBytes) {
			t.Errorf("%s: %v %v", name, removed, err)
		}
	}
	var left []string
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	slices.Sort(left)
	want := []string{other, oldProps, young, filepath.Base(oldDir)}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	if removed, err := RemoveOldWorkFile(root, oldBytes, cutoff); removed || err != nil {
		t.Fatalf("a second removal: %v %v", removed, err)
	}
}

// Ruling KN14: the listing of a large work folder stops once its context
// ends.
func TestOldWorkFilesStop(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "content", "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for i := range 500 {
		path := filepath.Join(tmp, fmt.Sprintf("tmp$0b7c6f0e-1d8e-4c1a-9b0e-%012d.1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.bytes", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if names, _, err := OldWorkFiles(context.Background(), root, time.Now()); err != nil || len(names) != 500 {
		t.Fatalf("%d %v", len(names), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	names, _, err := OldWorkFiles(ctx, root, time.Now())
	if !errors.Is(err, context.Canceled) || names != nil {
		t.Fatalf("a stopped listing: %d names, %v", len(names), err)
	}
}

// Ruling KN15: the work files of an upload are held from their creation
// until the upload is committed or discarded, and Hold holds a name until
// its release.
func TestUploadsHoldTheirWorkFiles(t *testing.T) {
	root := layout(t)
	w := openTestWriter(t, root, Options{Now: fixedClock, NewID: idList(id1, id2)})
	tmp := filepath.Join(root, "content", "tmp")
	var seen []string
	w.after = func(step Step) {
		if step != StepStream {
			return
		}
		entries, _ := os.ReadDir(tmp)
		for _, e := range entries {
			seen = append(seen, e.Name())
			if !w.Holds(e.Name()) {
				t.Errorf("after %s the work file %s is not held", step, e.Name())
			}
		}
	}
	upload, err := w.Upload(strings.NewReader("held"), -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || !w.Holds(seen[0]) {
		t.Fatalf("seen %v", seen)
	}
	if err := upload.Discard(); err != nil {
		t.Fatal(err)
	}
	if w.Holds(seen[0]) {
		t.Fatal("a discarded upload still holds its work file")
	}
	w.after = nil
	if _, err := w.Write(strings.NewReader("written"), stores.Attributes{Repo: "demo-raw", Path: "docs/held.txt", CreatedBy: "demo-admin"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if w.Holds(e.Name()) {
			t.Errorf("after a write %s is held", e.Name())
		}
	}
	release := w.Hold("tmp$demo")
	if !w.Holds("tmp$demo") {
		t.Fatal("Hold holds nothing")
	}
	release()
	if w.Holds("tmp$demo") {
		t.Fatal("a released name is held")
	}
}
