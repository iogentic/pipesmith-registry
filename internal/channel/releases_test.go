package channel

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// A missing or unreadable releases/ is named as such, not as another fetch
// stage that runs (C-10).
func TestReleasesDirectoryMissingOrUnreadable(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		r := newRig(t)
		r.start()
		dir := filepath.Join(r.root, release.ReleasesDir)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		_, err := r.f.Fetch(Options{})
		if !errors.Is(err, ErrStage) || errors.Is(err, release.ErrFetching) || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("error %v", err)
		}
		if len(r.stub.Requests()) != 0 {
			t.Fatal("requests sent")
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("uid 0 reads a directory of mode 0")
		}
		r := newRig(t)
		r.start()
		dir := filepath.Join(r.root, release.ReleasesDir)
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		_, err := r.f.Fetch(Options{})
		if !errors.Is(err, ErrStage) || errors.Is(err, release.ErrFetching) || !strings.Contains(err.Error(), dir) {
			t.Fatalf("error %v", err)
		}
	})
}
