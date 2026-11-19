package export

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func fixed(data string) func() (io.ReadCloser, int64, error) {
	return sized(data, int64(len(data)))
}

// sized delivers data and claims size.
func sized(data string, size int64) func() (io.ReadCloser, int64, error) {
	return func() (io.ReadCloser, int64, error) { return io.NopCloser(strings.NewReader(data)), size, nil }
}

type failingReader struct{ after int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("the store went away")
	}
	n := min(len(p), f.after)
	for i := range p[:n] {
		p[i] = 'x'
	}
	f.after -= n
	return n, nil
}

func zipEntries() []Entry {
	big := strings.Repeat("compressible text ", 4000)
	return []Entry{
		{Name: "com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom", Size: int64(len(big)), ModifiedMS: 1_700_000_000_000, Open: fixed(big)},
		{Name: "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar", Size: 9, ModifiedMS: 1_700_000_001_000, Open: fixed("jar bytes")},
		{Name: "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar.sha1", Size: 4, ModifiedMS: 1_700_000_002_000, Open: fixed("abcd")},
		{Name: "docs/empty.txt", Size: 0, ModifiedMS: 1_600_000_000_000, Open: fixed("")},
		{Name: "docs/pkg.TGZ", Size: 3, ModifiedMS: 1_600_000_000_000, Open: fixed("tgz")},
	}
}

func TestWriteZipListingEqualsInput(t *testing.T) {
	entries := zipEntries()
	var out bytes.Buffer
	if err := WriteZip(&out, entries); err != nil {
		t.Fatal(err)
	}
	r, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.File) != len(entries) {
		t.Fatalf("%d entries, want %d", len(r.File), len(entries))
	}
	for i, f := range r.File {
		e := entries[i]
		if f.Name != e.Name || int64(f.UncompressedSize64) != e.Size {
			t.Errorf("entry %d: %s %d, want %s %d", i, f.Name, f.UncompressedSize64, e.Name, e.Size)
		}
		if want := time.UnixMilli(e.ModifiedMS).UTC(); !f.Modified.UTC().Equal(want) {
			t.Errorf("%s: time %v, want %v", f.Name, f.Modified.UTC(), want)
		}
		method := zip.Deflate
		if Stored(e.Name) {
			method = zip.Store
		}
		if f.Method != method {
			t.Errorf("%s: method %d, want %d", f.Name, f.Method, method)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		src, _, _ := e.Open()
		want, _ := io.ReadAll(src)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: other bytes", f.Name)
		}
	}
	if r.File[0].CompressedSize64 >= r.File[0].UncompressedSize64 {
		t.Errorf("the pom was not compressed: %d of %d", r.File[0].CompressedSize64, r.File[0].UncompressedSize64)
	}
	var again bytes.Buffer
	if err := WriteZip(&again, zipEntries()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Fatal("two runs gave different bytes")
	}
}

func TestWriteZipReaderFails(t *testing.T) {
	entries := zipEntries()
	entries[1].Size = 100_000
	entries[1].Open = func() (io.ReadCloser, int64, error) { return io.NopCloser(&failingReader{after: 50_000}), 100_000, nil }
	var out bytes.Buffer
	err := WriteZip(&out, entries)
	if err == nil || !strings.Contains(err.Error(), "the store went away") {
		t.Fatalf("err %v, want the error of the reader", err)
	}
	if out.Len() == 0 {
		t.Fatal("nothing written before the failure")
	}
	if _, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); err == nil {
		t.Fatal("a cut archive opens as a zip")
	}
	opened := errors.New("cannot open")
	entries = zipEntries()
	entries[2].Open = func() (io.ReadCloser, int64, error) { return nil, 0, opened }
	out.Reset()
	if err := WriteZip(&out, entries); !errors.Is(err, opened) {
		t.Fatalf("err %v, want the error of Open", err)
	}
	if _, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); err == nil {
		t.Fatal("a cut archive opens as a zip")
	}
}

func TestWriteZipSizeMismatch(t *testing.T) {
	for _, size := range []int64{8, 10} { // the bytes are "jar bytes", 9 of them
		entries := zipEntries()
		entries[1].Open = sized("jar bytes", size)
		var out bytes.Buffer
		var mismatch *SizeError
		if err := WriteZip(&out, entries); !errors.As(err, &mismatch) || mismatch.Name != entries[1].Name {
			t.Fatalf("size %d: err %v, want a SizeError", size, err)
		}
		if _, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); err == nil {
			t.Fatalf("size %d: a cut archive opens as a zip", size)
		}
	}
}

func TestWriteZipRefusesABadNameBeforeTheFirstByte(t *testing.T) {
	entries := zipEntries()
	entries = append(entries, Entry{Name: "docs/../../etc/passwd", Size: 1, Open: fixed("x")})
	var out bytes.Buffer
	var bad *BadNameError
	if err := WriteZip(&out, entries); !errors.As(err, &bad) {
		t.Fatalf("err %v, want a BadNameError", err)
	}
	if out.Len() != 0 {
		t.Fatalf("%d bytes written before the refusal", out.Len())
	}
	// the positive twin: the same list without the bad name is written whole
	out.Reset()
	if err := WriteZip(&out, entries[:len(entries)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); err != nil {
		t.Fatal(err)
	}
}

// An entry whose Open answers ErrSkip stands nowhere in the archive, and the
// archive stays whole with every other entry.
func TestWriteZipLeavesOutASkippedEntry(t *testing.T) {
	entries := zipEntries()
	entries[1].Open = func() (io.ReadCloser, int64, error) { return nil, 0, ErrSkip }
	var out bytes.Buffer
	if err := WriteZip(&out, entries); err != nil {
		t.Fatalf("err %v, want a whole archive", err)
	}
	r, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	want := []string{entries[0].Name, entries[2].Name, entries[3].Name, entries[4].Name}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("entries %q, want %q", names, want)
	}
	// the positive twin: an error other than ErrSkip still ends the archive
	entries[1].Open = func() (io.ReadCloser, int64, error) { return nil, 0, errors.New("gone") }
	out.Reset()
	if err := WriteZip(&out, entries); err == nil {
		t.Fatal("an entry that is gone did not end the archive")
	}
}
