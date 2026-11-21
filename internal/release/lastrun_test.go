//go:build linux || darwin

package release

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLastRunRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ReleasesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadLastRun(root); r != nil || err != nil {
		t.Fatalf("no file: %v %v", r, err)
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	if err := WriteLastRun(root, LastRun{Result: RunBack, Version: "0.3.0", Message: strings.Repeat("x", 3000)}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, ReleasesDir, LastRunFile))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v %v", info, err)
	}
	r, err := ReadLastRun(root)
	if err != nil || r.Result != RunBack || r.Version != "0.3.0" || len(r.Message) != 2000 || r.Time == "" {
		t.Fatalf("%+v %v", r, err)
	}
}
