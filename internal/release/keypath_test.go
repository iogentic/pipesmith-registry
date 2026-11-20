package release

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The trust rule checks every directory above the trusted key, and the
// directories that hold a symlink naming it (finding A-9).
func TestTrustRuleKeyPath(t *testing.T) {
	place := func(t *testing.T, r *rig) (open, keyDir, key string) {
		open = filepath.Join(t.TempDir(), "open")
		keyDir = filepath.Join(open, "keys")
		if err := os.MkdirAll(keyDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o777); err != nil {
			t.Fatal(err)
		}
		key = filepath.Join(keyDir, "cosign.pub")
		data, err := os.ReadFile(r.s.pub)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(key, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return open, keyDir, key
	}
	t.Run("a directory above", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		open, _, key := place(t, r)
		writeConfig(t, r.l.Root, fmt.Sprintf(`{"trusted_key": %q, "data_dir": %q}`, key, r.data))
		err := r.unchanged(ErrPermissions, func() error { return r.u.Update(r.release(versionScript("0.2.0")), false) })
		if !strings.Contains(err.Error(), open+" ") {
			t.Fatalf("message %q", err)
		}
	})
	t.Run("the place of a link", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		open := filepath.Join(t.TempDir(), "open")
		if err := os.Mkdir(open, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(open, 0o777); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(open, "key.pem")
		if err := os.Symlink(r.s.pub, link); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, r.l.Root, fmt.Sprintf(`{"trusted_key": %q, "data_dir": %q}`, link, r.data))
		err := r.unchanged(ErrPermissions, func() error { return r.u.Update(r.release(versionScript("0.2.0")), false) })
		if !strings.Contains(err.Error(), open+" ") {
			t.Fatalf("message %q", err)
		}
	})
}
