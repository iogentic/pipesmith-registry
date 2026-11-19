package migrate_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// walCopy is a valid database at version 2 in WAL mode with its -wal file,
// copied into a folder of its own while a connection held it open, and the
// bytes of that -wal file.
func walCopy(t *testing.T) (string, []byte) {
	t.Helper()
	src, db := at(t, 2)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t1 VALUES ('in the wal')"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.sqlite")
	for _, side := range []string{"", "-wal"} {
		in, err := os.Open(src + side)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(path + side)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil || len(wal) == 0 {
		t.Fatalf("no -wal file: %v", err)
	}
	return path, wal
}

// FileVersion opens nothing when <file>-wal or <file>-shm is no regular
// file: a link to a path outside the folder is neither created nor written.
func TestFileVersionRefusesLinkedSideFiles(t *testing.T) {
	path, wal := walCopy(t)
	if v, err := migrate.FileVersion(path); err != nil || v != 2 {
		t.Fatalf("the copy in WAL mode: %d %v", v, err)
	}
	for _, c := range []struct {
		name, side string
		existing   []byte // nil: the target does not exist
	}{
		{"shm to nothing", "-shm", nil},
		{"shm to a file", "-shm", []byte("bytes of another file\n")},
		{"wal to nothing", "-wal", nil},
		{"wal to a file", "-wal", wal},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, _ := walCopy(t)
			target := filepath.Join(t.TempDir(), "outside")
			if c.existing != nil {
				if err := os.WriteFile(target, c.existing, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(path + c.side); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path+c.side); err != nil {
				t.Fatal(err)
			}
			v, err := migrate.FileVersion(path)
			got, rerr := os.ReadFile(target)
			t.Logf("FileVersion: %d, error %v; target after: %d bytes, read error %v", v, err, len(got), rerr)
			if c.existing == nil {
				if _, err := os.Lstat(target); err == nil {
					t.Errorf("FileVersion created %s through the link %s%s", target, path, c.side)
				}
			} else if rerr != nil || string(got) != string(c.existing) {
				t.Errorf("FileVersion changed %s through the link %s%s", target, path, c.side)
			}
			if err == nil || !strings.Contains(err.Error(), path+c.side) {
				t.Errorf("FileVersion passed a linked %s: %d %v", c.side, v, err)
			}
		})
	}
}
