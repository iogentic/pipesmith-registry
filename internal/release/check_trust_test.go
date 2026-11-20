//go:build unix

package release

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// CheckTrust applies the root and key rule with the owner of the root, for
// a reader that is not that owner, such as serve.
func TestCheckTrust(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ReleasesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "trusted.pub")
	if err := os.WriteFile(key, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(root, key); err != nil {
		t.Fatalf("good root and key refused: %v", err)
	}
	if err := os.Chmod(key, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(root, key); !errors.Is(err, ErrPermissions) {
		t.Fatalf("key writable by others: %v, want ErrPermissions", err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, ReleasesDir), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(root, key); !errors.Is(err, ErrPermissions) {
		t.Fatalf("releases/ writable by others: %v, want ErrPermissions", err)
	}
}

// LastSwitch is the newest line of releases/history.jsonl whose switch
// ended healthy; refusals and ways back after it do not count.
func TestLastSwitch(t *testing.T) {
	dir := t.TempDir()
	if e, err := LastSwitch(dir); err != nil || e != nil {
		t.Fatalf("no history: %+v, %v", e, err)
	}
	for _, e := range []HistoryEntry{
		{ID: "1", Time: "t1", Action: "update", Result: ResultOK, From: "0.1.3", To: "0.2.0"},
		{ID: "2", Time: "t2", Action: "update", Result: ResultOK, From: "0.2.0", To: "0.2.1"},
		{ID: "3", Time: "t3", Action: "update", Result: ResultRefused, From: "0.2.1", To: "0.2.2"},
		{ID: "4", Time: "t4", Action: "update", Result: ResultBack, From: "0.2.1", To: "0.2.3"},
	} {
		if err := appendHistory(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	e, err := LastSwitch(dir)
	if err != nil || e == nil || e.ID != "2" || e.To != "0.2.1" {
		t.Fatalf("last switch %+v, %v; want the line with id 2", e, err)
	}
}
