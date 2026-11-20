package channel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// A fetch removes the releases/<version>.partial/ of every version that a
// killed fetch left, not only the one of its candidate (C-3).
func TestLeftoverPartialsOfOtherVersionsAreRemoved(t *testing.T) {
	for name, publish := range map[string]bool{"a version is staged": true, "nothing to do": false} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			if publish {
				r.publish(published{version: "0.2.0"})
			}
			r.start()
			var left []string
			for _, v := range []string{"0.1.5", "0.3.0"} {
				dir := filepath.Join(r.root, release.ReleasesDir, v+".partial")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				left = append(left, dir)
			}
			keep := filepath.Join(r.root, release.ReleasesDir, "notes.partial")
			if err := os.WriteFile(keep, []byte("not a directory of a fetch"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.f.Fetch(Options{}); err != nil {
				t.Fatalf("fetch: %v\n%s", err, r.out.String())
			}
			for _, dir := range left {
				if exists(dir) {
					t.Errorf("%s stayed", dir)
				}
			}
			if !exists(keep) {
				t.Error("a file that is no version directory was removed")
			}
		})
	}
}
