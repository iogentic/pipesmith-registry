package state_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The secret files below secrets/ of the data directory (hardening line 11
// of the phase 3 plan): written with mode 0600 in a folder of mode 0700, read
// only while the file grants nothing to the group and to others and belongs
// to the user of the process. The value below is invented.

const secretValue = "example-bind-value-3"

func TestLDAPSecretPath(t *testing.T) {
	dir := filepath.Join("/srv", "data")
	for id, want := range map[string]string{
		"6f1c2a52-0000-4000-8000-000000000001": "ldap-6f1c2a52-0000-4000-8000-000000000001",
		"Demo_Directory.1":                     "ldap-Demo_Directory.1",
		"a/b":                                  "ldap-a%2Fb",
		"..":                                   "ldap-%2E.",
		".hidden":                              "ldap-%2Ehidden",
		"100%":                                 "ldap-100%25",
		"x y":                                  "ldap-x%20y",
		"ä":                                    "ldap-%C3%A4",
	} {
		if got := state.LDAPSecretPath(dir, id); got != filepath.Join(dir, "secrets", want) {
			t.Errorf("LDAPSecretPath(%q) = %q, want secrets/%s", id, got, want)
		}
	}
}

func TestSecretWrittenAndRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := state.LDAPSecretPath(dir, "demo-1")
	if _, err := state.ReadSecret(path); !errors.Is(err, state.ErrNoSecret) {
		t.Fatalf("a missing file gives %v, want ErrNoSecret", err)
	}
	if err := state.WriteSecret(path, []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", name, got, want)
		}
	}
	got, err := state.ReadSecret(path)
	if err != nil || string(got) != secretValue {
		t.Fatalf("ReadSecret: %q %v", got, err)
	}
	// a second write replaces the value; nothing else is left in the folder
	if err := state.WriteSecret(path, []byte(secretValue+"-new")); err != nil {
		t.Fatal(err)
	}
	if got, err := state.ReadSecret(path); err != nil || string(got) != secretValue+"-new" {
		t.Fatalf("after the second write: %q %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the secrets folder holds %d entries, %v", len(entries), err)
	}
}

func TestSecretRefusals(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := state.LDAPSecretPath(dir, "demo-1")
	if err := state.WriteSecret(path, []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, wantText string) {
		t.Helper()
		got, err := state.ReadSecret(path)
		if err == nil || errors.Is(err, state.ErrNoSecret) || !strings.Contains(err.Error(), wantText) {
			t.Errorf("%s: %q %v, want a refusal that says %q", what, got, err, wantText)
		}
		if err != nil && strings.Contains(err.Error(), secretValue) {
			t.Errorf("%s: the refusal holds the value", what)
		}
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o644, 0o601} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		refused("file mode "+mode.String(), "chmod 600")
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if got, err := state.ReadSecret(path); err != nil || string(got) != secretValue {
		t.Fatalf("a file of mode 0400 grants less than 0600 and is read: %q %v", got, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	// another owner: the user of the process is replaced in the check
	restore := state.SetProcessUID(func() int { return os.Geteuid() + 1 })
	refused("another owner", "belongs to user")
	restore()
	// as root (the container of tools/go.sh) the file and the folder each get
	// another owner, so that each check is seen alone
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 4242, 4242); err != nil {
			t.Fatal(err)
		}
		refused("a file of another owner", "the secret file "+path+" belongs to user 4242")
		if err := os.Chown(path, 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(filepath.Dir(path), 4242, 4242); err != nil {
			t.Fatal(err)
		}
		refused("a folder of another owner", "belongs to user 4242")
		if err := os.Chown(filepath.Dir(path), 0, 0); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log("not root: the owner of a file and of its folder are checked through SetProcessUID only")
	}

	// the folder may not be writable by the group or by others
	folder := filepath.Dir(path)
	for _, mode := range []os.FileMode{0o770, 0o702} {
		if err := os.Chmod(folder, mode); err != nil {
			t.Fatal(err)
		}
		refused("folder mode "+mode.String(), "chmod 700")
	}
	if err := os.Chmod(folder, 0o700); err != nil {
		t.Fatal(err)
	}

	// a symbolic link is no secret file, nor is a folder or a large file
	link := filepath.Join(folder, "ldap-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReadSecret(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a symbolic link: %v", err)
	}
	if err := os.Mkdir(filepath.Join(folder, "ldap-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReadSecret(filepath.Join(folder, "ldap-dir")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a folder: %v", err)
	}
	if err := state.WriteSecret(path, bytes.Repeat([]byte("x"), state.MaxSecretBytes+1)); err == nil {
		t.Error("a value above MaxSecretBytes is written")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), state.MaxSecretBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	refused("a large file", "larger than")
}

// WriteSecret refuses a secrets folder that others may write, before it
// writes a byte.
func TestSecretWriteRefusesAnOpenFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	folder := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(folder, 0o777); err != nil {
		t.Fatal(err)
	}
	path := state.LDAPSecretPath(dir, "demo-1")
	if err := state.WriteSecret(path, []byte(secretValue)); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("WriteSecret into a folder of mode 0777: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file was written: %v", err)
	}
}
