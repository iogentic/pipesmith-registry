//go:build linux

package release

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// watchOpens counts the opens of path from now on, through inotify, which
// sees opens by root as well.
func watchOpens(t *testing.T, path string) func() int {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_OPEN|syscall.IN_ACCESS); err != nil {
		t.Fatal(err)
	}
	return func() int {
		events := 0
		buf := make([]byte, 64*(syscall.SizeofInotifyEvent+syscall.NAME_MAX+1))
		for {
			n, err := syscall.Read(fd, buf)
			if errors.Is(err, syscall.EAGAIN) {
				return events
			}
			if err != nil {
				t.Fatal(err)
			}
			events += n / syscall.SizeofInotifyEvent
		}
	}
}

// TestUpdateNeverOpensCosignPub: neither the check nor the install opens the
// cosign.pub of the release directory.
func TestUpdateNeverOpensCosignPub(t *testing.T) {
	r := newRig(t, "0.1.0")
	dir := r.release(versionScript("0.2.0"))
	pub := filepath.Join(dir, PublicKeyFile)
	opens := watchOpens(t, pub)
	if err := r.u.Update(dir, false); err != nil {
		t.Fatal(err)
	}
	if n := opens(); n != 0 {
		t.Fatalf("cosign.pub of the release was opened or read: %d events", n)
	}
	if _, err := os.Stat(filepath.Join(r.l.Release("0.2.0"), PublicKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cosign.pub was installed: %v", err)
	}
	// The watch sees an open.
	if _, err := os.ReadFile(pub); err != nil {
		t.Fatal(err)
	}
	if n := opens(); n == 0 {
		t.Fatal("the watch saw no open of cosign.pub")
	}
}
