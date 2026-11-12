package pyjson

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

func isSurrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

// nextChar returns the code point at s[i:] and its width in bytes. A lone
// surrogate in its three byte form is a code point; any other byte that is
// not UTF-8 is U+DC00 plus the byte, as Python's surrogateescape reads it.
func nextChar(s string, i int) (rune, int) {
	r, width := utf8.DecodeRuneInString(s[i:])
	if r != utf8.RuneError || width > 1 {
		return r, width
	}
	if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
		return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F), 3
	}
	return 0xDC00 + rune(s[i]), 1
}

// appendChar writes r as UTF-8, a surrogate in its three byte form.
func appendChar(b []byte, r rune) []byte {
	if isSurrogate(r) {
		return append(b, 0xED, 0x80|byte(r>>6&0x3F), 0x80|byte(r&0x3F))
	}
	return utf8.AppendRune(b, r)
}

// normalize turns a Go string into the form of a Python str: bytes that are
// not UTF-8 become their surrogateescape code points.
func normalize(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); {
		r, width := nextChar(s, i)
		if width == 1 && r >= 0x80 {
			out = appendChar(out, r)
		} else {
			out = append(out, s[i:i+width]...)
		}
		i += width
	}
	return string(out)
}

func decodeError(codec string, b []byte, start, end int, reason string) error {
	var msg string
	if start < len(b) && end == start+1 {
		msg = fmt.Sprintf("'%s' codec can't decode byte 0x%02x in position %d: %s", codec, b[start], start, reason)
	} else {
		msg = fmt.Sprintf("'%s' codec can't decode bytes in position %d-%d: %s", codec, start, end-1, reason)
	}
	return &Error{Class: "UnicodeDecodeError", Message: msg}
}

func continuation(c byte) bool {
	return c&0xC0 == 0x80
}

// utf8Error checks the sequence at b[i:] (b[i] >= 0x80) as the UTF-8 decoder
// of Python does. It returns the width of a valid sequence, or 0 and the end
// and reason of the error that starts at i.
func utf8Error(b []byte, i int) (int, int, string) {
	c := b[i]
	left := len(b) - i
	switch {
	case c < 0xC2:
		return 0, i + 1, "invalid start byte"
	case c < 0xE0:
		if left < 2 {
			return 0, len(b), "unexpected end of data"
		}
		if !continuation(b[i+1]) {
			return 0, i + 1, "invalid continuation byte"
		}
		return 2, 0, ""
	case c < 0xF0:
		if left >= 2 {
			c2 := b[i+1]
			if !continuation(c2) || (c == 0xE0 && c2 < 0xA0) || (c == 0xED && c2 >= 0xA0) {
				return 0, i + 1, "invalid continuation byte"
			}
		}
		if left < 3 {
			return 0, len(b), "unexpected end of data"
		}
		if !continuation(b[i+2]) {
			return 0, i + 2, "invalid continuation byte"
		}
		return 3, 0, ""
	case c < 0xF5:
		if left >= 2 {
			c2 := b[i+1]
			if !continuation(c2) || (c == 0xF0 && c2 < 0x90) || (c == 0xF4 && c2 >= 0x90) {
				return 0, i + 1, "invalid continuation byte"
			}
		}
		if left >= 3 && !continuation(b[i+2]) {
			return 0, i + 2, "invalid continuation byte"
		}
		if left < 4 {
			return 0, len(b), "unexpected end of data"
		}
		if !continuation(b[i+3]) {
			return 0, i + 3, "invalid continuation byte"
		}
		return 4, 0, ""
	}
	return 0, i + 1, "invalid start byte"
}

// decodeUTF8 is bytes.decode("utf-8", errors) with errors "strict" or, with
// pass, "surrogatepass". The result keeps the bytes: UTF-8, and surrogates
// in their three byte form.
func decodeUTF8(b []byte, pass bool) (string, error) {
	for i := 0; i < len(b); {
		if b[i] < 0x80 {
			i++
			continue
		}
		width, end, reason := utf8Error(b, i)
		if width > 0 {
			i += width
			continue
		}
		if pass && len(b)-i >= 3 && b[i]&0xF0 == 0xE0 && continuation(b[i+1]) && continuation(b[i+2]) {
			r := rune(b[i]&0x0F)<<12 | rune(b[i+1]&0x3F)<<6 | rune(b[i+2]&0x3F)
			if isSurrogate(r) {
				i += 3
				continue
			}
		}
		return "", decodeError("utf-8", b, i, end, reason)
	}
	return string(b), nil
}

// decodeUTF16 decodes b[start:] with surrogatepass; positions in errors
// count from the start of b, as Python counts them when a BOM was read.
func decodeUTF16(b []byte, start int, bigEndian bool) (string, error) {
	codec := "utf-16-le"
	if bigEndian {
		codec = "utf-16-be"
	}
	unit := func(q int) rune {
		if bigEndian {
			return rune(b[q])<<8 | rune(b[q+1])
		}
		return rune(b[q+1])<<8 | rune(b[q])
	}
	out := make([]byte, 0, len(b))
	for q := start; q < len(b); {
		if len(b)-q < 2 {
			return "", decodeError(codec, b, q, len(b), "truncated data")
		}
		u := unit(q)
		q += 2
		if u >= 0xD800 && u < 0xDC00 && len(b)-q >= 2 {
			if low := unit(q); low >= 0xDC00 && low <= 0xDFFF {
				u = 0x10000 + (u-0xD800)<<10 + (low - 0xDC00)
				q += 2
			}
		}
		// A surrogate left alone passes as itself.
		out = appendChar(out, u)
	}
	return string(out), nil
}

// decodeUTF32 decodes b[start:] with surrogatepass.
func decodeUTF32(b []byte, start int, bigEndian bool) (string, error) {
	codec := "utf-32-le"
	if bigEndian {
		codec = "utf-32-be"
	}
	out := make([]byte, 0, len(b))
	for q := start; q < len(b); q += 4 {
		if len(b)-q < 4 {
			return "", decodeError(codec, b, q, len(b), "truncated data")
		}
		var u uint32
		if bigEndian {
			u = uint32(b[q])<<24 | uint32(b[q+1])<<16 | uint32(b[q+2])<<8 | uint32(b[q+3])
		} else {
			u = uint32(b[q+3])<<24 | uint32(b[q+2])<<16 | uint32(b[q+1])<<8 | uint32(b[q])
		}
		if u > 0x10FFFF {
			return "", decodeError(codec, b, q, q+4, "code point not in range(0x110000)")
		}
		out = appendChar(out, rune(u))
	}
	return string(out), nil
}

// decodeBytes is what json.loads does with bytes before it parses them:
// s.decode(json.detect_encoding(s), "surrogatepass").
func decodeBytes(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE, 0, 0}):
		return decodeUTF32(b, 4, false)
	case bytes.HasPrefix(b, []byte{0, 0, 0xFE, 0xFF}):
		return decodeUTF32(b, 4, true)
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b, 2, false)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b, 2, true)
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		// utf-8-sig drops the BOM before decoding, so positions start after it.
		return decodeUTF8(b[3:], true)
	}
	if len(b) >= 4 {
		if b[0] == 0 {
			if b[1] != 0 {
				return decodeUTF16(b, 0, true)
			}
			return decodeUTF32(b, 0, true)
		}
		if b[1] == 0 {
			if b[2] != 0 || b[3] != 0 {
				return decodeUTF16(b, 0, false)
			}
			return decodeUTF32(b, 0, false)
		}
	} else if len(b) == 2 {
		if b[0] == 0 {
			return decodeUTF16(b, 0, true)
		}
		if b[1] == 0 {
			return decodeUTF16(b, 0, false)
		}
	}
	return decodeUTF8(b, true)
}

// readText is Path.read_text(encoding="utf-8") of a file holding b: strict
// UTF-8 and universal newlines.
func readText(b []byte) (string, error) {
	text, err := decodeUTF8(b, false)
	if err != nil {
		return "", err
	}
	if strings.IndexByte(text, '\r') < 0 {
		return text, nil
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}
