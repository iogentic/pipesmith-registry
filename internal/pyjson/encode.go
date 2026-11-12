package pyjson

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// maxIntDigits is sys.get_int_max_str_digits() of Python: str() of a longer
// integer is a ValueError, in both directions.
const maxIntDigits = 4300

// Options are the arguments of json.dumps.
type Options struct {
	// Indent is indent=. nil writes one line; n writes n spaces per level and
	// a newline before every item, 0 and below only the newlines.
	Indent *int
	// EnsureASCII is ensure_ascii=. json.dumps defaults to true; Starlette's
	// JSONResponse passes false.
	EnsureASCII bool
	// ItemSep and KeySep are separators=. Empty means the default of
	// json.dumps: ItemSep "," with an Indent, else ", "; KeySep ": ".
	ItemSep, KeySep string
	// RejectNaN is allow_nan=False: NaN and the infinities are a ValueError.
	RejectNaN bool
}

// Dumps is json.dumps(v).
func Dumps(v any) ([]byte, error) {
	return Encode(v, Options{EnsureASCII: true})
}

// DumpsIndent is json.dumps(v, indent=indent), the form of config.json,
// repositories.json and the --json output of the commands.
func DumpsIndent(v any, indent int) ([]byte, error) {
	return Encode(v, Options{Indent: &indent, EnsureASCII: true})
}

// ResponseBody is the body of Starlette's JSONResponse(v): json.dumps with
// ensure_ascii=False, allow_nan=False and separators (",", ":"), in UTF-8.
func ResponseBody(v any) ([]byte, error) {
	return Encode(v, Options{ItemSep: ",", KeySep: ":", RejectNaN: true})
}

// Encode writes v as json.dumps does with opts and returns the UTF-8 bytes
// of the result. Go maps, which have no order, are written with their keys
// sorted by code point; *Object keeps its order. A nil slice, map or pointer
// is null. With EnsureASCII false a lone surrogate cannot be written in
// UTF-8 and is the UnicodeEncodeError of Python's str.encode("utf-8").
func Encode(v any, opts Options) ([]byte, error) {
	e := &encoder{opts: opts, keySep: opts.KeySep, itemSep: opts.ItemSep, markers: map[marker]bool{}}
	if e.keySep == "" {
		e.keySep = ": "
	}
	if e.itemSep == "" {
		e.itemSep = ", "
		if opts.Indent != nil {
			e.itemSep = ","
		}
	}
	if opts.Indent != nil && *opts.Indent > 0 {
		e.indent = strings.Repeat(" ", *opts.Indent)
	}
	if err := e.value(v, 0); err != nil {
		return nil, err
	}
	if !opts.EnsureASCII {
		if err := checkEncodable(e.buf); err != nil {
			return nil, err
		}
	}
	return e.buf, nil
}

type marker struct {
	ptr uintptr
	n   int
}

type encoder struct {
	opts            Options
	buf             []byte
	indent          string
	itemSep, keySep string
	markers         map[marker]bool
}

func typeError(v any) error {
	name := fmt.Sprintf("%T", v)
	if _, ok := v.([]byte); ok {
		name = "bytes"
	}
	return &Error{Class: "TypeError", Message: fmt.Sprintf("Object of type %s is not JSON serializable", name)}
}

func (e *encoder) value(v any, level int) error {
	switch x := v.(type) {
	case nil:
		e.buf = append(e.buf, "null"...)
		return nil
	case bool:
		e.buf = strconv.AppendBool(e.buf, x)
		return nil
	case string:
		e.str(x)
		return nil
	case int:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
		return nil
	case int64:
		e.buf = strconv.AppendInt(e.buf, x, 10)
		return nil
	case float64:
		return e.float(x)
	case *big.Int:
		if x == nil {
			e.buf = append(e.buf, "null"...)
			return nil
		}
		return e.bigInt(x)
	case big.Int:
		return e.bigInt(&x)
	case *Object:
		if x == nil {
			e.buf = append(e.buf, "null"...)
			return nil
		}
		return e.object(x, level)
	case Object:
		return e.object(&x, level)
	case []any:
		if x == nil {
			e.buf = append(e.buf, "null"...)
			return nil
		}
		return e.list(reflect.ValueOf(x), len(x), func(i int) any { return x[i] }, level)
	case []byte:
		return typeError(v)
	}
	return e.reflected(v, level)
}

func (e *encoder) reflected(v any, level int) error {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		e.buf = strconv.AppendBool(e.buf, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buf = strconv.AppendInt(e.buf, rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.buf = strconv.AppendUint(e.buf, rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return e.float(rv.Float())
	case reflect.String:
		e.str(rv.String())
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			e.buf = append(e.buf, "null"...)
			return nil
		}
		return e.value(rv.Elem().Interface(), level)
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return typeError(v)
		}
		if rv.IsNil() {
			e.buf = append(e.buf, "null"...)
			return nil
		}
		return e.list(rv, rv.Len(), func(i int) any { return rv.Index(i).Interface() }, level)
	case reflect.Array:
		return e.list(reflect.Value{}, rv.Len(), func(i int) any { return rv.Index(i).Interface() }, level)
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return typeError(v)
		}
		if rv.IsNil() {
			e.buf = append(e.buf, "null"...)
			return nil
		}
		keys := make([]string, 0, rv.Len())
		values := map[string]reflect.Value{}
		for it := rv.MapRange(); it.Next(); {
			key := it.Key().String()
			keys = append(keys, key)
			values[key] = it.Value()
		}
		// Byte order of UTF-8 is code point order, the order of sorted().
		slices.Sort(keys)
		return e.pairs(marker{rv.Pointer(), -1}, keys, func(key string) any { return values[key].Interface() }, level)
	default:
		return typeError(v)
	}
	return nil
}

func (e *encoder) enter(m marker) error {
	if e.markers[m] {
		return &Error{Class: "ValueError", Message: "Circular reference detected"}
	}
	e.markers[m] = true
	return nil
}

func (e *encoder) newline(level int) {
	e.buf = append(e.buf, '\n')
	for range level {
		e.buf = append(e.buf, e.indent...)
	}
}

func (e *encoder) list(rv reflect.Value, n int, item func(int) any, level int) error {
	if n == 0 {
		e.buf = append(e.buf, "[]"...)
		return nil
	}
	if rv.IsValid() {
		m := marker{rv.Pointer(), n}
		if err := e.enter(m); err != nil {
			return err
		}
		defer delete(e.markers, m)
	}
	e.buf = append(e.buf, '[')
	if e.opts.Indent != nil {
		level++
		e.newline(level)
	}
	for i := range n {
		if i > 0 {
			e.buf = append(e.buf, e.itemSep...)
			if e.opts.Indent != nil {
				e.newline(level)
			}
		}
		if err := e.value(item(i), level); err != nil {
			return err
		}
	}
	if e.opts.Indent != nil {
		e.newline(level - 1)
	}
	e.buf = append(e.buf, ']')
	return nil
}

func (e *encoder) object(o *Object, level int) error {
	return e.pairs(marker{reflect.ValueOf(o.values).Pointer(), -1}, o.keys, func(key string) any { return o.values[key] }, level)
}

func (e *encoder) pairs(m marker, keys []string, value func(string) any, level int) error {
	if len(keys) == 0 {
		e.buf = append(e.buf, "{}"...)
		return nil
	}
	if err := e.enter(m); err != nil {
		return err
	}
	defer delete(e.markers, m)
	e.buf = append(e.buf, '{')
	if e.opts.Indent != nil {
		level++
		e.newline(level)
	}
	for i, key := range keys {
		if i > 0 {
			e.buf = append(e.buf, e.itemSep...)
			if e.opts.Indent != nil {
				e.newline(level)
			}
		}
		e.str(key)
		e.buf = append(e.buf, e.keySep...)
		if err := e.value(value(key), level); err != nil {
			return err
		}
	}
	if e.opts.Indent != nil {
		e.newline(level - 1)
	}
	e.buf = append(e.buf, '}')
	return nil
}

func (e *encoder) float(f float64) error {
	var text string
	switch {
	case math.IsNaN(f):
		text = "NaN"
	case math.IsInf(f, 1):
		text = "Infinity"
	case math.IsInf(f, -1):
		text = "-Infinity"
	default:
		e.buf = append(e.buf, FloatRepr(f)...)
		return nil
	}
	if e.opts.RejectNaN {
		return &Error{Class: "ValueError", Message: "Out of range float values are not JSON compliant: " + FloatRepr(f)}
	}
	e.buf = append(e.buf, text...)
	return nil
}

func (e *encoder) bigInt(n *big.Int) error {
	text := n.String()
	if len(strings.TrimPrefix(text, "-")) > maxIntDigits {
		return &Error{Class: "ValueError", Message: fmt.Sprintf(
			"Exceeds the limit (%d digits) for integer string conversion; use sys.set_int_max_str_digits() to increase the limit",
			maxIntDigits)}
	}
	e.buf = append(e.buf, text...)
	return nil
}

const hexDigits = "0123456789abcdef"

func (e *encoder) unit(c rune) {
	e.buf = append(e.buf, '\\', 'u', hexDigits[c>>12&0xF], hexDigits[c>>8&0xF], hexDigits[c>>4&0xF], hexDigits[c&0xF])
}

// str writes a string as encode_basestring_ascii (EnsureASCII) or
// encode_basestring of Python's json module.
func (e *encoder) str(s string) {
	e.buf = append(e.buf, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x80 {
			if !e.opts.EnsureASCII {
				e.buf = append(e.buf, c)
				i++
				continue
			}
			r, width := nextChar(s, i)
			i += width
			if r >= 0x10000 {
				r -= 0x10000
				e.unit(0xD800 | r>>10)
				e.unit(0xDC00 | r&0x3FF)
			} else {
				e.unit(r)
			}
			continue
		}
		i++
		switch c {
		case '"':
			e.buf = append(e.buf, '\\', '"')
		case '\\':
			e.buf = append(e.buf, '\\', '\\')
		case '\n':
			e.buf = append(e.buf, '\\', 'n')
		case '\r':
			e.buf = append(e.buf, '\\', 'r')
		case '\t':
			e.buf = append(e.buf, '\\', 't')
		case '\b':
			e.buf = append(e.buf, '\\', 'b')
		case '\f':
			e.buf = append(e.buf, '\\', 'f')
		default:
			if c < 0x20 || (c == 0x7F && e.opts.EnsureASCII) {
				e.unit(rune(c))
			} else {
				e.buf = append(e.buf, c)
			}
		}
	}
	e.buf = append(e.buf, '"')
}

// checkEncodable fails as str.encode("utf-8") of Python fails on a run of
// surrogates. Positions count code points.
func checkEncodable(b []byte) error {
	s := string(b)
	chars := 0
	for i := 0; i < len(s); {
		r, width := nextChar(s, i)
		if !isSurrogate(r) {
			i += width
			chars++
			continue
		}
		start, first := chars, r
		for i < len(s) {
			r, width = nextChar(s, i)
			if !isSurrogate(r) {
				break
			}
			i += width
			chars++
		}
		var msg string
		if chars-start == 1 {
			msg = fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", first, start)
		} else {
			msg = fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", start, chars-1)
		}
		return &Error{Class: "UnicodeEncodeError", Message: msg}
	}
	return nil
}

// FloatRepr is repr() of a Python float: the shortest text that reads back
// to f, in exponent form below 1e-4 and from 1e16 on.
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	text := strconv.FormatFloat(f, 'e', -1, 64)
	var b strings.Builder
	if text[0] == '-' {
		b.WriteByte('-')
		text = text[1:]
	}
	mantissa, exponent, _ := strings.Cut(text, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exp, _ := strconv.Atoi(exponent)
	point := exp + 1 // digits before the decimal point
	switch {
	case point <= -4 || point > 16:
		b.WriteByte(digits[0])
		if len(digits) > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if exp < 0 {
			b.WriteByte('-')
			exp = -exp
		} else {
			b.WriteByte('+')
		}
		if exp < 10 {
			b.WriteByte('0')
		}
		b.WriteString(strconv.Itoa(exp))
	case point <= 0:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -point))
		b.WriteString(digits)
	case point >= len(digits):
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", point-len(digits)))
		b.WriteString(".0")
	default:
		b.WriteString(digits[:point])
		b.WriteByte('.')
		b.WriteString(digits[point:])
	}
	return b.String()
}
