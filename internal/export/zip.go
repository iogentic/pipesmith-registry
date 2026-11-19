package export

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"time"
)

// Entry is one file of an archive. Size is the size of the index, which
// the limits count. Open is called once, when the entry is written, and
// returns the bytes and their size, which the entry must deliver exactly;
// the archive closes the reader.
type Entry struct {
	Name       string
	Size       int64
	ModifiedMS int64
	Open       func() (io.ReadCloser, int64, error)
}

// ErrSkip from Open leaves the entry out of the archive and goes on: the
// path has no bytes to export, as a GET of it answers 404.
var ErrSkip = errors.New("the entry has no bytes to export")

// SizeError is an entry whose reader gave another number of bytes than its
// size.
type SizeError struct {
	Name       string
	Want, Read int64
}

func (e *SizeError) Error() string {
	more := "at least "
	if e.Read <= e.Want {
		more = ""
	}
	return fmt.Sprintf("%s: the index says %d bytes, the store gave %s%d", e.Name, e.Want, more, e.Read)
}

// CheckNames is CheckEntryName for every entry, before any byte is written.
func CheckNames(entries []Entry) error {
	for _, e := range entries {
		if err := CheckEntryName(e.Name); err != nil {
			return err
		}
	}
	return nil
}

// WriteZip writes the entries in their order as one zip, an entry with a
// compressed ending stored and every other one deflated, each with its time.
// A name that CheckEntryName refuses fails before the first byte. An entry
// that cannot be read, or whose bytes differ in number from its size, ends
// the archive at once without its central directory, so that no tool takes
// the short archive for a whole one; that error is returned. An entry whose
// Open answers ErrSkip is left out.
func WriteZip(w io.Writer, entries []Entry) error {
	if err := CheckNames(entries); err != nil {
		return err
	}
	z := zip.NewWriter(w)
	for _, e := range entries {
		in, size, err := e.Open()
		if errors.Is(err, ErrSkip) {
			continue
		}
		if err != nil {
			return err // z is left without Close: no central directory
		}
		if err := writeEntry(z, e, in, size); err != nil {
			return err
		}
	}
	return z.Close()
}

// writeEntry writes the header and exactly size bytes of in, and closes in.
func writeEntry(z *zip.Writer, e Entry, in io.ReadCloser, size int64) error {
	defer in.Close()
	header := &zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: time.UnixMilli(e.ModifiedMS).UTC()}
	if Stored(e.Name) {
		header.Method = zip.Store
	}
	header.SetMode(0o644)
	out, err := z.CreateHeader(header)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return &SizeError{Name: e.Name, Want: size, Read: n}
	}
	return nil
}
