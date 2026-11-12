package blobprops

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The vectors come from tools/vectors/blobprops.py, which runs the test
// functions of tests/test_blobprops.py of the reference with a recorder
// around parse() and adds invented inputs for load() and parse().
type vectorFile struct {
	IntMaxStrDigits  int        `json:"int_max_str_digits"`
	PythonWhitespace []rune     `json:"python_whitespace"`
	Tests            []testCase `json:"tests"`
	Extra            []extra    `json:"extra"`
}

type testCase struct {
	Test  string      `json:"test"`
	Cases []parseCase `json:"cases"`
}

type parseCase struct {
	Input  string       `json:"input_b64"`
	Expect *expectProps `json:"expect"`
	Error  *pythonError `json:"error"`
}

type extra struct {
	Name      string       `json:"name"`
	Input     string       `json:"input_b64"`
	Load      [][2]string  `json:"load"`
	LoadError *pythonError `json:"load_error"`
	Expect    *expectProps `json:"expect"`
	Error     *pythonError `json:"error"`
}

type pythonError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

type expectProps struct {
	Repo        string  `json:"repo"`
	Path        string  `json:"path"`
	Size        int64   `json:"size"`
	SHA1        *string `json:"sha1"`
	ContentType *string `json:"content_type"`
	CreatedMS   int64   `json:"created_ms"`
	Deleted     bool    `json:"deleted"`
	IsCopy      bool    `json:"is_copy"`
	Overflow    bool    `json:"overflow"`
}

func vectors(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/blobprops.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func decode(t *testing.T, text string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func latin1(data []byte) string {
	runes := make([]rune, len(data))
	for i, b := range data {
		runes[i] = rune(b)
	}
	return string(runes)
}

func show(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func checkParse(t *testing.T, label string, data []byte, want *expectProps, wantErr *pythonError) {
	t.Helper()
	got, err := Parse(data)
	if wantErr != nil {
		var ve *ValueError
		if !errors.As(err, &ve) || wantErr.Class != "ValueError" || err.Error() != wantErr.Message {
			t.Errorf("%s: Parse error %v, want %s %q", label, err, wantErr.Class, wantErr.Message)
		}
		if got != nil {
			t.Errorf("%s: Parse with an error returned %+v", label, got)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: Parse error %v", label, err)
		return
	}
	if want == nil {
		if got != nil {
			t.Errorf("%s: Parse = %+v, want nil", label, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s: Parse = nil, want %+v", label, *want)
		return
	}
	if got.Repo != want.Repo || got.Path != want.Path || got.Size != want.Size || got.CreatedMS != want.CreatedMS ||
		show(got.SHA1) != show(want.SHA1) || show(got.ContentType) != show(want.ContentType) ||
		got.Deleted != want.Deleted || got.IsCopy != want.IsCopy || got.Overflow != want.Overflow {
		t.Errorf("%s: Parse = %+v sha1=%s type=%s, want %+v sha1=%s type=%s", label, *got, show(got.SHA1),
			show(got.ContentType), *want, show(want.SHA1), show(want.ContentType))
	}
}

func TestReferenceTests(t *testing.T) {
	v := vectors(t)
	if len(v.Tests) != 21 {
		t.Fatalf("%d test functions in the vectors, want 21", len(v.Tests))
	}
	calls := 0
	for _, tc := range v.Tests {
		for i, c := range tc.Cases {
			checkParse(t, tc.Test+"#"+strconv.Itoa(i), decode(t, c.Input), c.Expect, c.Error)
			calls++
		}
	}
	if calls < 21 {
		t.Fatalf("only %d parse calls", calls)
	}
}

func TestExtraParse(t *testing.T) {
	v := vectors(t)
	if len(v.Extra) == 0 {
		t.Fatal("no extra cases")
	}
	for _, c := range v.Extra {
		checkParse(t, c.Name, decode(t, c.Input), c.Expect, c.Error)
	}
}

func TestExtraLoad(t *testing.T) {
	v := vectors(t)
	for _, c := range v.Extra {
		got, err := Load(latin1(decode(t, c.Input)))
		if c.LoadError != nil {
			if err == nil || c.LoadError.Class != "ValueError" || err.Error() != c.LoadError.Message {
				t.Errorf("%s: Load error %v, want %q", c.Name, err, c.LoadError.Message)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Load error %v", c.Name, err)
			continue
		}
		want := map[string]string{}
		for _, pair := range c.Load {
			want[string(decode(t, pair[0]))] = string(decode(t, pair[1]))
		}
		if len(got) != len(want) {
			t.Errorf("%s: Load gave %d keys %q, want %d %q", c.Name, len(got), got, len(want), want)
			continue
		}
		for key, value := range want {
			if g, ok := got[key]; !ok || g != value {
				t.Errorf("%s: Load[%q] = %q (present %v), want %q", c.Name, key, g, ok, value)
			}
		}
	}
}

func TestPythonWhitespace(t *testing.T) {
	v := vectors(t)
	want := map[rune]bool{}
	for _, r := range v.PythonWhitespace {
		want[r] = true
	}
	if len(want) == 0 {
		t.Fatal("no whitespace in the vectors")
	}
	for r := rune(0); r <= 0x10FFFF; r++ {
		if pyIsSpace(r) != want[r] {
			t.Errorf("pyIsSpace(U+%04X) = %v, want %v", r, pyIsSpace(r), want[r])
		}
	}
}

func TestDigitLimit(t *testing.T) {
	v := vectors(t)
	if v.IntMaxStrDigits != maxIntDigits {
		t.Fatalf("the reference reads %d digits, the port %d", v.IntMaxStrDigits, maxIntDigits)
	}
}

// Native cases, written before the port.

func props(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestParseNative(t *testing.T) {
	sha := "48d654e40570cd4b5bd21c06e77d45274d568e9e"
	got, err := Parse(props("#c", "@Bucket.repo-name=demo-raw", "@BlobStore.blob-name=//a/b.txt", "size = 5",
		"creationTime:-7", "sha1="+strings.ToUpper(sha), "@BlobStore.content-type text/plain"))
	if err != nil || got == nil {
		t.Fatalf("Parse = %v, %v", got, err)
	}
	if got.Repo != "demo-raw" || got.Path != "a/b.txt" || got.Size != 5 || got.CreatedMS != -7 ||
		show(got.SHA1) != sha || show(got.ContentType) != "text/plain" || got.Deleted || got.IsCopy || got.Overflow {
		t.Fatalf("Parse = %+v", *got)
	}
	for _, data := range [][]byte{
		nil,
		props("@Bucket.repo-name=demo-raw", "size=1", "creationTime=1"),
		props("@Bucket.repo-name=demo-raw", "@BlobStore.blob-name=/a", "size=-1", "creationTime=1"),
		props("@Bucket.repo-name=demo-raw", "@BlobStore.blob-name=/a\\uD800", "size=1", "creationTime=1"),
		props("@Bucket.repo-name=demo-raw", "@BlobStore.blob-name=/a\\u00zz", "size=1", "creationTime=1"),
	} {
		if got, err := Parse(data); got != nil || err != nil {
			t.Errorf("Parse(%q) = %+v, %v; want nil, nil", data, got, err)
		}
	}
	over, err := Parse(props("@Bucket.repo-name=demo-raw", "@BlobStore.blob-name=/a", "size=99999999999999999999",
		"creationTime=1", "deleted=true"))
	if err != nil || over == nil || !over.Overflow || !over.Deleted || over.Size != 0 || over.CreatedMS != 1 {
		t.Fatalf("Parse with a size beyond int64 = %+v, %v", over, err)
	}
	_, err = Parse(props("size=" + strings.Repeat("1", maxIntDigits+1)))
	var ve *ValueError
	if !errors.As(err, &ve) {
		t.Fatalf("Parse with %d digits: error %v, want a ValueError", maxIntDigits+1, err)
	}
}

func TestLoadNative(t *testing.T) {
	got, err := Load("a\\\n  b=c\\u00e4\\uD83D\\uDE00\nk\n\\uDE00=x")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ab": "cä\U0001F600", "k": "", "\xed\xb8\x80": "x"}
	if len(got) != len(want) {
		t.Fatalf("Load = %q, want %q", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("Load = %q, want %q", got, want)
		}
	}
	if _, err := Load("k=\\u12"); err == nil {
		t.Fatal("Load of a short escape gave no error")
	}
}
