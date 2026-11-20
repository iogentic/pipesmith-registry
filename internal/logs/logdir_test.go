package logs

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestServedNames(t *testing.T) {
	for _, name := range []string{"server.log", "requests.log", "requests-classic.log", "audit.log", "tasks.log",
		"server-2026-11-19.1.log", "audit-2026-11-19.12.log.gz", "requests-classic-2026-01-31.3.log.gz"} {
		if !ServedName(name) {
			t.Errorf("%s is not served", name)
		}
	}
	for _, name := range []string{"", "logs.lock", "audit-pending", "server.log.gz", "server-2026-11-19.1.log.gz.tmp",
		"../server.log", "/server.log", "sub/server.log", "server.log/", "./server.log", "server-2026-13-40.1.log",
		"other.log", "server.log\x00", "SERVER.log", `..\server.log`} {
		if ServedName(name) {
			t.Errorf("%q is served", name)
		}
	}
}

func putFile(t *testing.T, path string, data []byte, at time.Time) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if _, err := z.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestListFilesShowsOnlyTheLogFiles(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 20, 8, 30, 0, 0, time.UTC)
	putFile(t, filepath.Join(dir, "server.log"), []byte("abc\n"), at)
	putFile(t, filepath.Join(dir, "audit-2026-11-19.1.log.gz"), gzipped(t, []byte("x\n")), at)
	putFile(t, filepath.Join(dir, "tasks-2026-11-19.1.log.gz.tmp"), []byte("half"), at)
	putFile(t, filepath.Join(dir, "logs.lock"), nil, at)
	putFile(t, filepath.Join(dir, "notes.txt"), []byte("n"), at)
	if err := os.Mkdir(filepath.Join(dir, "audit-pending"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "tasks.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	putFile(t, outside, []byte("secret"), at)
	if err := os.Symlink(outside, filepath.Join(dir, "requests.log")); err != nil {
		t.Fatal(err)
	}
	files, err := ListFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"audit-2026-11-19.1.log.gz", "server.log"}) {
		t.Fatalf("listed %v", names)
	}
	if files[1].Size != 4 || !files[1].Modified.Equal(at) {
		t.Errorf("server.log: %+v", files[1])
	}
	if files, err := ListFiles(""); err != nil || len(files) != 0 {
		t.Errorf("files off: %v %v", files, err)
	}
	if files, err := ListFiles(filepath.Join(dir, "missing")); err != nil || len(files) != 0 {
		t.Errorf("no directory: %v %v", files, err)
	}
}

func TestOpenFileRefusesWhatIsNotALogFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	putFile(t, outside, []byte("secret"), time.Now())
	if err := os.Symlink(outside, filepath.Join(dir, "requests.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "tasks.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"requests.log", "tasks.log", "../secret.txt", "/etc/hosts"} {
		if f, err := OpenFile(dir, name); !errors.Is(err, ErrName) {
			t.Errorf("%s: %v", name, err)
			if f != nil {
				f.Close()
			}
		}
	}
	if _, err := OpenFile(dir, "audit.log"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := OpenFile("", "audit.log"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("files off: %v", err)
	}
}

func TestTailGivesTheLastBytes(t *testing.T) {
	dir := t.TempDir()
	plain := bytes.Repeat([]byte("0123456789"), 300) // 3,000 bytes
	putFile(t, filepath.Join(dir, "server.log"), plain, time.Now())
	putFile(t, filepath.Join(dir, "server-2026-11-19.1.log.gz"), gzipped(t, plain), time.Now())
	for _, name := range []string{"server.log", "server-2026-11-19.1.log.gz"} {
		for _, n := range []int64{1, 25, 2999, 3000, 3001, 100000} {
			f, err := OpenFile(dir, name)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.Tail(n)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := plain[max(0, int64(len(plain))-n):]
			if !bytes.Equal(got, want) {
				t.Errorf("%s tail %d: %d bytes, want %d", name, n, len(got), len(want))
			}
		}
	}
}

func TestOpenFileReadsTheWholeFileForADownload(t *testing.T) {
	dir := t.TempDir()
	data := gzipped(t, []byte("line\n"))
	putFile(t, filepath.Join(dir, "audit-2026-11-19.1.log.gz"), data, time.Now())
	f, err := OpenFile(dir, "audit-2026-11-19.1.log.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Size != int64(len(data)) || !f.Compressed {
		t.Errorf("%+v", f)
	}
	got, err := io.ReadAll(f.Reader())
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("download: %v, %d bytes", err, len(got))
	}
}
