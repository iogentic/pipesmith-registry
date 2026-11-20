package maven

import (
	"archive/zip"
	"bytes"
	"fmt"
	"testing"
)

// countingContent is a Content of bytes in memory that counts what is read.
type countingContent struct {
	*bytes.Reader
	read int64
}

func (c *countingContent) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Reader.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

func (*countingContent) Close() error { return nil }

// pluginJar is a jar with the plugin descriptor and filler more entries
// named with pad bytes each.
func pluginJar(t *testing.T, filler, pad int) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for i := range filler {
		if _, err := z.Create(fmt.Sprintf("f/%0*d", pad, i)); err != nil {
			t.Fatal(err)
		}
	}
	w, err := z.Create(descriptorName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<plugin><goalPrefix>demo</goalPrefix></plugin>")); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func prefixOf(t *testing.T, jar []byte) (string, int64) {
	t.Helper()
	content := &countingContent{Reader: bytes.NewReader(jar)}
	src := Source{Open: func(string) (Content, int64, error) { return content, int64(len(jar)), nil }}
	prefix, err := goalPrefix(src, "a.jar")
	if err != nil {
		t.Fatal(err)
	}
	return prefix, content.read
}

// The central directory of a jar is read only when it holds at most
// maxJarEntries entries in at most maxJarDirectory bytes (N4); a larger one
// counts as no archive, and only the end of the file is read.
func TestGoalPrefixBoundsTheDirectory(t *testing.T) {
	entries, directory := maxJarEntries, maxJarDirectory
	t.Cleanup(func() { maxJarEntries, maxJarDirectory = entries, directory })
	maxJarEntries, maxJarDirectory = 100, 1<<20

	if prefix, _ := prefixOf(t, pluginJar(t, 99, 4)); prefix != "demo" {
		t.Fatalf("100 entries: prefix %q", prefix)
	}
	jar := pluginJar(t, 100, 4)
	if prefix, read := prefixOf(t, jar); prefix != "" || read > 66<<10 {
		t.Fatalf("101 entries: prefix %q, %d of %d bytes read", prefix, read, len(jar))
	}

	maxJarEntries = 1000
	small := pluginJar(t, 10, 4)
	maxJarDirectory = int64(len(small))
	if prefix, _ := prefixOf(t, small); prefix != "demo" {
		t.Fatalf("a small directory: prefix %q", prefix)
	}
	if prefix, _ := prefixOf(t, pluginJar(t, 10, len(small))); prefix != "" {
		t.Fatalf("a directory above %d bytes: prefix %q", maxJarDirectory, prefix)
	}
}

// A jar of more than 65,535 entries carries its count in the ZIP64 end
// record, which the bound reads too.
func TestGoalPrefixBoundsAZip64Directory(t *testing.T) {
	entries := maxJarEntries
	t.Cleanup(func() { maxJarEntries = entries })
	jar := pluginJar(t, 70_000, 5)
	maxJarEntries = 70_001
	if prefix, _ := prefixOf(t, jar); prefix != "demo" {
		t.Fatalf("70,001 entries under a bound of as many: prefix %q", prefix)
	}
	maxJarEntries = 70_000
	if prefix, read := prefixOf(t, jar); prefix != "" || read > 67<<10 {
		t.Fatalf("70,001 entries over a bound of 70,000: prefix %q, %d bytes read", prefix, read)
	}
}
