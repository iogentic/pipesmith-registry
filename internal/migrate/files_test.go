package migrate_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

func sum(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func TestFileVersion(t *testing.T) {
	dir := t.TempDir()
	if v, err := migrate.FileVersion(filepath.Join(dir, "none.sqlite")); err != nil || v != 0 {
		t.Fatalf("missing file: %d %v", v, err)
	}
	for _, n := range []int{1, 3} {
		path, db := at(t, n)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := sum(t, path)
		if v, err := migrate.FileVersion(path); err != nil || v != n {
			t.Fatalf("version %d: %d %v", n, v, err)
		}
		if sum(t, path) != before {
			t.Fatal("FileVersion changed the file")
		}
		for _, side := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(path + side); err == nil {
				t.Fatalf("FileVersion left %s%s", path, side)
			}
		}
	}
	// A file another connection holds open, inside a write transaction.
	path, db := at(t, 2)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO t1 VALUES ('open')"); err != nil {
		t.Fatal(err)
	}
	if v, err := migrate.FileVersion(path); err != nil || v != 2 {
		t.Fatalf("held open: %d %v", v, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := migrate.FileVersion(empty); err != nil || v != 0 {
		t.Fatalf("empty file: %d %v", v, err)
	}
	text := filepath.Join(dir, "text.sqlite")
	if err := os.WriteFile(text, bytes.Repeat([]byte("not a database "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.FileVersion(text); err == nil {
		t.Fatal("a text file has a version")
	}
	if _, err := migrate.FileVersion(dir); err == nil {
		t.Fatal("a directory has a version")
	}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Enabled(context.Context, slog.Level) bool { return true }
func (l *logLines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, r.Level.String()+" "+r.Message)
	return nil
}
func (l *logLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logLines) WithGroup(string) slog.Handler      { return l }

func capture(t *testing.T) *logLines {
	t.Helper()
	l := &logLines{}
	previous := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return l
}

// CopyWhen of SpareCopy writes the copy when the free space is at least
// twice the file size, and migrates without a copy otherwise.
func TestSpareCopy(t *testing.T) {
	for _, c := range []struct {
		name   string
		free   func(size int64) (uint64, error)
		copied bool
		line   string
	}{
		{"twice", func(size int64) (uint64, error) { return uint64(2 * size), nil }, true, "INFO "},
		{"more", func(size int64) (uint64, error) { return 1 << 40, nil }, true, "INFO "},
		{"less", func(size int64) (uint64, error) { return uint64(2*size - 1), nil }, false, "WARN "},
		{"unknown", func(int64) (uint64, error) { return 0, errors.New("no statfs") }, false, "WARN "},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, db := at(t, 1)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			var asked []string
			free := func(dir string) (uint64, error) { asked = append(asked, dir); return c.free(info.Size()) }
			log := capture(t)
			var l ledger
			got, err := migrate.Run(db, path, l.steps(3), migrate.Options{Kind: kind, CopyWhen: migrate.SpareCopy(free)})
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(path + ".v1")
			if c.copied != (got.Copy == path+".v1") || c.copied != (statErr == nil) || got.To != 3 {
				t.Fatalf("result %+v, copy %v", got, statErr)
			}
			if len(log.lines) != 1 || !strings.HasPrefix(log.lines[0], c.line+path+" migrates") {
				t.Fatalf("log %q", log.lines)
			}
			if c.name != "unknown" && (len(asked) != 1 || asked[0] != filepath.Dir(path)) {
				t.Fatalf("asked %v", asked)
			}
		})
	}
	// Not asked for a new file, a file at the last version, nor with Backup.
	for _, start := range []int{0, 3} {
		path := filepath.Join(t.TempDir(), "x.sqlite")
		db := open(t, path)
		var l ledger
		if start > 0 {
			path, db = at(t, start)
		}
		asked := 0
		when := func(string) bool { asked++; return true }
		if _, err := migrate.Run(db, path, l.steps(3), migrate.Options{Kind: kind, Create: true, CopyWhen: when}); err != nil {
			t.Fatal(err)
		}
		if asked != 0 {
			t.Fatalf("start %d: asked %d times", start, asked)
		}
	}
	path, db := at(t, 1)
	var l ledger
	got, err := migrate.Run(db, path, l.steps(2), migrate.Options{Kind: kind, Backup: true,
		CopyWhen: func(string) bool { t.Error("asked with Backup"); return false }})
	if err != nil || got.Copy != path+".v1" {
		t.Fatalf("Backup: %+v %v", got, err)
	}
}

func TestFreeSpace(t *testing.T) {
	free, err := migrate.FreeSpace(t.TempDir())
	if err != nil || free == 0 {
		t.Fatalf("free %d, %v", free, err)
	}
	if _, err := migrate.FreeSpace(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("free space of a missing folder")
	}
}
