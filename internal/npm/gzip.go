package npm

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math/big"
)

// pyError is an exception of Python's tarfile or gzip module inside
// PackageManifest. Class is the Python class name.
type pyError struct {
	class, msg string
}

func (e *pyError) Error() string { return e.class + ": " + e.msg }

func newError(class, msg string) error { return &pyError{class, msg} }

func classOf(err error) string {
	if e, ok := err.(*pyError); ok {
		return e.class
	}
	return ""
}

const (
	gzHeader = iota // the next read starts a member
	gzData          // inside the deflate data of a member
	gzEnded         // the deflate data ended, the trailer is not read yet
	gzEOF
)

// gzipReader reads a gzip stream as gzip.GzipFile of Python 3.12 does:
// members one after another, zero bytes after a trailer skipped, the header
// CRC and the reserved flags ignored. The trailer of a member is checked
// only when a read asks for data past its end, so bytes after the last
// member that no read reaches stay unchecked.
type gzipReader struct {
	src     *bufio.Reader
	state   int
	inflate io.ReadCloser
	crc     uint32
	size    uint32
	err     error
}

func newGzipReader(data []byte) *gzipReader {
	return &gzipReader{src: bufio.NewReader(bytes.NewReader(data)), state: gzHeader}
}

func (z *gzipReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for z.err == nil {
		switch z.state {
		case gzEOF:
			return 0, io.EOF
		case gzEnded:
			z.err = z.trailer()
			z.state = gzHeader
		case gzHeader:
			more, err := z.header()
			switch {
			case err != nil:
				z.err = err
			case !more:
				z.state = gzEOF
			default:
				z.crc, z.size = 0, 0
				z.inflate = flate.NewReader(z.src)
				z.state = gzData
			}
		case gzData:
			n, err := z.inflate.Read(p)
			z.crc = crc32.Update(z.crc, crc32.IEEETable, p[:n])
			z.size += uint32(n) // #nosec G115 -- ISIZE of gzip is the length modulo 2^32, the wrap is intended; n of a Reader is never negative.
			switch {
			case err == io.EOF:
				z.state = gzEnded
			case err == io.ErrUnexpectedEOF:
				z.err = newError("EOFError", "Compressed file ended before the end-of-stream marker was reached")
			case err != nil:
				z.err = newError("error", "Error -3 while decompressing data: "+err.Error())
			}
			if n > 0 {
				return n, nil
			}
		}
	}
	return 0, z.err
}

func (z *gzipReader) exact(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(z.src, b); err != nil {
		return nil, newError("EOFError", "Compressed file ended before the end-of-stream marker was reached")
	}
	return b, nil
}

// header is _read_gzip_header: false at the end of the input.
func (z *gzipReader) header() (bool, error) {
	magic := make([]byte, 2)
	n, _ := io.ReadFull(z.src, magic)
	if n == 0 {
		return false, nil
	}
	if n < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return false, newError("BadGzipFile", "Not a gzipped file")
	}
	head, err := z.exact(8)
	if err != nil {
		return false, err
	}
	if head[0] != 8 {
		return false, newError("BadGzipFile", "Unknown compression method")
	}
	flag := head[1]
	if flag&4 != 0 {
		extra, err := z.exact(2)
		if err != nil {
			return false, err
		}
		if _, err := z.exact(int(binary.LittleEndian.Uint16(extra))); err != nil {
			return false, err
		}
	}
	for _, bit := range []byte{8, 16} { // a file name, then a comment, each up to a NUL
		if flag&bit == 0 {
			continue
		}
		for {
			c, err := z.src.ReadByte()
			if err != nil || c == 0 {
				break
			}
		}
	}
	if flag&2 != 0 {
		if _, err := z.exact(2); err != nil {
			return false, err
		}
	}
	return true, nil
}

// trailer is _read_eof: CRC and length of the member, then any zero bytes.
func (z *gzipReader) trailer() error {
	t, err := z.exact(8)
	if err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(t[:4]) != z.crc {
		return newError("BadGzipFile", "CRC check failed")
	}
	if binary.LittleEndian.Uint32(t[4:]) != z.size {
		return newError("BadGzipFile", "Incorrect length of data produced")
	}
	for {
		c, err := z.src.ReadByte()
		if err != nil {
			return nil
		}
		if c != 0 {
			return z.src.UnreadByte()
		}
	}
}

// stream is the GzipFile that tarfile reads: a position in the
// decompressed data, reads that stop short at the end, and seeks; a seek
// backwards decompresses again from the start, as Python does.
type stream struct {
	data []byte
	z    *gzipReader
	pos  int64
}

func newStream(data []byte) *stream {
	return &stream{data: data, z: newGzipReader(data)}
}

// read is GzipFile.read(n): up to n bytes, fewer only at the end.
func (s *stream) read(n int64) ([]byte, error) {
	var out []byte
	buf := make([]byte, min(n, 64*1024))
	for int64(len(out)) < n {
		chunk := buf[:min(int64(len(buf)), n-int64(len(out)))]
		k, err := s.z.Read(chunk)
		out = append(out, chunk[:k]...)
		s.pos += int64(k)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// seek is GzipFile.seek(target): a target outside the 64 bit range is the
// ValueError of the buffered reader, one past the end stops at the end, a
// negative one rewinds to the start.
func (s *stream) seek(target *big.Int) error {
	if !target.IsInt64() {
		return newError("ValueError", "cannot fit 'int' into an offset-sized integer")
	}
	t := target.Int64()
	if t < s.pos {
		s.z, s.pos = newGzipReader(s.data), 0
	}
	buf := make([]byte, 32*1024)
	for s.pos < t {
		k, err := s.z.Read(buf[:min(int64(len(buf)), t-s.pos)])
		s.pos += int64(k)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
