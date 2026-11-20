package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The secret file of a proxy (decision 5.13 of the phase 5 plan): one file
// per repository below secrets/remote/ of the data directory, mode 0600, the
// folders 0700.
func TestSecretFile(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := ReadSecret(dataDir, "demo-raw-proxy"); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("no file yet: %v", err)
	}
	path, err := WriteSecret(dataDir, "demo-raw-proxy", "first secret")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataDir, "secrets", "remote", "demo-raw-proxy"); path != want {
		resolved, _ := filepath.EvalSymlinks(want)
		if path != resolved {
			t.Fatalf("path %s, want %s", path, want)
		}
	}
	for dir, mode := range map[string]os.FileMode{filepath.Join(dataDir, "secrets"): 0o700, filepath.Join(dataDir, "secrets", "remote"): 0o700,
		path: 0o600} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s has mode %o, want %o", dir, info.Mode().Perm(), mode)
		}
	}
	if secret, err := ReadSecret(dataDir, "demo-raw-proxy"); err != nil || secret != "first secret" {
		t.Fatalf("%q %v", secret, err)
	}
	if _, err := WriteSecret(dataDir, "demo-raw-proxy", "second secret"); err != nil {
		t.Fatal(err)
	}
	if secret, err := ReadSecret(dataDir, "demo-raw-proxy"); err != nil || secret != "second secret" {
		t.Fatalf("after a second write: %q %v", secret, err)
	}
	for _, name := range []string{"", ".", "..", "../outside", "a/b", `a\b`, "a\x00b"} {
		if _, err := WriteSecret(dataDir, name, "x"); err == nil {
			t.Errorf("%q: written", name)
		}
		if _, err := ReadSecret(dataDir, name); err == nil || errors.Is(err, ErrNoSecret) {
			t.Errorf("%q: read %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "remote", "demo-empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(dataDir, "demo-empty"); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("an empty file: %v", err)
	}
}

// A secret file is resolved before it is checked against its folder: a
// link to a file outside the folder is refused when read, and a write
// replaces the link with a file of its own and leaves the target alone.
func TestSecretFileLinks(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := WriteSecret(dataDir, "demo-raw-proxy", "a secret"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(outside, []byte("outside the folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dataDir, "secrets", "remote", "demo-linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if secret, err := ReadSecret(dataDir, "demo-linked"); err == nil || errors.Is(err, ErrNoSecret) {
		t.Fatalf("a link out of the folder: %q %v", secret, err)
	}
	if _, err := WriteSecret(dataDir, "demo-linked", "new secret"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "outside the folder" {
		t.Fatalf("the write followed the link: %q", data)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the written file: %v %v", info, err)
	}
	if secret, err := ReadSecret(dataDir, "demo-linked"); err != nil || secret != "new secret" {
		t.Fatalf("%q %v", secret, err)
	}
}
