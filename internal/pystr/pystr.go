// Package pystr holds the parts of Python's str, int and pathlib that the
// ported modules depend on: repr() in messages, str.casefold and code point
// order in sorts, str.strip, int(str) and str() of a PurePosixPath.
//
// A Python str is a Go string in the form of internal/pyjson: UTF-8, a lone
// surrogate in its three byte form, and any other byte that is not UTF-8
// read as U+DC00 plus the byte (surrogateescape).
//
// The character tables follow Unicode 15.0.0, the database of Python 3.12 in
// the reference; the Go tables of package unicode are newer and are
// corrected where the two differ.
package pystr

import (
	"strings"
	"unicode/utf8"
)

// next returns the code point at s[i:] and its width in bytes.
func next(s string, i int) (rune, int) {
	r, width := utf8.DecodeRuneInString(s[i:])
	if r != utf8.RuneError || width > 1 {
		return r, width
	}
	if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
		return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F), 3
	}
	return 0xDC00 + rune(s[i]), 1
}

func isSurrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

func appendRune(b []byte, r rune) []byte {
	if isSurrogate(r) {
		return append(b, 0xED, 0x80|byte(r>>6&0x3F), 0x80|byte(r&0x3F))
	}
	return utf8.AppendRune(b, r)
}

// Runes returns the code points of s as Python sees them.
func Runes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, width := next(s, i)
		out = append(out, r)
		i += width
	}
	return out
}

// FromRunes is the inverse of Runes: surrogates in their three byte form.
func FromRunes(rs []rune) string {
	b := make([]byte, 0, len(rs))
	for _, r := range rs {
		b = appendRune(b, r)
	}
	return string(b)
}

// Compare orders a and b by code point, as Python compares two str.
func Compare(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ra, wa := next(a, i)
		rb, wb := next(b, j)
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		i += wa
		j += wb
	}
	switch {
	case i < len(a):
		return 1
	case j < len(b):
		return -1
	}
	return 0
}

// IsSpace is str.isspace of one character: bidirectional class WS, B or S,
// or category Zs.
func IsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D, r >= 0x1C && r <= 0x20, r == 0x85, r == 0xA0, r == 0x1680,
		r >= 0x2000 && r <= 0x200A, r == 0x2028, r == 0x2029, r == 0x202F, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

// Strip is str.strip(): leading and trailing IsSpace characters go, the
// bytes between them stay as they are.
func Strip(s string) string {
	start, end := -1, 0
	for i := 0; i < len(s); {
		r, width := next(s, i)
		if !IsSpace(r) {
			if start < 0 {
				start = i
			}
			end = i + width
		}
		i += width
	}
	if start < 0 {
		return ""
	}
	return s[start:end]
}

// Casefold is str.casefold().
func Casefold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	buf := make([]byte, 0, 8)
	for i := 0; i < len(s); {
		r, width := next(s, i)
		buf = buf[:0]
		for _, f := range foldRune(r) {
			buf = appendRune(buf, f)
		}
		b.Write(buf)
		i += width
	}
	return b.String()
}
