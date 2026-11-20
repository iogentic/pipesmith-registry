package logs

import (
	"fmt"
	"log/slog"
	"unicode/utf8"
)

// lineControl reports a character that LineText escapes from U+0080 on.
func lineControl(r rune) bool { return (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 }

// LineText is text written for one line of text (decision P7-24): a byte
// below 0x20, 0x7f and a byte that is not valid UTF-8 as \xHH, the
// characters U+0080 to U+009F, U+2028 and U+2029 as \uHHHH, hex digits in
// lower case. The backslash and every other character stay, so a text
// without such a character comes back as it is.
func LineText(text string) string {
	var b []byte
	for i := 0; i < len(text); {
		c := text[i]
		r, size := rune(c), 1
		if c >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(text[i:])
		}
		bad := r < 0x20 || r == 0x7f || (r == utf8.RuneError && size == 1)
		if !bad && !lineControl(r) {
			if b != nil {
				b = append(b, text[i:i+size]...)
			}
			i += size
			continue
		}
		if b == nil {
			b = append(make([]byte, 0, len(text)+8), text[:i]...)
		}
		if bad {
			b = append(b, '\\', 'x', hexDigits[c>>4], hexDigits[c&0xf])
		} else {
			b = append(b, '\\', 'u', hexDigits[r>>12&0xf], hexDigits[r>>8&0xf], hexDigits[r>>4&0xf], hexDigits[r&0xf])
		}
		i += size
	}
	if b == nil {
		return text
	}
	return string(b)
}

// jsonControls writes what encoding/json leaves raw in a line, 0x7f and
// U+0080 to U+009F, as \u00HH. Both occur only inside a JSON string.
func jsonControls(line []byte) []byte {
	var out []byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		n := 1
		switch {
		case c == 0x7f:
		case c == 0xc2 && i+1 < len(line) && line[i+1] >= 0x80 && line[i+1] <= 0x9f:
			c, n = line[i+1], 2
		default:
			if out != nil {
				out = append(out, c)
			}
			continue
		}
		if out == nil {
			out = append(make([]byte, 0, len(line)+16), line[:i]...)
		}
		out = append(out, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		i += n - 1
	}
	if out == nil {
		return line
	}
	return out
}

// linesKey is the key of the attribute of Lines.
const linesKey = "pipesmith_registry.lines"

// lineBreaks is the value of the attribute of Lines; only Lines makes one.
type lineBreaks struct{ text string }

// Lines is the attribute of a record whose message holds line breaks of its
// own format, as a traceback head: the text for standard error is the format
// with every string, byte slice, error and Stringer argument passed through
// LineText, so the format alone breaks the line. The handler of standard
// error writes it in place of the message; server.log leaves it out.
func Lines(format string, args ...any) slog.Attr {
	escaped := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case string:
			escaped[i] = LineText(v)
		case []byte:
			escaped[i] = LineText(string(v))
		case error:
			escaped[i] = LineText(v.Error())
		case fmt.Stringer:
			escaped[i] = LineText(v.String())
		default:
			escaped[i] = a
		}
	}
	return slog.Any(linesKey, lineBreaks{fmt.Sprintf(format, escaped...)})
}

// LinesText is the text of an attribute of Lines; ok is false for any other
// attribute.
func LinesText(a slog.Attr) (text string, ok bool) {
	if a.Key != linesKey {
		return "", false
	}
	l, ok := a.Value.Any().(lineBreaks)
	return l.text, ok
}
