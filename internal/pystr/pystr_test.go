package pystr_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// The vectors come from tools/vectors/pystr.py, which runs str, int and
// pathlib of the reference Python.
type vectorFile struct {
	Python     string                          `json:"python"`
	Unicode    string                          `json:"unicode"`
	MaxUnicode int                             `json:"max_unicode"`
	Repr       []struct{ Source, Repr string } `json:"repr"`
	StrRepr    []struct {
		Input string `json:"input_b64"`
		Repr  string `json:"repr"`
	} `json:"str_repr"`
	Printable [][2]rune   `json:"printable"`
	Space     [][2]rune   `json:"space"`
	Casefold  []foldCase  `json:"casefold"`
	JoinNames []string    `json:"join_names"`
	Decimals  [][2]int    `json:"decimals"`
	Ints      []intCase   `json:"ints"`
	Strip     []stripCase `json:"strip"`
	Paths     []pathCase  `json:"paths"`
	Order     []string    `json:"order_b64"`
}

type foldCase struct {
	CP     rune
	Folded []rune
}

func (f *foldCase) UnmarshalJSON(data []byte) error {
	var raw [2]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[0], &f.CP); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &f.Folded)
}

type intCase struct {
	Input string  `json:"input_b64"`
	Value *string `json:"value"`
	Error *struct {
		Class   string `json:"class"`
		Message string `json:"message"`
	} `json:"error"`
}

type stripCase struct {
	Input string `json:"input_b64"`
	Strip string `json:"strip_b64"`
}

type pathCase struct {
	Input  string   `json:"input_b64"`
	Str    string   `json:"str_b64"`
	Parent string   `json:"parent_b64"`
	Joins  []string `json:"joins"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/pystr.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Repr) == 0 || len(v.Casefold) == 0 || len(v.Ints) == 0 || len(v.Paths) == 0 {
		t.Fatal("vector file holds no cases")
	}
	return &v
}

func unb64(t *testing.T, s string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// inRuns reports whether cp lies in one of the sorted runs.
func inRuns(runs [][2]rune) func(rune) bool {
	return func(cp rune) bool {
		lo, hi := 0, len(runs)
		for lo < hi {
			mid := (lo + hi) / 2
			switch {
			case cp < runs[mid][0]:
				hi = mid
			case cp > runs[mid][1]:
				lo = mid + 1
			default:
				return true
			}
		}
		return false
	}
}

// mismatches lists the code points where got and want differ, as runs.
func mismatches(max rune, got, want func(rune) bool) []string {
	var out []string
	start := rune(-1)
	for cp := rune(0); cp <= max+1; cp++ {
		bad := cp <= max && got(cp) != want(cp)
		if bad && start < 0 {
			start = cp
		} else if !bad && start >= 0 {
			out = append(out, fmt.Sprintf("%04X..%04X want %v", start, cp-1, want(start)))
			start = -1
		}
	}
	return out
}

func TestReprVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Repr {
		value, err := pyjson.DecodeString(c.Source)
		if err != nil {
			t.Fatalf("%s: %v", c.Source, err)
		}
		if got := pystr.Repr(value); got != c.Repr {
			t.Errorf("repr(%s) = %q, want %q", c.Source, got, c.Repr)
		}
	}
	for _, c := range v.StrRepr {
		in := unb64(t, c.Input)
		if got := pystr.Repr(in); got != c.Repr {
			t.Errorf("repr(%q) = %q, want %q", in, got, c.Repr)
		}
	}
}

func TestReprNative(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{nil, "None"},
		{true, "True"},
		{8793, "8793"},
		{int64(-5), "-5"},
		{[]string{"a", "it's"}, `['a', "it's"]`},
		{[]string{}, "[]"},
		{"\xff", `'\udcff'`},         // a byte that is not UTF-8 reads as surrogateescape
		{"\xed\xa0\x80", `'\ud800'`}, // a surrogate in its three byte form
		{"café 中", "'café 中'"},       // printable characters stay
	}
	for _, c := range cases {
		if got := pystr.Repr(c.value); got != c.want {
			t.Errorf("Repr(%#v) = %q, want %q", c.value, got, c.want)
		}
	}
}

func TestPrintableWholeCodeSpace(t *testing.T) {
	v := load(t)
	want := inRuns(v.Printable)
	if bad := mismatches(rune(v.MaxUnicode), pystr.IsPrintable, want); len(bad) > 0 {
		t.Errorf("isprintable differs from Python %s (Unicode %s) in %d runs: %s", v.Python, v.Unicode, len(bad),
			strings.Join(bad, ", "))
	}
	// the positive twin: printable characters exist on both sides of the ASCII range
	if !pystr.IsPrintable('a') || !pystr.IsPrintable('é') || pystr.IsPrintable('\n') {
		t.Error("IsPrintable is wrong on a, e acute or newline")
	}
}

func TestSpaceWholeCodeSpace(t *testing.T) {
	v := load(t)
	want := inRuns(v.Space)
	if bad := mismatches(rune(v.MaxUnicode), pystr.IsSpace, want); len(bad) > 0 {
		t.Errorf("isspace differs from Python in: %s", strings.Join(bad, ", "))
	}
	if !pystr.IsSpace(' ') || !pystr.IsSpace('\x1c') || pystr.IsSpace('a') {
		t.Error("IsSpace is wrong on space, 0x1c or a")
	}
}

func TestCasefoldWholeCodeSpace(t *testing.T) {
	v := load(t)
	want := map[rune]string{}
	for _, c := range v.Casefold {
		want[c.CP] = string(c.Folded)
	}
	var bad []string
	for cp := rune(0); cp <= rune(v.MaxUnicode); cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue // checked below in their three byte form
		}
		expected, ok := want[cp]
		if !ok {
			expected = string(cp)
		}
		if got := pystr.Casefold(string(cp)); got != expected {
			bad = append(bad, fmt.Sprintf("%04X: got %+q want %+q", cp, got, expected))
		}
	}
	if len(bad) > 0 {
		t.Errorf("casefold differs from Python %s (Unicode %s) at %d code points: %s", v.Python, v.Unicode, len(bad),
			strings.Join(bad, "; "))
	}
	if got := pystr.Casefold("Straße \xed\xa0\x80"); got != "strasse \xed\xa0\x80" {
		t.Errorf("Casefold of a string = %q", got)
	}
	if got := pystr.Casefold("A\xffB"); got != "a\xed\xb3\xbfb" {
		t.Errorf("Casefold of a byte that is not UTF-8 = %q", got)
	}
}

func TestDecimalDigits(t *testing.T) {
	v := load(t)
	digits := map[rune]int{}
	for _, d := range v.Decimals {
		digits[rune(d[0])] = d[1]
	}
	var bad []string
	for cp := rune(0); cp <= rune(v.MaxUnicode); cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue
		}
		want, ok := digits[cp]
		if !ok {
			want = -1
		}
		if got := pystr.DecimalValue(cp); got != want {
			bad = append(bad, fmt.Sprintf("%04X: got %d want %d", cp, got, want))
		}
	}
	if len(bad) > 0 {
		t.Errorf("decimal values differ at %d code points: %s", len(bad), strings.Join(bad, "; "))
	}
}

func TestParseIntVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Ints {
		in := unb64(t, c.Input)
		got, err := pystr.ParseInt(in)
		if c.Error != nil {
			var pe *pyjson.Error
			if !errors.As(err, &pe) || pe.Class != c.Error.Class || pe.Message != c.Error.Message {
				t.Errorf("int(%q): got %v %v, want %s %q", in, got, err, c.Error.Class, c.Error.Message)
			}
			if !pyjson.IsValueError(err) {
				t.Errorf("int(%q): %v is no ValueError", in, err)
			}
			continue
		}
		if err != nil || got.String() != *c.Value {
			t.Errorf("int(%q) = %v %v, want %s", in, got, err, *c.Value)
		}
	}
}

func TestStripVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Strip {
		in, want := unb64(t, c.Input), unb64(t, c.Strip)
		if got := pystr.Strip(in); got != want {
			t.Errorf("%q.strip() = %q, want %q", in, got, want)
		}
	}
	if got := pystr.Strip(" a\xffb "); got != "a\xffb" {
		t.Errorf("Strip changed the bytes inside: %q", got)
	}
}

func TestPathVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Paths {
		in := unb64(t, c.Input)
		if got, want := pystr.Path(in), unb64(t, c.Str); got != want {
			t.Errorf("str(PurePosixPath(%q)) = %q, want %q", in, got, want)
		}
		if got, want := pystr.Parent(in), unb64(t, c.Parent); got != want {
			t.Errorf("PurePosixPath(%q).parent = %q, want %q", in, got, want)
		}
		for i, name := range v.JoinNames {
			if got, want := pystr.Join(in, name), unb64(t, c.Joins[i]); got != want {
				t.Errorf("PurePosixPath(%q) / %q = %q, want %q", in, name, got, want)
			}
		}
	}
}

func TestCompareIsCodePointOrder(t *testing.T) {
	v := load(t)
	order := make([]string, len(v.Order))
	for i, s := range v.Order {
		order[i] = unb64(t, s)
	}
	for i := range order {
		for j := range order {
			got := pystr.Compare(order[i], order[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%q, %q) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	// bytes that are not UTF-8 sort as their surrogateescape code points
	if pystr.Compare("\xff", "") >= 0 || pystr.Compare("\xff", "퟿") <= 0 {
		t.Error("a byte 0xff does not sort as U+DCFF")
	}
}

func TestRunes(t *testing.T) {
	got := pystr.Runes("a\xed\xa0\x80\xffé")
	want := []rune{'a', 0xD800, 0xDCFF, 0xE9}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Runes = %U, want %U", got, want)
	}
	if s := pystr.FromRunes(want); s != "a\xed\xa0\x80\xed\xb3\xbfé" {
		t.Errorf("FromRunes = %q", s)
	}
}
