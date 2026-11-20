package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/state"
)

func TestLogTargetSecretPath(t *testing.T) {
	dir := filepath.Join("/srv", "data")
	for name, want := range map[string]string{
		"demo-loki": "log-target-demo-loki",
		"demo_2":    "log-target-demo_2",
		"a/b":       "log-target-a%2Fb",
		"..":        "log-target-%2E.",
	} {
		if got := state.LogTargetSecretPath(dir, name); got != filepath.Join(dir, "secrets", want) {
			t.Errorf("LogTargetSecretPath(%q) = %q, want secrets/%s", name, got, want)
		}
	}
}

// RemoveSecret deletes the file; a missing file is no error.
func TestRemoveSecret(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := state.LogTargetSecretPath(dir, "demo-hec")
	if err := state.RemoveSecret(path); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if err := state.WriteSecret(path, []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	if err := state.RemoveSecret(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still there: %v", err)
	}
	if _, err := state.ReadSecret(path); !errors.Is(err, state.ErrNoSecret) {
		t.Fatalf("read after remove: %v", err)
	}
}
