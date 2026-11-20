//go:build linux || darwin

package release

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A release directory with a FIFO in place of a file that the check reads
// is refused at once instead of blocking update while it holds the lock of
// the root (finding A-19).
func TestFIFOInTheReleaseDirectory(t *testing.T) {
	for _, name := range []string{platformBinary(), SumsFile, BundleFile} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			dir := r.release(versionScript("0.2.0"))
			p := filepath.Join(dir, name)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- r.u.Update(dir, false) }()
			select {
			case err := <-done:
				if err == nil || errors.Is(err, ErrLocked) {
					t.Fatalf("update: %v", err)
				}
			case <-time.After(10 * time.Second):
				// unblock the open, so the goroutine ends
				if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					f.Close()
				}
				t.Fatal("update blocked on the FIFO")
			}
		})
	}
}
