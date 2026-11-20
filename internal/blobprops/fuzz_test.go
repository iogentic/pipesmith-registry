package blobprops

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParse parses a properties file. It never panics; a result names a
// repository and a path of valid UTF-8, the path without a leading slash,
// and the only error is the ValueError of a number with too many digits.
// The seeds are the inputs of testdata/vectors/blobprops.json.
func FuzzParse(f *testing.F) {
	data, err := os.ReadFile("../../testdata/vectors/blobprops.json")
	if err != nil {
		f.Fatal(err)
	}
	var v struct {
		Tests []struct {
			Cases []struct {
				Input string `json:"input_b64"`
			} `json:"cases"`
		} `json:"tests"`
		Extra []struct {
			Input string `json:"input_b64"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		f.Fatal(err)
	}
	seeds := 0
	add := func(b64 string) {
		if b, err := base64.StdEncoding.DecodeString(b64); err == nil {
			f.Add(b)
			seeds++
		}
	}
	for _, test := range v.Tests {
		for _, c := range test.Cases {
			add(c.Input)
		}
	}
	for _, c := range v.Extra {
		add(c.Input)
	}
	if seeds < 100 {
		f.Fatalf("%d seeds from the vectors", seeds)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		props, err := Parse(data)
		if err != nil {
			var ve *ValueError
			if !errors.As(err, &ve) || props != nil {
				t.Fatalf("Parse(%q): %v with %+v, want a ValueError alone", data, err, props)
			}
			return
		}
		if props == nil {
			return
		}
		if props.Repo == "" || props.Path == "" || !utf8.ValidString(props.Repo) || !utf8.ValidString(props.Path) {
			t.Fatalf("Parse(%q): repository %q and path %q", data, props.Repo, props.Path)
		}
		if strings.HasPrefix(props.Path, "/") {
			t.Fatalf("Parse(%q): path %q keeps its leading slash", data, props.Path)
		}
		if props.ContentType != nil && (*props.ContentType == "" || !utf8.ValidString(*props.ContentType)) {
			t.Fatalf("Parse(%q): content type %q", data, *props.ContentType)
		}
		if props.SHA1 != nil && len(*props.SHA1) != 40 {
			t.Fatalf("Parse(%q): sha1 %q", data, *props.SHA1)
		}
		if props.Overflow && props.Size != 0 && props.CreatedMS != 0 {
			t.Fatalf("Parse(%q): overflow with size %d and creation %d", data, props.Size, props.CreatedMS)
		}
	})
}
