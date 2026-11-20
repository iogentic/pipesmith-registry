//go:build linux || darwin

package release

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// releases/history.jsonl has mode 0644 whatever the umask of the caller, so
// serve, which runs as the service user, can read it (finding R3-9).
func TestHistoryModeIgnoresTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	if err := appendHistory(dir, newEntry("update", ResultOK, "0.1.0", "0.2.0", "r")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, HistoryFile)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendHistory(dir, newEntry("update", ResultOK, "0.2.0", "0.3.0", "r")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", info.Mode().Perm())
	}
}
