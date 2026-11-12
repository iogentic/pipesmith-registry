package maven

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

// Cases of tools/vectors/maven.py for the unexported pieces: str.lower,
// str.isdigit, the sort of the merge and expat's check of the byte map a
// codec gives for an encoding it does not know.

type internalVectors struct {
	Lower []struct {
		Text  string `json:"text"`
		Value string `json:"value"`
	} `json:"lower"`
	IsDigit []struct {
		Text  string `json:"text"`
		Value bool   `json:"value"`
	} `json:"isdigit"`
	Versions struct {
		Sort []struct {
			Input  []string `json:"input"`
			Sorted []string `json:"sorted"`
		} `json:"sort"`
	} `json:"versions"`
	UnknownEncodingMaps []struct {
		Name   string `json:"name"`
		Map    []int  `json:"map"`
		Merges []struct {
			Name      string   `json:"name"`
			Documents []string `json:"documents"`
			Value     *string  `json:"value"`
			Error     string   `json:"error"`
		} `json:"merges"`
	} `json:"unknown_encoding_maps"`
}

func loadInternal(t *testing.T) *internalVectors {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/maven.json")
	if err != nil {
		t.Fatal(err)
	}
	var v internalVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Lower) < 20 || len(v.IsDigit) < 10 || len(v.Versions.Sort) < 5 || len(v.UnknownEncodingMaps) < 10 {
		t.Fatalf("%d lower, %d isdigit, %d sorts, %d maps", len(v.Lower), len(v.IsDigit), len(v.Versions.Sort),
			len(v.UnknownEncodingMaps))
	}
	return &v
}

func TestPyLower(t *testing.T) {
	changed := 0
	for _, c := range loadInternal(t).Lower {
		if got := pyLower(c.Text); got != c.Value {
			t.Errorf("pyLower(%+q) = %+q, want %+q", c.Text, got, c.Value)
		}
		if c.Text != c.Value {
			changed++
		}
	}
	if changed < 20 {
		t.Fatalf("only %d samples change", changed)
	}
}

func TestPyIsDigit(t *testing.T) {
	yes := 0
	for _, c := range loadInternal(t).IsDigit {
		if got := pyIsDigit(c.Text); got != c.Value {
			t.Errorf("pyIsDigit(%+q) = %v", c.Text, got)
		}
		if c.Value {
			yes++
		}
	}
	if yes < 5 {
		t.Fatalf("only %d digit samples", yes)
	}
}

func TestSortVersions(t *testing.T) {
	for _, c := range loadInternal(t).Versions.Sort {
		got := append([]string(nil), c.Input...)
		if err := sortVersions(got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c.Sorted) {
			t.Errorf("sort of %q\n got %q\nwant %q", c.Input, got, c.Sorted)
		}
	}
}

// TestUnknownEncodingMaps registers each map of the vectors under its demo
// name, as the script registers a codec with Python, and merges the
// documents that declare it.
func TestUnknownEncodingMaps(t *testing.T) {
	refused := 0
	for _, m := range loadInternal(t).UnknownEncodingMaps {
		var table [256]rune
		for i, c := range m.Map {
			table[i] = rune(c)
		}
		lookup := func(name string) codecResult {
			if name == "demo_"+m.Name {
				return codecResult{kind: codecMap, table: &table}
			}
			return pythonCodec(name)
		}
		if _, ok := newUnknownEncoding(&table); !ok {
			refused++
		}
		for _, c := range m.Merges {
			var documents [][]byte
			for _, d := range c.Documents {
				b, err := base64.StdEncoding.DecodeString(d)
				if err != nil {
					t.Fatal(err)
				}
				documents = append(documents, b)
			}
			got, err := mergeMetadata(documents, lookup)
			if err != nil {
				t.Errorf("%s: %v", m.Name, err)
				continue
			}
			if c.Value == nil {
				if got != nil {
					t.Errorf("%s %q: got %q, want None", m.Name, documents[0], got)
				}
				continue
			}
			want, _ := base64.StdEncoding.DecodeString(*c.Value)
			if string(got) != string(want) {
				t.Errorf("%s %q:\n got %q\nwant %q", m.Name, documents[0], got, want)
			}
		}
	}
	if refused < 3 {
		t.Fatalf("only %d maps refused", refused)
	}
}

func TestPythonCodecLookup(t *testing.T) {
	for _, c := range []struct {
		name string
		kind codecKind
	}{
		{"cp1252", codecMap}, {"Windows-1252", codecMap}, {"latin1", codecMap}, {"ISO_8859_1", codecMap},
		{"iso8859.1", codecMap}, {"utf8", codecMap}, {"hex", codecNotText}, {"aliases", codecUnknown},
		{"mbcs", codecUnknown}, {"demo-encoding", codecUnknown}, {"shift_jis", codecCaught}, {"utf-16", codecCaught},
	} {
		if got := pythonCodec(c.name); got.kind != c.kind {
			t.Errorf("pythonCodec(%q) = %v, want %v", c.name, got.kind, c.kind)
		}
	}
}

func TestRecursionLimit(t *testing.T) {
	deep := func(depth int) []byte {
		return []byte("<metadata><versioning><snapshot/>" + strings.Repeat("<x>", depth) + strings.Repeat("</x>", depth) +
			"</versioning></metadata>")
	}
	// deep(n) nests n+2 elements.
	if got, err := MergeMetadata([][]byte{deep(maxSnapshotDepth - 2)}); err != nil || got == nil {
		t.Fatalf("depth at the limit: %v", err)
	}
	_, err := MergeMetadata([][]byte{deep(maxSnapshotDepth - 1)})
	var pyErr *pyjson.Error
	if !errors.As(err, &pyErr) || pyErr.Class != "RecursionError" {
		t.Fatalf("depth beyond the limit: %v", err)
	}
	// Only the document that is written out counts.
	flat := []byte("<metadata>" + strings.Repeat("<x>", 5000) + strings.Repeat("</x>", 5000) + "</metadata>")
	if got, err := MergeMetadata([][]byte{flat, []byte("<metadata/>")}); err != nil || got == nil {
		t.Fatalf("deep document without snapshot: %v", err)
	}
}

// TestHasPrefixDoesNotCopy: hasPrefix runs once per "]" and "<" of a
// document; a copy of the rest of the text there makes a large document
// quadratic.
func TestHasPrefixDoesNotCopy(t *testing.T) {
	s := &source{b: []byte(strings.Repeat("]", 1<<16) + "]]>")}
	if !s.hasPrefix(1<<16, "]]>") || s.hasPrefix(0, "]]>") || s.hasPrefix(1<<16+2, "]]>") || s.hasPrefix(1<<20, "]") {
		t.Fatal("hasPrefix gives wrong answers")
	}
	if allocs := testing.AllocsPerRun(100, func() { s.hasPrefix(10, "]]>") }); allocs != 0 {
		t.Fatalf("hasPrefix allocates %.0f times", allocs)
	}
}
