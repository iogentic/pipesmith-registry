package logs

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// The tail of a compressed file stops at maxUncompressed bytes of content, so
// a small file that inflates without end costs bounded time.
func TestTailOfACompressedFileIsBounded(t *testing.T) {
	old := maxUncompressed
	maxUncompressed = 1 << 20
	t.Cleanup(func() { maxUncompressed = old })
	dir := t.TempDir()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(bytes.Repeat([]byte("a"), 4<<20)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	name := "server-2026-11-20.1.log.gz"
	if err := os.WriteFile(filepath.Join(dir, name), gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFile(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Tail(100); err == nil {
		t.Fatalf("the tail of %d bytes of content passed a bound of %d", 4<<20, maxUncompressed)
	}
	// a file inside the bound still gives its tail
	var small bytes.Buffer
	w = gzip.NewWriter(&small)
	_, _ = w.Write([]byte("one\ntwo\n"))
	_ = w.Close()
	name = "server-2026-11-20.2.log.gz"
	if err := os.WriteFile(filepath.Join(dir, name), small.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := OpenFile(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	tail, err := g.Tail(4)
	if err != nil || string(tail) != "two\n" {
		t.Fatalf("tail %q, %v; want \"two\\n\"", tail, err)
	}
}
