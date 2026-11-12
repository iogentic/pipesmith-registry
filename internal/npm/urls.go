package npm

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// unquote is urllib.parse.unquote of a str: each run of ASCII characters
// has its %XX escapes decoded and is read as UTF-8 with errors="replace";
// every other character stays as it is.
func unquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		if s[i] >= 0x80 {
			for j < len(s) && s[j] >= 0x80 {
				j++
			}
			b.WriteString(s[i:j])
		} else {
			for j < len(s) && s[j] < 0x80 {
				j++
			}
			b.WriteString(decodeReplace(unquoteToBytes(s[i:j])))
		}
		i = j
	}
	return b.String()
}

func unquoteToBytes(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

// decodeReplace is bytes.decode("utf-8", "replace"): every maximal invalid
// subpart becomes one U+FFFD, as Python counts them.
func decodeReplace(b []byte) string {
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += maximalSubpart(b[i:])
	}
	return sb.String()
}

func maximalSubpart(b []byte) int {
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch lead := b[0]; {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		c := b[n]
		if k == 0 && (c < lo || c > hi) || k > 0 && (c < 0x80 || c > 0xBF) {
			break
		}
		n++
	}
	return n
}

const alwaysSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"

// quote is urllib.parse.quote(s, safe="/@"). The text is encoded strictly
// as UTF-8 first, so a lone surrogate is the UnicodeEncodeError of Python.
func quote(s string) (string, error) {
	if err := encodable(s); err != nil {
		return "", err
	}
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 && (strings.IndexByte(alwaysSafe, c) >= 0 || c == '/' || c == '@') {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upper[c>>4])
		b.WriteByte(upper[c&15])
	}
	return b.String(), nil
}

// encodable fails as str.encode("utf-8") fails on the first run of
// surrogates: the three byte form of internal/pyjson, or a byte that is not
// UTF-8 (surrogateescape). Positions count code points.
func encodable(s string) error {
	chars := 0
	for i := 0; i < len(s); {
		r, width := char(s, i)
		if !surrogate(r) {
			i += width
			chars++
			continue
		}
		start, first := chars, r
		for i < len(s) {
			r, width = char(s, i)
			if !surrogate(r) {
				break
			}
			i += width
			chars++
		}
		msg := fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", start, chars-1)
		if chars-start == 1 {
			msg = fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", first, start)
		}
		return &pyjson.Error{Class: "UnicodeEncodeError", Message: msg}
	}
	return nil
}

func surrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

// char is the code point at s[i:] in the form of internal/pyjson.
func char(s string, i int) (rune, int) {
	r, width := utf8.DecodeRuneInString(s[i:])
	if r != utf8.RuneError || width > 1 {
		return r, width
	}
	if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
		return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F), 3
	}
	return 0xDC00 + rune(s[i]), 1
}
