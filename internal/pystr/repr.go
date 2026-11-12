package pystr

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

// Repr is repr() of a value as internal/pyjson decodes it: nil, bool,
// int, int64, *big.Int, float64, string, []any, []string and *pyjson.Object.
// Other values give their Go form with %v.
func Repr(v any) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

func writeRepr(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case *big.Int:
		b.WriteString(x.String())
	case float64:
		b.WriteString(pyjson.FloatRepr(x))
	case string:
		b.WriteString(reprString(x))
	case []string:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(reprString(item))
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, item)
		}
		b.WriteByte(']')
	case *pyjson.Object:
		b.WriteByte('{')
		first := true
		for key, value := range x.All() {
			if !first {
				b.WriteString(", ")
			}
			first = false
			b.WriteString(reprString(key))
			b.WriteString(": ")
			writeRepr(b, value)
		}
		b.WriteByte('}')
	default:
		fmt.Fprintf(b, "%v", x)
	}
}

const hexDigits = "0123456789abcdef"

// reprString is unicode_repr of CPython.
func reprString(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	b := make([]byte, 0, len(s)+2)
	b = append(b, quote)
	for i := 0; i < len(s); {
		r, width := next(s, i)
		i += width
		switch {
		case r == rune(quote) || r == '\\':
			b = append(b, '\\', byte(r)) // #nosec G115 -- r is the quote byte or a backslash on this branch.
		case r == '\t':
			b = append(b, '\\', 't')
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r < ' ' || r == 0x7F:
			b = append(b, '\\', 'x', hexDigits[r>>4], hexDigits[r&0xF])
		case r < 0x7F:
			b = append(b, byte(r))
		case IsPrintable(r):
			b = appendRune(b, r)
		case r <= 0xFF:
			b = append(b, '\\', 'x', hexDigits[r>>4], hexDigits[r&0xF])
		case r <= 0xFFFF:
			b = append(b, '\\', 'u')
			b = appendHex(b, r, 4)
		default:
			b = append(b, '\\', 'U')
			b = appendHex(b, r, 8)
		}
	}
	return string(append(b, quote))
}

func appendHex(b []byte, r rune, digits int) []byte {
	for shift := (digits - 1) * 4; shift >= 0; shift -= 4 {
		b = append(b, hexDigits[r>>shift&0xF])
	}
	return b
}
