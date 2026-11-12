package manifest

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
)

// gzipReader reads a gzip file as gzip.open(part, "rb") of Python 3.12
// does: members one after the other, zero bytes after a member skipped, the
// header CRC not checked, and the errors of _GzipReader with their texts.
// compress/gzip differs in these points, so the members are read here on
// top of compress/flate.
type gzipReader struct {
	src     *bufio.Reader
	path    string
	inflate io.ReadCloser // of the current member, nil between members
	crc     uint32
	size    uint32
	ended   bool // a member has ended, so zero padding may follow
	err     error
}

const (
	gzipHCRC    = 1 << 1
	gzipExtra   = 1 << 2
	gzipName    = 1 << 3
	gzipComment = 1 << 4
)

func newGzipReader(r io.Reader, path string) *gzipReader {
	return &gzipReader{src: bufio.NewReaderSize(r, 128*1024), path: path}
}

// endsEarly is the EOFError of _GzipReader as _lines rewrites it.
func (g *gzipReader) endsEarly() error {
	return newError("OSError", "%s ends early: Compressed file ended before the end-of-stream marker was reached", g.path)
}

func (g *gzipReader) Read(p []byte) (int, error) {
	for {
		if g.err != nil {
			return 0, g.err
		}
		if g.inflate == nil {
			more, err := g.header()
			if err != nil {
				g.err = err
				continue
			}
			if !more {
				g.err = io.EOF
				continue
			}
		}
		n, err := g.inflate.Read(p)
		g.crc = crc32.Update(g.crc, crc32.IEEETable, p[:n])
		g.size += uint32(n) // #nosec G115 -- ISIZE of gzip is the length modulo 2^32, the wrap is intended; n of a Reader is never negative.
		switch {
		case errors.Is(err, io.EOF):
			g.inflate.Close()
			g.inflate = nil
			if err := g.trailer(); err != nil {
				g.err = err
			}
		case errors.Is(err, io.ErrUnexpectedEOF):
			g.err = g.endsEarly()
		case err != nil:
			// zlib.error in Python, which is no OSError.
			g.err = newError("error", "Error -3 while decompressing data: %s", strings.TrimPrefix(err.Error(), "flate: "))
		}
		if n > 0 {
			return n, nil
		}
	}
}

// header reads the header of the next member; false at the end of the file.
func (g *gzipReader) header() (bool, error) {
	if g.ended {
		for {
			c, err := g.src.ReadByte()
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if c != 0 {
				g.src.UnreadByte()
				break
			}
		}
	}
	magic, err := g.readUpTo(2)
	if err != nil {
		return false, err
	}
	if len(magic) == 0 {
		return false, nil
	}
	if string(magic) != "\x1f\x8b" {
		return false, newError("OSError", "Not a gzipped file (%s)", bytesRepr(magic))
	}
	fixed, err := g.readExact(8)
	if err != nil {
		return false, err
	}
	if fixed[0] != 8 {
		return false, newError("OSError", "Unknown compression method")
	}
	flags := fixed[1]
	if flags&gzipExtra != 0 {
		length, err := g.readExact(2)
		if err != nil {
			return false, err
		}
		if _, err := g.readExact(int(binary.LittleEndian.Uint16(length))); err != nil {
			return false, err
		}
	}
	for _, flag := range []byte{gzipName, gzipComment} {
		if flags&flag == 0 {
			continue
		}
		// A string ended by NUL; the end of the file ends it as well.
		for {
			c, err := g.src.ReadByte()
			if errors.Is(err, io.EOF) || err == nil && c == 0 {
				break
			}
			if err != nil {
				return false, err
			}
		}
	}
	if flags&gzipHCRC != 0 {
		if _, err := g.readExact(2); err != nil {
			return false, err
		}
	}
	g.crc, g.size = 0, 0
	g.inflate = flate.NewReader(g.src)
	return true, nil
}

// trailer checks CRC and size of the member that ended, as _read_eof does.
func (g *gzipReader) trailer() error {
	g.ended = true
	t, err := g.readExact(8)
	if err != nil {
		return err
	}
	stored, size := binary.LittleEndian.Uint32(t[:4]), binary.LittleEndian.Uint32(t[4:])
	if stored != g.crc {
		return newError("OSError", "CRC check failed %#x != %#x", stored, g.crc)
	}
	if size != g.size {
		return newError("OSError", "Incorrect length of data produced")
	}
	return nil
}

func (g *gzipReader) readUpTo(n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(g.src, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return buf[:got], err
}

func (g *gzipReader) readExact(n int) ([]byte, error) {
	buf, err := g.readUpTo(n)
	if err != nil {
		return nil, err
	}
	if len(buf) < n {
		return nil, g.endsEarly()
	}
	return buf, nil
}

// bytesRepr is repr() of a Python bytes object.
func bytesRepr(b []byte) string {
	quote := byte('\'')
	if strings.IndexByte(string(b), '\'') >= 0 && strings.IndexByte(string(b), '"') < 0 {
		quote = '"'
	}
	var out strings.Builder
	out.WriteString("b")
	out.WriteByte(quote)
	for _, c := range b {
		switch {
		case c == quote || c == '\\':
			out.WriteByte('\\')
			out.WriteByte(c)
		case c == '\t':
			out.WriteString(`\t`)
		case c == '\n':
			out.WriteString(`\n`)
		case c == '\r':
			out.WriteString(`\r`)
		case c < ' ' || c >= 0x7F:
			fmt.Fprintf(&out, `\x%02x`, c)
		default:
			out.WriteByte(c)
		}
	}
	out.WriteByte(quote)
	return out.String()
}
