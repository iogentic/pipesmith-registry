package node_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/node"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func readFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, node.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The first start writes a random UUID into node-id with mode 0600; the
// second start on the same directory answers the same id.
func TestIDIsWrittenOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	first, err := node.ID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !uuidPattern.MatchString(first) {
		t.Fatalf("id %q is no lower case UUID of version 4", first)
	}
	info, err := os.Stat(filepath.Join(dir, node.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	if got := readFile(t, dir); got != first+"\n" {
		t.Fatalf("file %q, want the id and a line end", got)
	}
	second, err := node.ID(dir)
	if err != nil || second != first {
		t.Fatalf("second start: %q %v, want %q", second, err, first)
	}
	if other, _ := node.ID(t.TempDir()); other == first {
		t.Fatalf("two directories share the id %q", first)
	}
}

// A file that holds no UUID is refused and left as it is.
func TestMalformedFileIsRefused(t *testing.T) {
	for _, text := range []string{"", "not a uuid\n", "6B1D3C2E-8F4A-4C55-9E0F-2A7D91C4E5B3\n", "6b1d3c2e8f4a4c559e0f2a7d91c4e5b3"} {
		dir := t.TempDir()
		path := filepath.Join(dir, node.FileName)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := node.ID(dir)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("%q: %q %v, want an error that names %s", text, id, err, path)
		}
		if got := readFile(t, dir); got != text {
			t.Fatalf("%q: the file became %q", text, got)
		}
	}
}

// A hand written id of any UUID version is taken as it is, with or
// without a line end.
func TestWrittenIDIsTaken(t *testing.T) {
	dir := t.TempDir()
	const id = "6b1d3c2e-8f4a-1c55-0e0f-2a7d91c4e5b3"
	if err := os.WriteFile(filepath.Join(dir, node.FileName), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := node.ID(dir); err != nil || got != id {
		t.Fatalf("%q %v, want %q", got, err, id)
	}
}

func settingRow(t *testing.T, dir string) (string, bool) {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	value, found, err := st.Setting(node.SettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return "", false
	}
	var id string
	if err := json.Unmarshal(value, &id); err != nil {
		t.Fatal(err)
	}
	return id, true
}

func putRow(t *testing.T, dir string, value any) {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutSetting(node.SettingKey, value); err != nil {
		t.Fatal(err)
	}
}

func newState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// I20 of the phase 2 plan: with state.sqlite the id of the file is copied
// into state.settings at the first start, and from then on the row is the
// source.
func TestIDIsCopiedIntoState(t *testing.T) {
	dir := newState(t)
	const id = "6b1d3c2e-8f4a-4c55-9e0f-2a7d91c4e5b3"
	if err := os.WriteFile(filepath.Join(dir, node.FileName), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := node.ID(dir); err != nil || got != id {
		t.Fatalf("%q %v, want %q", got, err, id)
	}
	if row, found := settingRow(t, dir); !found || row != id {
		t.Fatalf("row %q %v, want %q", row, found, id)
	}
	// the row wins over a file changed later
	if err := os.WriteFile(filepath.Join(dir, node.FileName), []byte("0b1d3c2e-8f4a-4c55-9e0f-2a7d91c4e5b3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := node.ID(dir); err != nil || got != id {
		t.Fatalf("after a file change: %q %v, want the row %q", got, err, id)
	}
	// a missing file is written again from the row
	if err := os.Remove(filepath.Join(dir, node.FileName)); err != nil {
		t.Fatal(err)
	}
	if got, err := node.ID(dir); err != nil || got != id {
		t.Fatalf("without the file: %q %v, want the row %q", got, err, id)
	}
	if got := readFile(t, dir); got != id+"\n" {
		t.Fatalf("file %q, want the row", got)
	}
}

// A new id on a directory with state.sqlite goes into both places.
func TestNewIDGoesIntoFileAndState(t *testing.T) {
	dir := newState(t)
	id, err := node.ID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if row, found := settingRow(t, dir); !found || row != id {
		t.Fatalf("row %q %v, want %q", row, found, id)
	}
	if got := readFile(t, dir); got != id+"\n" {
		t.Fatalf("file %q, want %q", got, id)
	}
}

// A row that holds no UUID is refused.
func TestMalformedRowIsRefused(t *testing.T) {
	for _, value := range []any{"nope", 7, map[string]string{"id": "x"}} {
		dir := newState(t)
		putRow(t, dir, value)
		if id, err := node.ID(dir); err == nil || !strings.Contains(err.Error(), node.SettingKey) {
			t.Fatalf("%v: %q %v, want an error that names the row", value, id, err)
		}
	}
}
