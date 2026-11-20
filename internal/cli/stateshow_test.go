package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Review 3-15: state show only reads. A state file of an older schema stays
// as it is, byte for byte and without a copy beside it, and the command says
// the schema it found and that serve or another command that writes
// migrates the file.
func TestStateShowLeavesAnOlderSchema(t *testing.T) {
	dir := dataDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	released, err := os.ReadFile(filepath.Join("..", "..", "testdata", "state", "v1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	path := state.Path(dir)
	if err := os.WriteFile(path, released, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "state", "show")
	if code != 1 || out != "" || !strings.Contains(errOut, "schema 1") ||
		!strings.Contains(errOut, "schema "+strconv.Itoa(state.SchemaVersion)) || !strings.Contains(errOut, "serve") {
		t.Fatalf("state show of schema 1: %d %q %q", code, out, errOut)
	}
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(now, released) {
		t.Fatal("state show changed the file of schema 1")
	}
	if _, err := os.Stat(path + ".v1"); !os.IsNotExist(err) {
		t.Fatalf("state show wrote the copy of schema 1: %v", err)
	}
	// a command that writes migrates it; state show reads it after that
	if code, out, errOut := withInput(t, "", "anonymous", "off"); code != 0 {
		t.Fatalf("anonymous off: %d %q %q", code, out, errOut)
	}
	if code, out, errOut := run(t, "state", "show"); code != 0 || !strings.Contains(out, `"schema": `+strconv.Itoa(state.SchemaVersion)) {
		t.Fatalf("state show after the migration: %d %q %q", code, out, errOut)
	}
}
