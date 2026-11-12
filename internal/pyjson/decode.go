package pyjson

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// maxDepth is the deepest nesting of arrays and objects that json.loads of
// the reference reads from a script; one more is a RecursionError. Python
// 3.12 counts it against its C recursion limit, so a caller deep in a stack
// may get a few levels less there.
const maxDepth = 9997

// Decode is json.loads(data) for bytes: the encoding comes from
// json.detect_encoding (UTF-8, UTF-8 with BOM, UTF-16, UTF-32), lone
// surrogates pass.
func Decode(data []byte) (any, error) {
	text, err := decodeBytes(data)
	if err != nil {
		return nil, err
	}
	return parse(text)
}

// DecodeString is json.loads(s) for a str: a leading BOM is an error.
func DecodeString(s string) (any, error) {
	text := normalize(s)
	if strings.HasPrefix(text, "\uFEFF") {
		return nil, (&parser{s: text}).syntax("Unexpected UTF-8 BOM (decode using utf-8-sig)", 0)
	}
	return parse(text)
}

// DecodeText is json.loads(path.read_text(encoding="utf-8")) for a file that
// holds data, as config.json and repositories.json are read: strict UTF-8, a
// BOM is an error, "\r\n" and "\r" become "\n" before parsing.
func DecodeText(data []byte) (any, error) {
	text, err := readText(data)
	if err != nil {
		return nil, err
	}
	return DecodeString(text)
}

// stop is the StopIteration of Python's scanner: no value starts at idx.
type stop struct{ idx int }

func (s *stop) Error() string { return "no value" }

// parser works on the text in UTF-8 with surrogates in their three byte
// form. Indices are bytes; errors report code points.
type parser struct {
	s     string
	depth int
}

func parse(text string) (any, error) {
	p := &parser{s: text}
	value, end, err := p.scan(p.skip(0))
	if err != nil {
		var none *stop
		if errors.As(err, &none) {
			return nil, p.syntax("Expecting value", none.idx)
		}
		return nil, err
	}
	if end = p.skip(end); end != len(text) {
		return nil, p.syntax("Extra data", end)
	}
	return value, nil
}

// chars counts the code points of s[:i].
func (p *parser) chars(i int) int {
	n := 0
	for j := 0; j < i; j++ {
		if !continuation(p.s[j]) {
			n++
		}
	}
	return n
}

func (p *parser) syntax(msg string, idx int) error {
	pos := p.chars(idx)
	colno := pos + 1
	if nl := strings.LastIndexByte(p.s[:idx], '\n'); nl >= 0 {
		colno = pos - p.chars(nl)
	}
	return &SyntaxError{Msg: msg, Pos: pos, Lineno: strings.Count(p.s[:idx], "\n") + 1, Colno: colno}
}

func (p *parser) skip(idx int) int {
	for idx < len(p.s) {
		switch p.s[idx] {
		case ' ', '\t', '\n', '\r':
			idx++
		default:
			return idx
		}
	}
	return idx
}

func (p *parser) has(idx int, word string) bool {
	return strings.HasPrefix(p.s[idx:], word)
}

// scan is scan_once of the C scanner.
func (p *parser) scan(idx int) (any, int, error) {
	if idx >= len(p.s) {
		return nil, 0, &stop{idx}
	}
	switch p.s[idx] {
	case '"':
		return p.str(idx + 1)
	case '{', '[':
		p.depth++
		defer func() { p.depth-- }()
		kind := "object"
		if p.s[idx] == '[' {
			kind = "array"
		}
		if p.depth > maxDepth {
			return nil, 0, &Error{Class: "RecursionError",
				Message: "maximum recursion depth exceeded while decoding a JSON " + kind + " from a unicode string"}
		}
		if kind == "array" {
			return p.array(idx + 1)
		}
		return p.object(idx + 1)
	case 'n':
		if p.has(idx, "null") {
			return nil, idx + 4, nil
		}
	case 't':
		if p.has(idx, "true") {
			return true, idx + 4, nil
		}
	case 'f':
		if p.has(idx, "false") {
			return false, idx + 5, nil
		}
	case 'N':
		if p.has(idx, "NaN") {
			return math.NaN(), idx + 3, nil
		}
	case 'I':
		if p.has(idx, "Infinity") {
			return math.Inf(1), idx + 8, nil
		}
	case '-':
		if p.has(idx, "-Infinity") {
			return math.Inf(-1), idx + 9, nil
		}
	}
	return p.number(idx)
}

func digit(c byte) bool {
	return c >= '0' && c <= '9'
}

// number is _match_number_unicode: an integer of any size, or a float when
// a fraction or an exponent follows.
func (p *parser) number(start int) (any, int, error) {
	s, last := p.s, len(p.s)-1
	idx := start
	if s[idx] == '-' {
		idx++
		if idx > last {
			return nil, 0, &stop{start}
		}
	}
	switch {
	case s[idx] >= '1' && s[idx] <= '9':
		idx++
		for idx <= last && digit(s[idx]) {
			idx++
		}
	case s[idx] == '0':
		idx++
	default:
		return nil, 0, &stop{start}
	}
	isFloat := false
	if idx < last && s[idx] == '.' && digit(s[idx+1]) {
		isFloat = true
		idx += 2
		for idx <= last && digit(s[idx]) {
			idx++
		}
	}
	if idx < last && (s[idx] == 'e' || s[idx] == 'E') {
		mark := idx
		idx++
		if idx < last && (s[idx] == '-' || s[idx] == '+') {
			idx++
		}
		for idx <= last && digit(s[idx]) {
			idx++
		}
		if digit(s[idx-1]) {
			isFloat = true
		} else {
			idx = mark
		}
	}
	text := s[start:idx]
	if isFloat {
		// Overflow gives an infinity and underflow zero, as float() does.
		f, _ := strconv.ParseFloat(text, 64)
		return f, idx, nil
	}
	if digits := len(strings.TrimPrefix(text, "-")); digits > maxIntDigits {
		return nil, 0, &Error{Class: "ValueError", Message: fmt.Sprintf(
			"Exceeds the limit (%d digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit",
			maxIntDigits, digits)}
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n, idx, nil
	}
	n, _ := new(big.Int).SetString(text, 10)
	return n, idx, nil
}

func hex4(s string) (rune, bool) {
	var r rune
	for i := 0; i < 4; i++ {
		c := s[i]
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}

// str is scanstring_unicode; end is the index after the opening quote.
func (p *parser) str(end int) (any, int, error) {
	s := p.s
	begin := end - 1
	var out []byte
	for {
		next := end
		var c byte
		for ; next < len(s); next++ {
			c = s[next]
			if c == '"' || c == '\\' {
				break
			}
			if c <= 0x1F {
				return nil, 0, p.syntax("Invalid control character at", next)
			}
		}
		if next >= len(s) {
			return nil, 0, p.syntax("Unterminated string starting at", begin)
		}
		out = append(out, s[end:next]...)
		next++
		if c == '"' {
			end = next
			break
		}
		if next == len(s) {
			return nil, 0, p.syntax("Unterminated string starting at", begin)
		}
		c = s[next]
		if c != 'u' {
			end = next + 1
			switch c {
			case '"', '\\', '/':
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			default:
				return nil, 0, p.syntax("Invalid \\escape", end-2)
			}
			out = append(out, c)
			continue
		}
		next++
		end = next + 4
		if end >= len(s) {
			return nil, 0, p.syntax("Invalid \\uXXXX escape", next-1)
		}
		r, ok := hex4(s[next:end])
		if !ok {
			return nil, 0, p.syntax("Invalid \\uXXXX escape", end-5)
		}
		if r >= 0xD800 && r <= 0xDBFF && end+6 < len(s) && s[end] == '\\' && s[end+1] == 'u' {
			end += 6
			low, ok := hex4(s[end-4 : end])
			if !ok {
				return nil, 0, p.syntax("Invalid \\uXXXX escape", end-5)
			}
			if low >= 0xDC00 && low <= 0xDFFF {
				r = 0x10000 + (r-0xD800)<<10 + (low - 0xDC00)
			} else {
				end -= 6
			}
		}
		out = appendChar(out, r)
	}
	return string(out), end, nil
}

func (p *parser) array(idx int) (any, int, error) {
	s, last := p.s, len(p.s)-1
	items := []any{}
	idx = p.skip(idx)
	if idx > last || s[idx] != ']' {
		for {
			value, next, err := p.scan(idx)
			if err != nil {
				return nil, 0, err
			}
			items = append(items, value)
			idx = p.skip(next)
			if idx <= last && s[idx] == ']' {
				break
			}
			if idx > last || s[idx] != ',' {
				return nil, 0, p.syntax("Expecting ',' delimiter", idx)
			}
			idx = p.skip(idx + 1)
		}
	}
	return items, idx + 1, nil
}

func (p *parser) object(idx int) (any, int, error) {
	s, last := p.s, len(p.s)-1
	obj := NewObject()
	idx = p.skip(idx)
	if idx > last || s[idx] != '}' {
		for {
			if idx > last || s[idx] != '"' {
				return nil, 0, p.syntax("Expecting property name enclosed in double quotes", idx)
			}
			key, next, err := p.str(idx + 1)
			if err != nil {
				return nil, 0, err
			}
			idx = p.skip(next)
			if idx > last || s[idx] != ':' {
				return nil, 0, p.syntax("Expecting ':' delimiter", idx)
			}
			value, next, err := p.scan(p.skip(idx + 1))
			if err != nil {
				return nil, 0, err
			}
			// A repeated key keeps its first position and takes the last value.
			obj.Set(key.(string), value)
			idx = p.skip(next)
			if idx <= last && s[idx] == '}' {
				break
			}
			if idx > last || s[idx] != ',' {
				return nil, 0, p.syntax("Expecting ',' delimiter", idx)
			}
			idx = p.skip(idx + 1)
		}
	}
	return obj, idx + 1, nil
}
