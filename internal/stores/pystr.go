package stores

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// A Python str is a Go string in the form of internal/pyjson: UTF-8, a lone
// surrogate as its three byte form, a byte that is not UTF-8 as the
// surrogate surrogateescape gives it. A file name is bytes, which Python
// reads with surrogateescape: every byte outside valid UTF-8 is U+DC00 plus
// the byte, also the bytes of an encoded surrogate.

func isSurrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

// pyRunes reads a Python str into code points.
func pyRunes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
				r, width = 0xD000|rune(s[i+1]&0x3F)<<6|rune(s[i+2]&0x3F), 3
			} else {
				r = 0xDC00 + rune(s[i])
			}
		}
		out = append(out, r)
		i += width
	}
	return out
}

// fsRunes reads a file name into the code points of os.fsdecode.
func fsRunes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			r = 0xDC00 + rune(s[i])
		}
		out = append(out, r)
		i += width
	}
	return out
}

// fsCompare orders file names as sorted() orders their decoded str: by code
// point, which is byte order for UTF-8.
func fsCompare(a, b string) int {
	if utf8.ValidString(a) && utf8.ValidString(b) {
		return strings.Compare(a, b)
	}
	return slices.Compare(fsRunes(a), fsRunes(b))
}

// repr is repr() of a Python str.
func repr(s string) string {
	runes := pyRunes(s)
	quote := '\''
	if slices.Contains(runes, '\'') && !slices.Contains(runes, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range runes {
		switch {
		case r == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7F:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7F:
			b.WriteRune(r)
		case isSurrogate(r) || !unicode.IsPrint(r):
			switch {
			case r <= 0xFF:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xFFFF:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// osErrorText is str() of the OSError Python raises for err:
// "[Errno 2] No such file or directory: '/x'".
func osErrorText(err error) string {
	var pathErr *fs.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) && errors.As(pathErr.Err, &errno) {
		text := errno.Error()
		if text != "" {
			text = strings.ToUpper(text[:1]) + text[1:]
		}
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), text, repr(pathErr.Path))
	}
	return err.Error()
}

// pyOSError is an OSError of Python: its text is that of Python, errors.Is
// and errors.As see the Go error below it.
type pyOSError struct {
	err error
}

func (e *pyOSError) Error() string { return osErrorText(e.err) }

func (e *pyOSError) Unwrap() error { return e.err }

func osError(err error) error {
	return &pyOSError{err: err}
}

// encodeError is the UnicodeEncodeError of str.encode("utf-8") for a file
// name that holds surrogates.
func encodeError(name string) *Error {
	runes := fsRunes(name)
	start := slices.IndexFunc(runes, isSurrogate)
	end := start
	for end < len(runes) && isSurrogate(runes[end]) {
		end++
	}
	message := fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed",
		start, end-1)
	if end-start == 1 {
		message = fmt.Sprintf(`'utf-8' codec can't encode character '\u%04x' in position %d: surrogates not allowed`,
			runes[start], start)
	}
	return &Error{Class: "UnicodeEncodeError", Message: message}
}
