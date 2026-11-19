package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHoldRoot(t *testing.T) {
	l := installRoot(t, "0.1.0")
	key := newSigner(t).pub
	writeConfig(t, l.Root, `{"trusted_key": "`+key+`", "channel": {"owner": "acme"}}`)
	before := tree(t, l.Root)
	h, err := HoldRoot(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := h.Config.Member("channel")
	if h.Current != "0.1.0" || h.Config.TrustedKey != key || !ok || string(raw) != `{"owner": "acme"}` {
		t.Fatalf("hold %+v, channel %s", h, raw)
	}
	if _, ok := h.Config.Member("nothing"); ok {
		t.Fatal("a missing key is a member")
	}
	if _, err := HoldRoot(l.Root); !errors.Is(err, ErrLocked) {
		t.Fatalf("second hold: %v", err)
	}
	u := &Updater{Root: l.Root}
	if err := u.Rollback(); !errors.Is(err, ErrLocked) {
		t.Fatalf("rollback under a hold: %v", err)
	}
	h.Release()
	h, err = HoldRoot(l.Root)
	if err != nil {
		t.Fatalf("hold after the release: %v", err)
	}
	h.Release()
	if after := tree(t, l.Root); after != before {
		t.Fatalf("a hold changed the tree:\n%s\n%s", before, after)
	}
}

func TestHoldRootRefusals(t *testing.T) {
	l := installRoot(t, "0.1.0")
	writeConfig(t, l.Root, `{}`)
	_, err := holdRoot(l.Root, func() int { return 4242 }, fileOwner, lstatFile, nil)
	if !errors.Is(err, ErrOwner) || !strings.Contains(err.Error(), "runs as uid 4242") {
		t.Fatalf("other uid: %v", err)
	}
	_, err = holdRoot(l.Root, func() int { return 7 }, func(string) (int, error) { return 0, fmt.Errorf("no stat") }, lstatFile, nil)
	if !errors.Is(err, ErrOwner) {
		t.Fatalf("no owner: %v", err)
	}

	empty := installRoot(t)
	writeConfig(t, empty.Root, `{}`)
	_, err = HoldRoot(empty.Root)
	if !errors.Is(err, ErrLayout) || !strings.Contains(err.Error(), "installed by hand") {
		t.Fatalf("no current: %v", err)
	}
	if h, err := HoldRoot(empty.Root); h != nil || err == nil {
		t.Fatal("the lock of a refused hold was kept or the hold passed")
	}

	if err := os.Remove(filepath.Join(l.Root, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := HoldRoot(l.Root); !errors.Is(err, ErrConfig) {
		t.Fatalf("no update.json: %v", err)
	}
	if _, err := HoldRoot(filepath.Join(t.TempDir(), "none")); !errors.Is(err, ErrOwner) {
		t.Fatalf("no root: %v", err)
	}
}

func TestRunVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.WriteFile(bin, versionScript("1.2.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := RunVersion(bin, 0)
	if err != nil || v != (Version{1, 2, 3}) {
		t.Fatalf("%v %v", v, err)
	}
	if err := os.WriteFile(bin, script(`echo "pipesmith-registry 1.2"; exit 0`), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RunVersion(bin, 0); !errors.Is(err, ErrVersion) {
		t.Fatalf("bad answer: %v", err)
	}
}
