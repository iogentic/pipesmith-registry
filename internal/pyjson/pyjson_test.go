package pyjson_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The vectors come from tools/vectors/pyjson.py, which runs Python's json
// module, Path.read_text and Starlette's JSONResponse of the reference.
type vectorFile struct {
	Python  string       `json:"python"`
	Forms   []form       `json:"forms"`
	Encode  []encodeCase `json:"encode"`
	Decode  []decodeCase `json:"decode"`
	Floats  []floatCase  `json:"floats"`
	Ints    []intCase    `json:"ints"`
	Depth   depthCases   `json:"depth"`
	Ops     objectOps    `json:"object_ops"`
	Native  []nativeCase `json:"native"`
	Servers []serverFile `json:"server_files"`
	Escapes []escapeCase `json:"surrogateescape"`
}

type escapeCase struct {
	Input         string   `json:"input_b64"`
	Dumps         string   `json:"dumps"`
	DumpsKey      string   `json:"dumps_key"`
	Response      *string  `json:"response"`
	ResponseError *pyError `json:"response_error"`
	Roundtrip     string   `json:"roundtrip"`
}

type form struct {
	Name        string    `json:"name"`
	Indent      *int      `json:"indent"`
	EnsureASCII bool      `json:"ensure_ascii"`
	Separators  *[]string `json:"separators"`
	AllowNaN    bool      `json:"allow_nan"`
}

type pyError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
	Msg     string `json:"msg"`
	Pos     int    `json:"pos"`
	Lineno  int    `json:"lineno"`
	Colno   int    `json:"colno"`
}

type output struct {
	Text  *string  `json:"text"`
	Error *pyError `json:"error"`
}

type encodeCase struct {
	Name    string            `json:"name"`
	Source  string            `json:"source"`
	Value   string            `json:"value"`
	Outputs map[string]output `json:"outputs"`
}

type decodeCase struct {
	Name   string   `json:"name"`
	Via    string   `json:"via"`
	Input  string   `json:"input_b64"`
	Result *string  `json:"result"`
	Error  *pyError `json:"error"`
}

type floatCase struct {
	Bits   string   `json:"bits"`
	Repr   string   `json:"repr"`
	Dumps  string   `json:"dumps"`
	Finite bool     `json:"finite"`
	Error  *pyError `json:"error"`
}

type intCase struct {
	Pow10 int      `json:"pow10"`
	Sign  int      `json:"sign"`
	Dumps *string  `json:"dumps"`
	Error *pyError `json:"error"`
}

type depthCases struct {
	MaxDepth int `json:"max_depth"`
	Cases    []struct {
		Kind       string   `json:"kind"`
		Depth      int      `json:"depth"`
		Error      *pyError `json:"error"`
		BytesError *pyError `json:"bytes_error"`
	} `json:"cases"`
}

type objectOps struct {
	Ops   [][]any `json:"ops"`
	Dumps string  `json:"dumps"`
}

type nativeCase struct {
	Name     string `json:"name"`
	Dumps    string `json:"dumps"`
	Indent2  string `json:"indent2"`
	Response string `json:"response"`
}

type serverFile struct {
	Name    string `json:"name"`
	Bytes   string `json:"bytes_b64"`
	Form    string `json:"form"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/pyjson.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Encode) == 0 || len(v.Decode) == 0 || len(v.Floats) == 0 {
		t.Fatal("vector file holds no cases")
	}
	return &v
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func optionsOf(f form) pyjson.Options {
	opts := pyjson.Options{Indent: f.Indent, EnsureASCII: f.EnsureASCII, RejectNaN: !f.AllowNaN}
	if f.Separators != nil {
		opts.ItemSep, opts.KeySep = (*f.Separators)[0], (*f.Separators)[1]
	}
	return opts
}

// codePoints decodes generalized UTF-8 as the decoder writes it: surrogates
// are three byte sequences of their own.
func codePoints(s string) []rune {
	var out []rune
	b := []byte(s)
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80:
			out = append(out, rune(c))
			i++
		case c < 0xE0 && i+1 < len(b):
			out = append(out, rune(c&0x1F)<<6|rune(b[i+1]&0x3F))
			i += 2
		case c < 0xF0 && i+2 < len(b):
			out = append(out, rune(c&0x0F)<<12|rune(b[i+1]&0x3F)<<6|rune(b[i+2]&0x3F))
			i += 3
		case i+3 < len(b):
			out = append(out, rune(c&0x07)<<18|rune(b[i+1]&0x3F)<<12|rune(b[i+2]&0x3F)<<6|rune(b[i+3]&0x3F))
			i += 4
		default:
			out = append(out, -1)
			i++
		}
	}
	return out
}

// describe is the twin of describe() in tools/vectors/pyjson.py.
func describe(v any) string {
	switch x := v.(type) {
	case *pyjson.Object:
		parts := []string{}
		for key, value := range x.All() {
			parts = append(parts, describe(key)+":"+describe(value))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, len(x))
		for i, value := range x {
			parts[i] = describe(value)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		points := codePoints(x)
		parts := make([]string, len(points))
		for i, r := range points {
			parts[i] = strconv.FormatInt(int64(r), 16)
		}
		return "s<" + strings.Join(parts, ",") + ">"
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return "null"
	case int64:
		return "i" + strconv.FormatInt(x, 10)
	case *big.Int:
		return "i" + x.String()
	case float64:
		return "f" + pyjson.FloatRepr(x)
	}
	return fmt.Sprintf("?%T", v)
}

func classOf(err error) string {
	var syntax *pyjson.SyntaxError
	if errors.As(err, &syntax) {
		return "JSONDecodeError"
	}
	var other *pyjson.Error
	if errors.As(err, &other) {
		return other.Class
	}
	return fmt.Sprintf("?%T", err)
}

func checkError(t *testing.T, where string, err error, want *pyError) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want %s: %s", where, want.Class, want.Message)
		return
	}
	if got := classOf(err); got != want.Class {
		t.Errorf("%s: error class %s, want %s (%v)", where, got, want.Class, err)
	}
	if err.Error() != want.Message {
		t.Errorf("%s: error\n got %q\nwant %q", where, err.Error(), want.Message)
	}
	var syntax *pyjson.SyntaxError
	if want.Class == "JSONDecodeError" && errors.As(err, &syntax) {
		if syntax.Msg != want.Msg || syntax.Pos != want.Pos || syntax.Lineno != want.Lineno || syntax.Colno != want.Colno {
			t.Errorf("%s: fields %q %d %d %d, want %q %d %d %d", where, syntax.Msg, syntax.Pos, syntax.Lineno,
				syntax.Colno, want.Msg, want.Pos, want.Lineno, want.Colno)
		}
	}
	if pyjson.IsValueError(err) != (want.Class != "RecursionError" && want.Class != "TypeError") {
		t.Errorf("%s: IsValueError is %v for %s", where, pyjson.IsValueError(err), want.Class)
	}
}

func TestEncodeVectors(t *testing.T) {
	v := load(t)
	forms := map[string]form{}
	for _, f := range v.Forms {
		forms[f.Name] = f
	}
	checked := 0
	for _, c := range v.Encode {
		value, err := pyjson.DecodeString(c.Source)
		if err != nil {
			t.Errorf("%s: decode: %v", c.Name, err)
			continue
		}
		if got := describe(value); got != c.Value {
			t.Errorf("%s: decoded\n got %s\nwant %s", c.Name, got, c.Value)
		}
		for name, want := range c.Outputs {
			out, err := pyjson.Encode(value, optionsOf(forms[name]))
			where := c.Name + "/" + name
			if want.Error != nil {
				checkError(t, where, err, want.Error)
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			if string(out) != *want.Text {
				t.Errorf("%s:\n got %q\nwant %q", where, out, *want.Text)
			}
			checked++
		}
	}
	if checked < 300 {
		t.Errorf("only %d encode outputs checked", checked)
	}
}

func TestShortForms(t *testing.T) {
	v := load(t)
	for _, c := range v.Encode {
		value, err := pyjson.DecodeString(c.Source)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		for name, run := range map[string]func(any) ([]byte, error){
			"dumps":    pyjson.Dumps,
			"indent2":  func(x any) ([]byte, error) { return pyjson.DumpsIndent(x, 2) },
			"response": pyjson.ResponseBody,
		} {
			want := c.Outputs[name]
			out, err := run(value)
			if want.Error != nil {
				checkError(t, c.Name+"/"+name, err, want.Error)
			} else if err != nil || string(out) != *want.Text {
				t.Errorf("%s/%s: got %q %v, want %q", c.Name, name, out, err, *want.Text)
			}
		}
	}
}

func TestDecodeVectors(t *testing.T) {
	v := load(t)
	vias := map[string]int{}
	for _, c := range v.Decode {
		data := unb64(t, c.Input)
		var value any
		var err error
		switch c.Via {
		case "bytes":
			value, err = pyjson.Decode(data)
		case "str":
			value, err = pyjson.DecodeString(string(data))
		case "text":
			value, err = pyjson.DecodeText(data)
		default:
			t.Fatalf("%s: unknown via %q", c.Name, c.Via)
		}
		vias[c.Via]++
		where := c.Name + "/" + c.Via
		if c.Error != nil {
			checkError(t, where, err, c.Error)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", where, err)
			continue
		}
		if got := describe(value); got != *c.Result {
			t.Errorf("%s:\n got %s\nwant %s", where, got, *c.Result)
		}
	}
	for _, via := range []string{"bytes", "str", "text"} {
		if vias[via] < 5 {
			t.Errorf("only %d decode cases via %s", vias[via], via)
		}
	}
}

func TestFloatVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Floats {
		bits, err := strconv.ParseUint(c.Bits, 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		f := math.Float64frombits(bits)
		if got := pyjson.FloatRepr(f); got != c.Repr {
			t.Errorf("%s: repr %q, want %q", c.Bits, got, c.Repr)
		}
		out, err := pyjson.Dumps(f)
		if err != nil || string(out) != c.Dumps {
			t.Errorf("%s: dumps %q %v, want %q", c.Bits, out, err, c.Dumps)
		}
		out, err = pyjson.ResponseBody(f)
		if c.Finite {
			if err != nil || string(out) != c.Dumps {
				t.Errorf("%s: response %q %v, want %q", c.Bits, out, err, c.Dumps)
			}
		} else {
			checkError(t, c.Bits, err, c.Error)
		}
	}
}

func TestIntVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Ints {
		n := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(c.Pow10)), nil)
		n.Mul(n, big.NewInt(int64(c.Sign)))
		where := fmt.Sprintf("%d*10**%d", c.Sign, c.Pow10)
		out, err := pyjson.Dumps([]any{n})
		if c.Error != nil {
			checkError(t, where, err, c.Error)
		} else if err != nil || string(out) != *c.Dumps {
			t.Errorf("%s: got %d bytes %v, want %d bytes", where, len(out), err, len(*c.Dumps))
		}
	}
}

func TestDepthVectors(t *testing.T) {
	v := load(t)
	if v.Depth.MaxDepth < 1000 {
		t.Fatalf("max depth %d", v.Depth.MaxDepth)
	}
	for _, c := range v.Depth.Cases {
		var text string
		switch c.Kind {
		case "array":
			text = strings.Repeat("[", c.Depth) + strings.Repeat("]", c.Depth)
		case "object":
			text = strings.Repeat(`{"a":`, c.Depth) + "1" + strings.Repeat("}", c.Depth)
		case "mixed":
			text = strings.Repeat("[", c.Depth-1) + `{"a":1}` + strings.Repeat("]", c.Depth-1)
		}
		where := fmt.Sprintf("%s/%d", c.Kind, c.Depth)
		_, err := pyjson.DecodeString(text)
		if c.Error != nil {
			checkError(t, where, err, c.Error)
		} else if err != nil {
			t.Errorf("%s: %v", where, err)
		}
		_, err = pyjson.Decode([]byte(text))
		if c.BytesError != nil {
			checkError(t, where+"/bytes", err, c.BytesError)
		} else if err != nil {
			t.Errorf("%s/bytes: %v", where, err)
		}
	}
}

func TestObjectOps(t *testing.T) {
	v := load(t)
	o := pyjson.NewObject()
	for _, op := range v.Ops.Ops {
		switch op[0] {
		case "set":
			o.Set(op[1].(string), int64(op[2].(float64)))
		case "del":
			o.Delete(op[1].(string))
		}
	}
	out, err := pyjson.Dumps(o)
	if err != nil || string(out) != v.Ops.Dumps {
		t.Errorf("got %q %v, want %q", out, err, v.Ops.Dumps)
	}
}

func nativeValue(name string) any {
	str := func(s string) *string { return &s }
	switch name {
	case "config_form":
		type store struct {
			name, kind, root string
			repos            []string
			manifest         *string
		}
		stores := []any{}
		for _, s := range []store{
			{"demo-store", "nexus-file", "/srv/demo", nil, nil},
			{"demo-tree", "directory", "/srv/tree", []string{"demo-raw"}, str("/srv/manifest.tsv")},
		} {
			stores = append(stores, pyjson.NewObject().Set("name", s.name).Set("kind", s.kind).
				Set("root", s.root).Set("repos", s.repos).Set("manifest", s.manifest))
		}
		return pyjson.NewObject().Set("bind", "127.0.0.1").Set("port", 14100).Set("stores", stores).
			Set("docker_ports", map[string]int{"demo-docker-b": 14202, "demo-docker-a": 14201})
	case "go_ints":
		return []any{int8(-128), uint8(255), int16(-32768), uint16(65535), int32(-2147483648),
			uint32(4294967295), int64(math.MinInt64), uint64(math.MaxUint64), 0}
	case "go_float32":
		return []any{float32(0.1), float32(1.5), float32(16777216)}
	case "go_sorted_map":
		return map[string]int{"zeta": 1, "alpha": 2, "\u00e9": 3, "Z": 4, "a b": 5}
	case "go_nil_and_empty":
		var nilObject *pyjson.Object
		var nilPointer *int
		return pyjson.NewObject().Set("nil_slice", []string(nil)).Set("empty_slice", []string{}).
			Set("nil_map", map[string]any(nil)).Set("empty_map", map[string]any{}).
			Set("nil_object", nilObject).Set("empty_object", &pyjson.Object{}).Set("nil_pointer", nilPointer)
	case "go_nested_slices":
		return [][]any{{"demo-a", "demo-b"}, {}, {1, 2.5, true, nil}}
	}
	return nil
}

func TestNativeVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Native {
		value := nativeValue(c.Name)
		if value == nil {
			t.Errorf("%s: no Go value", c.Name)
			continue
		}
		for name, want := range map[string]string{"dumps": c.Dumps, "indent2": c.Indent2, "response": c.Response} {
			var out []byte
			var err error
			switch name {
			case "dumps":
				out, err = pyjson.Dumps(value)
			case "indent2":
				out, err = pyjson.DumpsIndent(value, 2)
			case "response":
				out, err = pyjson.ResponseBody(value)
			}
			if err != nil || string(out) != want {
				t.Errorf("%s/%s:\n got %q %v\nwant %q", c.Name, name, out, err, want)
			}
		}
	}
}

func TestServerFiles(t *testing.T) {
	v := load(t)
	if len(v.Servers) < 3 {
		t.Fatalf("%d server files", len(v.Servers))
	}
	for _, f := range v.Servers {
		data := unb64(t, f.Bytes)
		var out []byte
		var err error
		switch f.Form {
		case "indent2_newline":
			var value any
			value, err = pyjson.DecodeText(data)
			if err == nil {
				out, err = pyjson.DumpsIndent(value, 2)
				out = append(out, '\n')
			}
		case "dumps":
			doc := pyjson.NewObject().Set("errors", []any{pyjson.NewObject().Set("code", f.Code).Set("message", f.Message)})
			out, err = pyjson.Dumps(doc)
		default:
			t.Fatalf("%s: form %q", f.Name, f.Form)
		}
		if err != nil || string(out) != string(data) {
			t.Errorf("%s:\n got %q %v\nwant %q", f.Name, out, err, data)
		}
	}
}

func TestEncodeTypeErrors(t *testing.T) {
	type plain struct{ A int }
	for _, value := range []any{plain{1}, []byte("x"), make(chan int), map[int]string{1: "x"}, func() {}, complex(1, 2)} {
		_, err := pyjson.Dumps(value)
		if err == nil || classOf(err) != "TypeError" || pyjson.IsValueError(err) {
			t.Errorf("%T: got %v, want a TypeError", value, err)
		}
		if err != nil && !strings.HasSuffix(err.Error(), "is not JSON serializable") {
			t.Errorf("%T: message %q", value, err.Error())
		}
	}
	out, err := pyjson.Dumps([]any{[]any{"ok"}})
	if err != nil || string(out) != `[["ok"]]` {
		t.Errorf("nested slice: %q %v", out, err)
	}
}

func TestEncodeCircular(t *testing.T) {
	o := pyjson.NewObject()
	o.Set("self", o)
	_, err := pyjson.Dumps(o)
	if err == nil || classOf(err) != "ValueError" || err.Error() != "Circular reference detected" {
		t.Errorf("object: got %v", err)
	}
	list := make([]any, 1)
	list[0] = list
	_, err = pyjson.Dumps(list)
	if err == nil || err.Error() != "Circular reference detected" {
		t.Errorf("list: got %v", err)
	}
	shared := []any{"x"}
	out, err := pyjson.Dumps([]any{shared, shared})
	if err != nil || string(out) != `[["x"], ["x"]]` {
		t.Errorf("shared, not circular: %q %v", out, err)
	}
}

func TestObjectAPI(t *testing.T) {
	o := pyjson.NewObject().Set("b", 1).Set("a", 2).Set("b", 3)
	if o.Len() != 2 || strings.Join(o.Keys(), ",") != "b,a" {
		t.Errorf("keys %v", o.Keys())
	}
	if value, ok := o.Get("b"); !ok || value != 3 {
		t.Errorf("get b: %v %v", value, ok)
	}
	if _, ok := o.Get("missing"); ok || o.Has("missing") || !o.Has("a") {
		t.Error("presence")
	}
	o.Delete("b")
	o.Delete("missing")
	if strings.Join(o.Keys(), ",") != "a" {
		t.Errorf("after delete %v", o.Keys())
	}
	var zero pyjson.Object
	zero.Set("x", true)
	if out, err := pyjson.Dumps(zero); err != nil || string(out) != `{"x": true}` {
		t.Errorf("zero value object: %q %v", out, err)
	}
}

func TestDecodedTypes(t *testing.T) {
	value, err := pyjson.Decode([]byte(`[1, -9223372036854775808, 9223372036854775808, 1.0, "s", true, null, {}, []]`))
	if err != nil {
		t.Fatal(err)
	}
	list := value.([]any)
	if _, ok := list[0].(int64); !ok {
		t.Errorf("small int is %T", list[0])
	}
	if _, ok := list[1].(int64); !ok {
		t.Errorf("min int64 is %T", list[1])
	}
	if n, ok := list[2].(*big.Int); !ok || n.String() != "9223372036854775808" {
		t.Errorf("big int is %T %v", list[2], list[2])
	}
	if _, ok := list[3].(float64); !ok {
		t.Errorf("float is %T", list[3])
	}
	if _, ok := list[7].(*pyjson.Object); !ok {
		t.Errorf("object is %T", list[7])
	}
	if a, ok := list[8].([]any); !ok || a == nil {
		t.Errorf("empty array is %T %v", list[8], list[8])
	}
}

// A Go string with bytes that are not UTF-8 is the str Python makes of such
// bytes with surrogateescape, as os.fsdecode gives for a file name.
func TestSurrogateEscape(t *testing.T) {
	v := load(t)
	if len(v.Escapes) < 5 {
		t.Fatalf("%d surrogateescape cases", len(v.Escapes))
	}
	for _, c := range v.Escapes {
		s := string(unb64(t, c.Input))
		if out, err := pyjson.Dumps(s); err != nil || string(out) != c.Dumps {
			t.Errorf("%q: dumps %q %v, want %q", s, out, err, c.Dumps)
		}
		if out, err := pyjson.Dumps(pyjson.NewObject().Set(s, 1)); err != nil || string(out) != c.DumpsKey {
			t.Errorf("%q: key %q %v, want %q", s, out, err, c.DumpsKey)
		}
		out, err := pyjson.ResponseBody(s)
		if c.ResponseError != nil {
			checkError(t, fmt.Sprintf("%q/response", s), err, c.ResponseError)
		} else if err != nil || string(out) != *c.Response {
			t.Errorf("%q: response %q %v", s, out, err)
		}
		value, err := pyjson.DecodeString(`"` + s + `"`)
		if err != nil || describe(value) != c.Roundtrip {
			t.Errorf("%q: decoded %s %v, want %s", s, describe(value), err, c.Roundtrip)
		}
	}
}
