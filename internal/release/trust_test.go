package release

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uidOf is the stat of the files, with uid for the file at path.
func uidOf(t *testing.T, path string, uid int) func(string) (fileStat, error) {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return func(p string) (fileStat, error) {
		st, err := lstatFile(p)
		if p == real {
			st.uid = uid
		}
		return st, err
	}
}

// The root, releases/, update.json, the trusted key and its directory must
// belong to the owner of the root (the last three also to uid 0), and
// neither group nor others may write them. rollback does not read the key.
func TestTrustRule(t *testing.T) {
	for name, set := range map[string]func(r *rig) trustLayout{
		"root writable by the group": func(r *rig) trustLayout { return chmod(r, r.l.Root, 0o775, false) },
		"releases writable by others": func(r *rig) trustLayout {
			return chmod(r, r.l.Releases(), 0o757, false)
		},
		"releases of another uid": func(r *rig) trustLayout {
			r.u.statOf = uidOf(r.t, r.l.Releases(), 4242)
			return trustLayout{r.l.Releases(), false}
		},
		"releases of uid 0 under another owner": func(r *rig) trustLayout {
			owner := 4242
			r.u.ownerOf = func(string) (int, error) { return owner, nil }
			r.u.euid = func() int { return owner }
			r.u.statOf = func(p string) (fileStat, error) {
				st, err := lstatFile(p)
				if p != filepath.Join(r.l.Root, ReleasesDir) {
					st.uid = owner
				} else {
					st.uid = 0
				}
				return st, err
			}
			return trustLayout{r.l.Releases(), false}
		},
		"update.json writable by the group": func(r *rig) trustLayout {
			return chmod(r, filepath.Join(r.l.Root, ConfigFile), 0o620, false)
		},
		"update.json of another uid": func(r *rig) trustLayout {
			p := filepath.Join(r.l.Root, ConfigFile)
			r.u.statOf = uidOf(r.t, p, 4242)
			return trustLayout{p, false}
		},
		"update.json a symlink": func(r *rig) trustLayout {
			p := filepath.Join(r.l.Root, ConfigFile)
			moved := filepath.Join(r.t.TempDir(), ConfigFile)
			if err := os.Rename(p, moved); err != nil {
				r.t.Fatal(err)
			}
			if err := os.Symlink(moved, p); err != nil {
				r.t.Fatal(err)
			}
			return trustLayout{p, false}
		},
		"key writable by others": func(r *rig) trustLayout { return chmod(r, r.s.pub, 0o602, true) },
		"key of another uid": func(r *rig) trustLayout {
			r.u.statOf = uidOf(r.t, r.s.pub, 4242)
			return trustLayout{r.s.pub, true}
		},
		"key a link to a file writable by the group": func(r *rig) trustLayout {
			target := filepath.Join(r.t.TempDir(), "real.pub")
			data, err := os.ReadFile(r.s.pub)
			if err != nil {
				r.t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0o620); err != nil {
				r.t.Fatal(err)
			}
			if err := os.Chmod(target, 0o620); err != nil {
				r.t.Fatal(err)
			}
			link := filepath.Join(r.t.TempDir(), "link.pub")
			if err := os.Symlink(target, link); err != nil {
				r.t.Fatal(err)
			}
			writeConfig(r.t, r.l.Root, fmt.Sprintf(`{"trusted_key": %q, "data_dir": %q}`, link, r.data))
			return trustLayout{target, true}
		},
		"key in a directory writable by others": func(r *rig) trustLayout {
			dir := keyDir(r, 0o777)
			return trustLayout{dir, true}
		},
		"key in a directory of another uid": func(r *rig) trustLayout {
			dir := filepath.Dir(r.s.pub)
			r.u.statOf = uidOf(r.t, dir, 4242)
			return trustLayout{dir, true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.2.0", "0.1.0")
			if err := os.Symlink("releases/0.1.0", filepath.Join(r.l.Root, PreviousLink)); err != nil {
				t.Fatal(err)
			}
			dir := r.release(versionScript("0.3.0"))
			want := set(r)
			err := r.unchanged(ErrPermissions, func() error { return r.u.Update(dir, false) })
			for _, words := range []string{want.path, "uid", "mode"} {
				if !strings.Contains(err.Error(), words) {
					t.Fatalf("message %q lacks %q", err, words)
				}
			}
			if want.keyOnly {
				if err := r.u.Rollback(); err != nil {
					t.Fatalf("rollback reads no key: %v", err)
				}
				return
			}
			r.unchanged(ErrPermissions, r.u.Rollback)
		})
	}
}

// trustLayout names the path a refusal names, and whether rollback passes.
type trustLayout struct {
	path    string
	keyOnly bool
}

// chmod sets the mode of path and returns the layout that names it.
func chmod(r *rig, path string, mode os.FileMode, keyOnly bool) trustLayout {
	r.t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { os.Chmod(path, 0o700) })
	return trustLayout{path, keyOnly}
}

// keyDir moves the trusted key into a new directory of mode and points
// update.json at it.
func keyDir(r *rig, mode os.FileMode) string {
	r.t.Helper()
	dir := filepath.Join(r.t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		r.t.Fatal(err)
	}
	key := filepath.Join(dir, "trusted.pub")
	if err := os.Rename(r.s.pub, key); err != nil {
		r.t.Fatal(err)
	}
	r.s.pub = key
	r.config("")
	return dir
}

// Layouts that pass: a key in a directory others can write with the sticky
// bit, a key through a symlink, files of uid 0 under another owner.
func TestTrustRulePasses(t *testing.T) {
	t.Run("sticky", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		keyDir(r, 0o777|os.ModeSticky)
		if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("key link", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		link := filepath.Join(t.TempDir(), "link.pub")
		if err := os.Symlink(r.s.pub, link); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, r.l.Root, fmt.Sprintf(`{"trusted_key": %q, "data_dir": %q}`, link, r.data))
		if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("uid 0 under another owner", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		owner := 4242
		r.u.ownerOf = func(string) (int, error) { return owner, nil }
		r.u.euid = func() int { return owner }
		real := map[string]bool{}
		for _, p := range []string{r.l.Root, r.l.Releases()} {
			q, err := filepath.EvalSymlinks(p)
			if err != nil {
				t.Fatal(err)
			}
			real[q] = true
		}
		r.u.statOf = func(p string) (fileStat, error) {
			st, err := lstatFile(p)
			if real[p] {
				st.uid = owner
			} else {
				st.uid = 0
			}
			return st, err
		}
		if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
			t.Fatal(err)
		}
	})
}

// The fetch stage holds the root under the same rule.
func TestHoldRootTrustRule(t *testing.T) {
	r := newRig(t, "0.1.0")
	if err := os.Chmod(filepath.Join(r.l.Root, ConfigFile), 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err := HoldRoot(r.l.Root); err == nil || !strings.Contains(err.Error(), ConfigFile) || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("hold: %v", err)
	}
	if err := os.Chmod(filepath.Join(r.l.Root, ConfigFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.s.pub, 0o606); err != nil {
		t.Fatal(err)
	}
	if _, err := HoldRoot(r.l.Root); err == nil || !strings.Contains(err.Error(), r.s.pub) {
		t.Fatalf("hold with an open key: %v", err)
	}
}
