package npm

import (
	"bytes"
	"math"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// maxHeaderChain is the longest chain of extended headers (pax "x", "g",
// "X", GNU "L", "K") before one member that tarfile reads when server.py
// calls package_manifest: each header nests three Python frames, and one
// more ends in RecursionError, which package_manifest catches. Measured on
// 2026-11-12 with tools/vectors/npm.py at the depth of the server's request
// thread (10 frames, recursion limit 1000); see the pax-chain cases of
// testdata/vectors/npm.json.
const maxHeaderChain = 326

const blockSize = 512

var (
	maxInt64 = big.NewInt(math.MaxInt64)
	bigBlock = big.NewInt(blockSize)
)

// PackageManifest is package_manifest: the package/package.json object of
// a gzip tarball, or nil when it has none. The tarball is read as Python's
// tarfile reads it (the last member of the name wins, a damaged header
// after the first ends the archive), and the member is read only up to
// MaxPackageJSONBytes. The error is an exception package_manifest does not
// catch (IndexError, OverflowError), as a *pyjson.Error.
func PackageManifest(tarball []byte) (*pyjson.Object, error) {
	manifest, err := packageManifest(tarball)
	switch classOf(err) {
	case "":
		if err != nil {
			return nil, err
		}
		return manifest, nil
	case "IndexError", "OverflowError":
		e := err.(*pyError)
		return nil, &pyjson.Error{Class: e.class, Message: e.msg}
	}
	// KeyError, OSError (BadGzipFile), EOFError, ValueError, RecursionError,
	// tarfile.TarError and zlib errors: no manifest.
	return nil, nil
}

func packageManifest(tarball []byte) (*pyjson.Object, error) {
	w := &walker{s: newStream(tarball), offset: new(big.Int), global: &paxMap{}}
	var found *member
	for {
		m, err := w.next()
		if err != nil {
			return nil, err
		}
		if m == nil {
			break
		}
		if m.name == PackageJSON {
			found = m
		}
	}
	if found == nil {
		return nil, newError("KeyError", "filename 'package/package.json' not found")
	}
	if !found.isreg() || found.size.Cmp(big.NewInt(MaxPackageJSONBytes)) > 0 {
		return nil, nil
	}
	data, err := w.extract(found)
	if err != nil {
		return nil, err
	}
	value, err := pyjson.Decode(data)
	if err != nil {
		return nil, nil // ValueError and RecursionError of json.loads
	}
	manifest, _ := value.(*pyjson.Object)
	return manifest, nil
}

// paxMap is a dict of pax keywords, in insertion order. Keywords and values
// keep their bytes: tarfile decodes them as UTF-8 with surrogateescape, which
// keeps them apart in the same way.
type paxMap struct {
	keys   []string
	values map[string]string
}

func (p *paxMap) set(key, value string) {
	if p.values == nil {
		p.values = map[string]string{}
	}
	if _, ok := p.values[key]; !ok {
		p.keys = append(p.keys, key)
	}
	p.values[key] = value
}

func (p *paxMap) get(key string) (string, bool) {
	v, ok := p.values[key]
	return v, ok
}

func (p *paxMap) copy() *paxMap {
	out := &paxMap{keys: append([]string(nil), p.keys...), values: make(map[string]string, len(p.values))}
	for k, v := range p.values {
		out.values[k] = v
	}
	return out
}

// member is a TarInfo with the fields package_manifest depends on.
type member struct {
	name       string
	typ        byte
	size       *big.Int
	offsetData int64
	sparse     [][2]*big.Int
	hasSparse  bool
	// the old GNU sparse structures of the header, for procSparse
	structs  [][2]*big.Int
	extended bool
	origsize *big.Int
}

func (m *member) isreg() bool {
	switch m.typ {
	case '0', 0, '7', 'S':
		return true
	}
	return false
}

func supported(typ byte) bool {
	return strings.IndexByte("0\x001234567LKS", typ) >= 0
}

func gnuType(typ byte) bool {
	return typ == 'L' || typ == 'K' || typ == 'S'
}

// walker is a TarFile opened for reading.
type walker struct {
	s      *stream
	offset *big.Int // TarFile.offset: where the next header starts
	global *paxMap  // TarFile.pax_headers
}

func isHeaderError(err error) bool {
	switch classOf(err) {
	case "EmptyHeaderError", "TruncatedHeaderError", "EOFHeaderError", "InvalidHeaderError", "SubsequentHeaderError":
		return true
	}
	return false
}

// next is TarFile.next: the next member, nil at the end of the archive.
func (w *walker) next() (*member, error) {
	if w.offset.Cmp(big.NewInt(w.s.pos)) != 0 {
		if w.offset.Sign() == 0 {
			return nil, nil
		}
		if err := w.s.seek(new(big.Int).Sub(w.offset, big.NewInt(1))); err != nil {
			return nil, err
		}
		b, err := w.s.read(1)
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			return nil, newError("ReadError", "unexpected end of data")
		}
	}
	m, err := w.fromtarfile(1)
	if err == nil {
		return m, nil
	}
	first := w.offset.Sign() == 0 // TarFile.offset as it stands after the failed header
	e, _ := err.(*pyError)
	switch classOf(err) {
	case "EOFHeaderError":
		return nil, nil
	case "InvalidHeaderError", "TruncatedHeaderError":
		if first {
			return nil, newError("ReadError", e.msg)
		}
		return nil, nil
	case "EmptyHeaderError":
		if first {
			return nil, newError("ReadError", "empty file")
		}
		return nil, nil
	case "SubsequentHeaderError":
		return nil, newError("ReadError", e.msg)
	}
	return nil, err
}

// fromtarfile is TarInfo.fromtarfile; depth counts the nested calls.
func (w *walker) fromtarfile(depth int) (*member, error) {
	if depth > maxHeaderChain+1 {
		return nil, newError("RecursionError", "maximum recursion depth exceeded")
	}
	buf, err := w.s.read(blockSize)
	if err != nil {
		return nil, err
	}
	m, err := frombuf(buf)
	if err != nil {
		return nil, err
	}
	switch m.typ {
	case 'L', 'K':
		return w.procGnulong(m, depth)
	case 'S':
		return w.procSparse(m)
	case 'x', 'g', 'X':
		return w.procPax(m, depth)
	}
	return w.procBuiltin(m)
}

func nts(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func invalid() error { return newError("InvalidHeaderError", "invalid header") }

// nti reads a number field: base 256 after 0x80 or 0xFF, else int(text, 8)
// of the text up to the first NUL. An empty field is the IndexError of s[0].
func nti(s []byte) (*big.Int, error) {
	if len(s) == 0 {
		return nil, newError("IndexError", "index out of range")
	}
	if s[0] == 0x80 || s[0] == 0xFF {
		n := new(big.Int).SetBytes(s[1:])
		if s[0] == 0xFF {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(8*(len(s)-1))))
		}
		return n, nil
	}
	text := nts(s)
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return nil, invalid() // UnicodeDecodeError of the ASCII decode
		}
	}
	text = pystr.Strip(text)
	if text == "" {
		text = "0"
	}
	n, ok := parseOctal(text)
	if !ok {
		return nil, invalid()
	}
	return n, nil
}

func pySpace(c byte) bool {
	return c == ' ' || c >= '\t' && c <= '\r'
}

// parseOctal is int(text, 8) of an ASCII text: white space around, a sign,
// an optional 0o prefix with one underscore after it, single underscores
// between digits.
func parseOctal(t string) (*big.Int, bool) {
	p := 0
	for p < len(t) && pySpace(t[p]) {
		p++
	}
	negative := false
	if p < len(t) && (t[p] == '+' || t[p] == '-') {
		negative = t[p] == '-'
		p++
	}
	if p+1 < len(t) && t[p] == '0' && (t[p+1] == 'o' || t[p+1] == 'O') {
		p += 2
		if p < len(t) && t[p] == '_' {
			p++
		}
	}
	if p < len(t) && t[p] == '_' {
		return nil, false
	}
	start := p
	var digits []byte
	prev := byte(0)
	for p < len(t) && (t[p] >= '0' && t[p] <= '7' || t[p] == '_') {
		if t[p] == '_' {
			if prev == '_' {
				return nil, false
			}
		} else {
			digits = append(digits, t[p])
		}
		prev = t[p]
		p++
	}
	if prev == '_' || p == start {
		return nil, false
	}
	for p < len(t) && pySpace(t[p]) {
		p++
	}
	if p != len(t) {
		return nil, false
	}
	n, _ := new(big.Int).SetString(string(digits), 8)
	if negative {
		n.Neg(n)
	}
	return n, true
}

func checksums(buf []byte) (int64, int64) {
	unsigned, signed := int64(256), int64(256)
	for i, c := range buf {
		if i >= 148 && i < 156 {
			continue
		}
		unsigned += int64(c)
		signed += int64(int8(c)) // #nosec G115 -- the signed checksum of tarfile reads each byte as int8, the wrap is intended.
	}
	return unsigned, signed
}

// frombuf is TarInfo.frombuf.
func frombuf(buf []byte) (*member, error) {
	switch {
	case len(buf) == 0:
		return nil, newError("EmptyHeaderError", "empty header")
	case len(buf) != blockSize:
		return nil, newError("TruncatedHeaderError", "truncated header")
	case bytes.Count(buf, []byte{0}) == blockSize:
		return nil, newError("EOFHeaderError", "end of file header")
	}
	chksum, err := nti(buf[148:156])
	if err != nil {
		return nil, err
	}
	unsigned, signed := checksums(buf)
	if !chksum.IsInt64() || chksum.Int64() != unsigned && chksum.Int64() != signed {
		return nil, newError("InvalidHeaderError", "bad checksum")
	}
	m := &member{name: nts(buf[0:100]), typ: buf[156]}
	for _, field := range [][2]int{{100, 108}, {108, 116}, {116, 124}, {124, 136}, {136, 148}, {329, 337}, {337, 345}} {
		n, err := nti(buf[field[0]:field[1]])
		if err != nil {
			return nil, err
		}
		if field[0] == 124 {
			m.size = n
		}
	}
	prefix := nts(buf[345:500])
	if m.typ == 0 && strings.HasSuffix(m.name, "/") {
		m.typ = '5'
	}
	if m.typ == 'S' {
		for pos := 386; pos < 386+4*24; pos += 24 {
			offset, err := nti(buf[pos : pos+12])
			if err != nil {
				return nil, err
			}
			numbytes, err := nti(buf[pos+12 : pos+24])
			if err != nil {
				return nil, err
			}
			m.structs = append(m.structs, [2]*big.Int{offset, numbytes})
		}
		m.extended = buf[482] != 0
		if m.origsize, err = nti(buf[483:495]); err != nil {
			return nil, err
		}
	}
	if m.typ == '5' {
		m.name = strings.TrimRight(m.name, "/")
	}
	if prefix != "" && !gnuType(m.typ) {
		m.name = prefix + "/" + m.name
	}
	return m, nil
}

// block is TarInfo._block: count rounded up to whole blocks.
func block(count *big.Int) (*big.Int, error) {
	if count.Sign() < 0 {
		return nil, newError("InvalidHeaderError", "invalid offset")
	}
	n := new(big.Int).Add(count, big.NewInt(blockSize-1))
	n.Quo(n, bigBlock)
	return n.Mul(n, bigBlock), nil
}

// readBlocks is fileobj.read(n) for the data of an extended header: n above
// the 64 bit range is the OverflowError of the buffered reader.
func (w *walker) readBlocks(n *big.Int) ([]byte, error) {
	if n.Cmp(maxInt64) > 0 {
		return nil, newError("OverflowError", "cannot fit 'int' into an index-sized integer")
	}
	return w.s.read(n.Int64())
}

func (w *walker) procBuiltin(m *member) (*member, error) {
	m.offsetData = w.s.pos
	offset := big.NewInt(m.offsetData)
	if m.isreg() || !supported(m.typ) {
		b, err := block(m.size)
		if err != nil {
			return nil, err
		}
		offset.Add(offset, b)
	}
	w.offset = offset
	if err := m.applyPax(w.global); err != nil {
		return nil, err
	}
	if m.typ == '5' {
		m.name = strings.TrimRight(m.name, "/")
	}
	return m, nil
}

func (w *walker) procGnulong(m *member, depth int) (*member, error) {
	n, err := block(m.size)
	if err != nil {
		return nil, err
	}
	buf, err := w.readBlocks(n)
	if err != nil {
		return nil, err
	}
	next, err := w.fromtarfile(depth + 1)
	if err != nil {
		if isHeaderError(err) {
			return nil, newError("SubsequentHeaderError", err.(*pyError).msg)
		}
		return nil, err
	}
	if m.typ == 'L' {
		next.name = nts(buf)
	}
	if next.typ == '5' {
		next.name = strings.TrimSuffix(next.name, "/")
	}
	return next, nil
}

// slice is buf[a:b] with Python's clamping.
func slice(buf []byte, a, b int) []byte {
	return buf[min(a, len(buf)):min(b, len(buf))]
}

func (w *walker) procSparse(m *member) (*member, error) {
	structs := m.structs
	for extended := m.extended; extended; {
		buf, err := w.s.read(blockSize)
		if err != nil {
			return nil, err
		}
		for pos := 0; pos < 21*24; pos += 24 {
			offset, err := nti(slice(buf, pos, pos+12))
			if err != nil {
				return nil, err
			}
			numbytes, err := nti(slice(buf, pos+12, pos+24))
			if err != nil {
				return nil, err
			}
			if offset.Sign() != 0 && numbytes.Sign() != 0 {
				structs = append(structs, [2]*big.Int{offset, numbytes})
			}
		}
		if len(buf) <= 504 {
			return nil, newError("IndexError", "index out of range")
		}
		extended = buf[504] != 0
	}
	m.sparse, m.hasSparse = structs, true
	m.offsetData = w.s.pos
	b, err := block(m.size)
	if err != nil {
		return nil, err
	}
	w.offset = b.Add(b, big.NewInt(m.offsetData))
	m.size = m.origsize
	return m, nil
}

type paxRecord struct {
	keyword, value []byte
}

func (w *walker) procPax(m *member, depth int) (*member, error) {
	n, err := block(m.size)
	if err != nil {
		return nil, err
	}
	buf, err := w.readBlocks(n)
	if err != nil {
		return nil, err
	}
	pax := w.global
	if m.typ != 'g' {
		pax = w.global.copy()
	}
	var records []paxRecord
	for pos := 0; len(buf) > pos && buf[pos] != 0; {
		// "%d %s=%s\n" with a length of one to twenty digits
		d := 0
		for pos+d < len(buf) && buf[pos+d] >= '0' && buf[pos+d] <= '9' {
			d++
		}
		if d == 0 || d > 20 || pos+d >= len(buf) || buf[pos+d] != ' ' {
			return nil, invalid()
		}
		length, _ := new(big.Int).SetString(string(buf[pos:pos+d]), 10)
		if length.Cmp(big.NewInt(5)) < 0 || new(big.Int).Add(length, big.NewInt(int64(pos))).Cmp(big.NewInt(int64(len(buf)))) > 0 {
			return nil, invalid()
		}
		end := pos + int(length.Int64()) - 1
		var keyword, value []byte
		equals := false
		if start := pos + d + 1; start < end {
			keyword, value, equals = bytes.Cut(buf[start:end], []byte("="))
		}
		if len(keyword) == 0 || !equals || buf[end] != '\n' {
			return nil, invalid()
		}
		records = append(records, paxRecord{keyword, value})
		pos += int(length.Int64())
	}
	for _, r := range records {
		pax.set(string(r.keyword), string(r.value))
	}
	next, err := w.fromtarfile(depth + 1)
	if err != nil {
		if isHeaderError(err) {
			return nil, newError("SubsequentHeaderError", err.(*pyError).msg)
		}
		return nil, err
	}
	major, _ := pax.get("GNU.sparse.major")
	minor, _ := pax.get("GNU.sparse.minor")
	if sparseMap, ok := pax.get("GNU.sparse.map"); ok {
		var numbers []*big.Int
		for _, part := range strings.Split(sparseMap, ",") {
			n, err := pystr.ParseInt(part)
			if err != nil {
				return nil, newError("ValueError", err.Error())
			}
			numbers = append(numbers, n)
		}
		next.sparse, next.hasSparse = pairs(numbers), true
	} else if _, ok := pax.get("GNU.sparse.size"); ok {
		var offsets, sizes []*big.Int
		for _, r := range records {
			k := string(r.keyword)
			if k != "GNU.sparse.offset" && k != "GNU.sparse.numbytes" {
				continue
			}
			if !utf8.Valid(r.value) {
				return nil, invalid()
			}
			n, err := pystr.ParseInt(string(r.value))
			if err != nil {
				return nil, invalid()
			}
			if k == "GNU.sparse.offset" {
				offsets = append(offsets, n)
			} else {
				sizes = append(sizes, n)
			}
		}
		next.hasSparse = true
		next.sparse = nil
		for i := 0; i < len(offsets) && i < len(sizes); i++ {
			next.sparse = append(next.sparse, [2]*big.Int{offsets[i], sizes[i]})
		}
	} else if major == "1" && minor == "0" {
		if err := w.sparse10(next); err != nil {
			return nil, err
		}
	}
	if m.typ == 'x' || m.typ == 'X' {
		if err := next.applyPax(pax); err != nil {
			return nil, err
		}
		if _, ok := pax.get("size"); ok {
			offset := big.NewInt(next.offsetData)
			if next.isreg() || !supported(next.typ) {
				b, err := block(next.size)
				if err != nil {
					return nil, err
				}
				offset.Add(offset, b)
			}
			w.offset = offset
		}
	}
	return next, nil
}

func pairs(numbers []*big.Int) [][2]*big.Int {
	var out [][2]*big.Int
	for i := 0; i+1 < len(numbers); i += 2 {
		out = append(out, [2]*big.Int{numbers[i], numbers[i+1]})
	}
	return out
}

// intBytes is int() of a bytes object: ASCII only.
func intBytes(b []byte) (*big.Int, error) {
	for _, c := range b {
		if c >= 0x80 {
			return nil, newError("ValueError", "invalid literal for int() with base 10")
		}
	}
	n, err := pystr.ParseInt(string(b))
	if err != nil {
		return nil, newError("ValueError", err.Error())
	}
	return n, nil
}

// sparse10 is _proc_gnusparse_10: the map of a GNU sparse file of format
// 1.0, in the blocks before its data.
func (w *walker) sparse10(next *member) error {
	buf, err := w.s.read(blockSize)
	if err != nil {
		return err
	}
	head, rest, ok := bytes.Cut(buf, []byte("\n"))
	if !ok {
		return newError("ValueError", "not enough values to unpack (expected 2, got 1)")
	}
	fields, err := intBytes(head)
	if err != nil {
		return err
	}
	want := new(big.Int).Mul(fields, big.NewInt(2))
	var numbers []*big.Int
	for big.NewInt(int64(len(numbers))).Cmp(want) < 0 {
		if !bytes.Contains(rest, []byte("\n")) {
			more, err := w.s.read(blockSize)
			if err != nil {
				return err
			}
			rest = append(rest, more...)
		}
		number, tail, ok := bytes.Cut(rest, []byte("\n"))
		if !ok {
			return newError("ValueError", "not enough values to unpack (expected 2, got 1)")
		}
		n, err := intBytes(number)
		if err != nil {
			return err
		}
		numbers = append(numbers, n)
		rest = tail
	}
	next.offsetData = w.s.pos
	next.sparse, next.hasSparse = pairs(numbers), true
	return nil
}

// applyPax is TarInfo._apply_pax_info for the fields that decide the name,
// the size and the type of a member.
func (m *member) applyPax(pax *paxMap) error {
	for _, key := range pax.keys {
		value := pax.values[key]
		switch key {
		case "GNU.sparse.name":
			m.name = value
		case "GNU.sparse.size", "GNU.sparse.realsize":
			n, err := pystr.ParseInt(value)
			if err != nil {
				return newError("ValueError", err.Error())
			}
			m.size = n
		case "path":
			m.name = strings.TrimRight(value, "/")
		case "size":
			n, err := pystr.ParseInt(value)
			if err != nil {
				n = new(big.Int)
			}
			m.size = n
		}
	}
	return nil
}

type extent struct {
	data              bool
	start, stop, real *big.Int
}

// extract reads a member as ExFileObject reads it: through the map of data
// and zero blocks of _FileInFile, every data block at its place in the
// decompressed stream.
func (w *walker) extract(m *member) ([]byte, error) {
	if m.size.Sign() <= 0 {
		return nil, nil
	}
	size := m.size.Int64() // at most MaxPackageJSONBytes here
	blocks := [][2]*big.Int{{new(big.Int), m.size}}
	if m.hasSparse {
		blocks = m.sparse
	}
	var extents []extent
	last := new(big.Int)
	real := big.NewInt(m.offsetData)
	for _, b := range blocks {
		offset, n := b[0], b[1]
		if offset.Cmp(last) > 0 {
			extents = append(extents, extent{start: last, stop: offset})
		}
		extents = append(extents, extent{data: true, start: offset, stop: new(big.Int).Add(offset, n), real: real})
		real = new(big.Int).Add(real, n)
		last = new(big.Int).Add(offset, n)
	}
	if last.Cmp(m.size) < 0 {
		extents = append(extents, extent{start: last, stop: m.size})
	}
	out := make([]byte, 0, size)
	index := 0
	for position := int64(0); position < size; {
		at := big.NewInt(position)
		for tries := 0; ; tries++ {
			e := extents[index]
			if e.start.Cmp(at) <= 0 && at.Cmp(e.stop) < 0 {
				break
			}
			if tries > len(extents) {
				return nil, newError("ReadError", "no block holds the position")
			}
			index = (index + 1) % len(extents)
		}
		e := extents[index]
		length := size - position
		if left := new(big.Int).Sub(e.stop, at); left.Cmp(big.NewInt(length)) < 0 {
			length = left.Int64()
		}
		if e.data {
			target := new(big.Int).Add(e.real, new(big.Int).Sub(at, e.start))
			if err := w.s.seek(target); err != nil {
				return nil, err
			}
			b, err := w.s.read(length)
			if err != nil {
				return nil, err
			}
			if int64(len(b)) != length {
				return nil, newError("ReadError", "unexpected end of data")
			}
			out = append(out, b...)
		} else {
			out = append(out, make([]byte, length)...)
		}
		position += length
	}
	return out, nil
}
