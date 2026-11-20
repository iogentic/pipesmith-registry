package blobwrite

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The age rule of the work files (ruling KN7 of leg 6-kinds): a task
// removes a work file of content/tmp only when it is a regular file named
// as a work file and was last written before a cutoff. The caller leaves
// out the files a running upload holds.

// OldWorkFiles are the names of the work files in content/tmp of the store
// at root last written before cutoff, in the order of the directory, and
// the number of the younger ones. It stops once ctx ends (ruling KN14).
func OldWorkFiles(ctx context.Context, root string, cutoff time.Time) ([]string, int, error) {
	tmp := filepath.Join(root, stores.ContentDir, TmpDir)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil, 0, err
	}
	var old []string
	young := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !e.Type().IsRegular() || !workFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if info.ModTime().Before(cutoff) {
			old = append(old, e.Name())
		} else {
			young++
		}
	}
	return old, young, nil
}

// RemoveOldWorkFile removes the work file name of content/tmp of the store
// at root when it is still a regular work file last written before cutoff.
// It is false for a file that is gone, younger, or no work file.
func RemoveOldWorkFile(root, name string, cutoff time.Time) (bool, error) {
	if !workFile(name) || filepath.Base(name) != name {
		return false, nil
	}
	path := filepath.Join(root, stores.ContentDir, TmpDir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// Holds reports whether a running upload holds the work file name of
// content/tmp (ruling KN15).
func (w *Writer) Holds(name string) bool {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	return w.held[name] > 0
}

// Hold marks the work file name of content/tmp as held until release is
// called, for a work file the writer does not make itself.
func (w *Writer) Hold(name string) (release func()) {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	if w.held == nil {
		w.held = map[string]int{}
	}
	w.held[name]++
	var once sync.Once
	return func() {
		once.Do(func() {
			w.heldMu.Lock()
			defer w.heldMu.Unlock()
			if w.held[name]--; w.held[name] <= 0 {
				delete(w.held, name)
			}
		})
	}
}
