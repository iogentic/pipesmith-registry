package maven

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// How expat inside Python 3.12 reads the bytes of a document: the encoding
// from the first bytes and the XML declaration, the six encodings expat
// knows, and for any other name the byte map a Python codec gives.

type codecKind int8

const (
	codecUnknown codecKind = iota // LookupError "unknown encoding"
	codecNotText                  // LookupError "is not a text encoding"
	codecCaught                   // decoding raised a ValueError or TypeError: _parse gives None
	codecMap                      // a map of the 256 byte values
)

type codecModule struct {
	name  string
	kind  codecKind
	index int
}

var codecModules = sync.OnceValue(func() map[string]codecModule {
	out := make(map[string]codecModule, len(codecModuleList))
	for _, m := range codecModuleList {
		out[m.name] = m
	}
	return out
})

var codecAliases = sync.OnceValue(func() map[string]string {
	out := make(map[string]string, len(codecAliasList))
	for _, pair := range codecAliasList {
		out[pair[0]] = pair[1]
	}
	return out
})

type codecResult struct {
	kind  codecKind
	table *[256]rune
}

var codecTables = sync.OnceValue(func() [][256]rune {
	out := make([][256]rune, len(codecMaps))
	for i, text := range codecMaps {
		j := 0
		for _, r := range text {
			if r == 0xFFFD {
				r = -1
			}
			out[i][j] = r
			j++
		}
	}
	return out
})

// normalizeEncoding is _Py_normalize_encoding followed by
// encodings.normalize_encoding, for the ASCII names a declaration allows:
// lower case, every run of other characters than letters, digits and "."
// one "_", none at the ends.
func normalizeEncoding(name string) string {
	var b strings.Builder
	punct := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' {
			if punct && b.Len() > 0 {
				b.WriteByte('_')
			}
			punct = false
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b.WriteByte(c)
		} else {
			punct = true
		}
	}
	return b.String()
}

// pythonCodec is what the unknown encoding handler of ElementTree gets for
// a name: the search function of the encodings package tries the aliased
// module, then the module of the name itself.
func pythonCodec(name string) codecResult {
	norm := normalizeEncoding(name)
	aliases := codecAliases()
	alias := aliases[norm]
	if alias == "" {
		alias = aliases[strings.ReplaceAll(norm, ".", "_")]
	}
	candidates := []string{norm}
	if alias != "" {
		candidates = []string{alias, norm}
	}
	for _, candidate := range candidates {
		if candidate == "" || strings.Contains(candidate, ".") {
			continue
		}
		module, ok := codecModules()[candidate]
		if !ok {
			continue
		}
		if module.kind == codecMap {
			return codecResult{kind: codecMap, table: &codecTables()[module.index]}
		}
		return codecResult{kind: module.kind}
	}
	return codecResult{kind: codecUnknown}
}

func significantASCII(c rune) bool {
	t := asciiTypes[c]
	return t != btOther && t != btNonXML
}

// newUnknownEncoding is the check of XmlInitUnknownEncoding: the ASCII
// characters that carry syntax must map to themselves, and no byte may map
// to such a character or above U+FFFF.
func newUnknownEncoding(table *[256]rune) (*[256]rune, bool) {
	for i := rune(0); i < 128; i++ {
		if significantASCII(i) && table[i] != i {
			return nil, false
		}
	}
	for i, c := range table {
		switch {
		case c == -1:
		case c < 0:
			return nil, false
		case c < 0x80:
			if significantASCII(c) && c != rune(i) {
				return nil, false
			}
		case !validChar(c):
			// a surrogate, U+FFFE or U+FFFF: a byte expat refuses when it meets it
		case c > 0xFFFF:
			return nil, false
		}
	}
	return table, true
}

type encodingKind int8

const (
	encUTF8 encodingKind = iota
	encUTF16BE
	encUTF16LE
	encLatin1
	encASCII
	encTable
)

type encoding struct {
	kind  encodingKind
	table *[256]rune
}

func (e encoding) minBytes() int {
	if e.kind == encUTF16BE || e.kind == encUTF16LE {
		return 2
	}
	return 1
}

// latin1Names: expat classes U+0080 to U+00FF with its Latin-1 table in
// these encodings, and with its name pages in the others.
func (e encoding) latin1Names() bool {
	return e.kind == encUTF16BE || e.kind == encUTF16LE || e.kind == encLatin1
}

// validChar is a character expat accepts in a document.
func validChar(r rune) bool {
	switch {
	case r == 0x9, r == 0xA, r == 0xD:
		return true
	case r < 0x20, r >= 0xD800 && r <= 0xDFFF, r == 0xFFFE, r == 0xFFFF, r > 0x10FFFF:
		return false
	}
	return true
}

// next decodes one character at b[i:]; ok false for bytes the encoding
// refuses or that end too early.
func (e encoding) next(b []byte, i int) (r rune, n int, ok bool) {
	switch e.kind {
	case encUTF8:
		r, n = utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 {
			return 0, 0, false
		}
	case encUTF16BE, encUTF16LE:
		unit := func(j int) rune {
			if e.kind == encUTF16BE {
				return rune(b[j])<<8 | rune(b[j+1])
			}
			return rune(b[j+1])<<8 | rune(b[j])
		}
		if len(b)-i < 2 {
			return 0, 0, false
		}
		r, n = unit(i), 2
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return 0, 0, false
		case r >= 0xD800 && r <= 0xDBFF:
			// expat takes the next unit as the second half without
			// looking at it.
			if len(b)-i < 4 {
				return 0, 0, false
			}
			r, n = 0x10000+(r&0x3FF)<<10|(unit(i+2)&0x3FF), 4
		}
	case encLatin1:
		r, n = rune(b[i]), 1
	case encASCII:
		r, n = rune(b[i]), 1
		if r >= 0x80 {
			return 0, 0, false
		}
	case encTable:
		r, n = e.table[b[i]], 1
		if r < 0 {
			return 0, 0, false
		}
	}
	if !validChar(r) {
		return 0, 0, false
	}
	return r, n, true
}

// decoded is a document as UTF-8, from the first character after the BOM
// and the XML declaration on.
type decoded struct {
	text        []byte
	latin1Names bool
	declared    bool // an XML declaration came first
	standalone  bool
	// For the amplification limit: bytes of the original before text, and
	// original bytes per byte of text.
	before int
	ratio  float64
}

// declaration is the XML declaration that starts the document at b[i:]
// in enc: its pseudo attributes and the offset after "?>". isDecl false when
// the document starts otherwise; ok false when expat fails on it.
func declaration(enc encoding, b []byte, i int) (body []rune, end int, isDecl, ok bool) {
	var runes []rune
	j := i
	read := func() (rune, bool) {
		if j >= len(b) {
			return 0, false
		}
		r, n, good := enc.next(b, j)
		if !good {
			return 0, false
		}
		j += n
		return r, true
	}
	for _, want := range "<?xml" {
		r, good := read()
		if !good || r != want {
			return nil, 0, false, true
		}
	}
	r, good := read()
	if !good {
		return nil, 0, true, false
	}
	t := classify(r, enc.latin1Names(), false)
	switch {
	case isNameType(t):
		return nil, 0, false, true // a processing instruction with a longer target
	case r == '?':
		if r, good := read(); good && r == '>' {
			return nil, j, true, true
		}
		return nil, 0, true, false
	case t != btS && t != btCR && t != btLF:
		return nil, 0, true, false
	}
	runes = append(runes, r)
	for {
		r, good := read()
		if !good {
			return nil, 0, true, false
		}
		if r == '?' {
			if j < len(b) {
				if next, n, good := enc.next(b, j); good && next == '>' {
					return runes, j + n, true, true
				}
			}
		}
		runes = append(runes, r)
	}
}

func declSpace(c rune) bool {
	return c == ' ' || c == '\r' || c == '\n' || c == '\t'
}

// parseDeclaration is XmlParseXmlDecl of a document entity: version first,
// then encoding and standalone, each optional, in this order.
func parseDeclaration(body []rune) (encodingName string, standalone, ok bool) {
	p := 0
	// pseudo returns name and value of the next pseudo attribute; done
	// when only white space is left.
	pseudo := func() (name, value string, done, good bool) {
		if p == len(body) {
			return "", "", true, true
		}
		if !declSpace(body[p]) {
			return "", "", false, false
		}
		for p < len(body) && declSpace(body[p]) {
			p++
		}
		if p == len(body) {
			return "", "", true, true
		}
		start := p
		for {
			if p == len(body) || body[p] >= 0x80 {
				return "", "", false, false
			}
			if body[p] == '=' {
				name = string(body[start:p])
				break
			}
			if declSpace(body[p]) {
				name = string(body[start:p])
				for p < len(body) && declSpace(body[p]) {
					p++
				}
				if p == len(body) || body[p] != '=' {
					return "", "", false, false
				}
				break
			}
			p++
		}
		if p == start {
			return "", "", false, false
		}
		p++
		for p < len(body) && declSpace(body[p]) {
			p++
		}
		if p == len(body) || body[p] != '"' && body[p] != '\'' {
			return "", "", false, false
		}
		open := body[p]
		p++
		valueStart := p
		for {
			if p == len(body) {
				return "", "", false, false
			}
			c := body[p]
			if c == open {
				break
			}
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' ||
				c == '_') {
				return "", "", false, false
			}
			p++
		}
		value = string(body[valueStart:p])
		p++
		return name, value, false, true
	}
	name, _, done, good := pseudo()
	if !good || done || name != "version" {
		return "", false, false
	}
	name, value, done, good := pseudo()
	if !good {
		return "", false, false
	}
	if done {
		return "", false, true
	}
	if name == "encoding" {
		if value == "" || !(value[0] >= 'a' && value[0] <= 'z' || value[0] >= 'A' && value[0] <= 'Z') {
			return "", false, false
		}
		encodingName = value
		name, value, done, good = pseudo()
		if !good {
			return "", false, false
		}
		if done {
			return encodingName, false, true
		}
	}
	if name != "standalone" || value != "yes" && value != "no" {
		return "", false, false
	}
	for p < len(body) && declSpace(body[p]) {
		p++
	}
	if p != len(body) {
		return "", false, false
	}
	return encodingName, value == "yes", true
}

// knownEncodings are the names expat knows itself, matched without regard
// to ASCII case.
var knownEncodings = map[string]encodingKind{
	"ISO-8859-1": encLatin1, "US-ASCII": encASCII, "UTF-8": encUTF8,
	"UTF-16": encUTF16BE, "UTF-16BE": encUTF16BE, "UTF-16LE": encUTF16LE,
}

// decodeDocument reads data as expat does. ok false when expat fails; the
// error is the exception Python raises from the codec lookup.
func decodeDocument(data []byte, codecs func(string) codecResult) (*decoded, bool, error) {
	if len(data) < 2 {
		return nil, false, nil
	}
	enc, start := encoding{kind: encUTF8}, 0
	switch {
	case data[0] == 0xFE && data[1] == 0xFF:
		enc, start = encoding{kind: encUTF16BE}, 2
	case data[0] == 0xFF && data[1] == 0xFE:
		enc, start = encoding{kind: encUTF16LE}, 2
	case data[0] == 0x3C && data[1] == 0x00:
		enc = encoding{kind: encUTF16LE}
	case data[0] == 0xEF && data[1] == 0xBB:
		if len(data) == 2 {
			return nil, false, nil
		}
		if data[2] == 0xBF {
			start = 3
		}
	case data[0] == 0:
		enc = encoding{kind: encUTF16BE}
	case data[1] == 0:
		enc = encoding{kind: encUTF16LE}
	}
	out := &decoded{before: start}
	body, end, isDecl, ok := declaration(enc, data, start)
	if !ok {
		return nil, false, nil
	}
	if isDecl {
		name, standalone, ok := parseDeclaration(body)
		if !ok {
			return nil, false, nil
		}
		out.declared, out.standalone, out.before = true, standalone, end
		if name != "" {
			kind, known := encodingKind(0), false
			if len(name) <= 127 {
				for knownName, k := range knownEncodings {
					if strings.EqualFold(name, knownName) {
						kind, known = k, true
					}
				}
			}
			if known {
				next := encoding{kind: kind}
				if strings.EqualFold(name, "UTF-16") && enc.minBytes() == 2 {
					next = enc
				}
				if next.minBytes() != enc.minBytes() || next.minBytes() == 2 && next.kind != enc.kind {
					return nil, false, nil
				}
				enc = next
			} else {
				result := codecs(name)
				switch result.kind {
				case codecUnknown:
					return nil, false, &pyjson.Error{Class: "LookupError", Message: "unknown encoding: " + name}
				case codecNotText:
					return nil, false, &pyjson.Error{Class: "LookupError", Message: "'" + truncate(name, 400) +
						"' is not a text encoding; use codecs.decode() to handle arbitrary codecs"}
				case codecCaught:
					return nil, false, nil
				}
				table, good := newUnknownEncoding(result.table)
				if !good {
					return nil, false, nil
				}
				enc = encoding{kind: encTable, table: table}
			}
		}
	}
	rest := data[out.before:]
	out.latin1Names = enc.latin1Names()
	if enc.kind == encUTF8 {
		for i := 0; i < len(rest); {
			_, n, good := enc.next(rest, i)
			if !good {
				return nil, false, nil
			}
			i += n
		}
		out.text, out.ratio = rest, 1
		return out, true, nil
	}
	text := make([]byte, 0, len(rest)+len(rest)/2)
	for i := 0; i < len(rest); {
		r, n, good := enc.next(rest, i)
		if !good {
			return nil, false, nil
		}
		text = utf8.AppendRune(text, r)
		i += n
	}
	out.text, out.ratio = text, 1
	if len(text) > 0 {
		out.ratio = float64(len(rest)) / float64(len(text))
	}
	return out, true, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
