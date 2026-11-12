package npm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

// The vectors come from tools/vectors/npm.py, which runs npm.py of the
// reference over the cases of tests/test_npm.py and invented documents and
// tarballs. Documents and manifests are compared as the bytes json.dumps
// writes, the form server.py sends.

type pyErr struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

type vectorFile struct {
	Python    string `json:"python"`
	Constants struct {
		TarballDirectory    string   `json:"TARBALL_DIRECTORY"`
		MergedKeys          []string `json:"MERGED_KEYS"`
		PackageJSON         string   `json:"PACKAGE_JSON"`
		MaxPackageJSONBytes int      `json:"MAX_PACKAGE_JSON_BYTES"`
		MaxNameLength       int      `json:"MAX_NAME_LENGTH"`
		ServerManifestDepth int      `json:"SERVER_MANIFEST_DEPTH"`
		RecursionLimit      int      `json:"RECURSION_LIMIT"`
	} `json:"constants"`
	Documents map[string]string `json:"documents_b64"`
	ParsePath []struct {
		Path   string `json:"path_b64"`
		Result *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"result"`
		PackageName *string `json:"package_name"`
		Unscoped    string  `json:"unscoped_b64"`
	} `json:"parse_path"`
	VersionKey []struct {
		Version string `json:"version_b64"`
		Key     []any  `json:"key"`
		Error   *pyErr `json:"error"`
	} `json:"version_key"`
	VersionOrder []struct {
		Versions     []string `json:"versions_b64"`
		Sorted       []string `json:"sorted_b64"`
		SortedError  *pyErr   `json:"sorted_error"`
		Highest      *string  `json:"highest_b64"`
		HighestError *pyErr   `json:"highest_error"`
	} `json:"version_order"`
	IsoTime []struct {
		CreatedMS int64   `json:"created_ms"`
		ISO       *string `json:"iso"`
		Error     *pyErr  `json:"error"`
	} `json:"iso_time"`
	DigitRuns [][2]rune `json:"digit_runs"`
	FileName  []struct {
		URL    string  `json:"url_b64"`
		Result *string `json:"result_b64"`
	} `json:"file_name"`
	TarballPaths []struct {
		Document string `json:"document"`
		Result   string `json:"result_b64"`
	} `json:"tarball_paths"`
	SelectVersions []struct {
		Document string   `json:"document"`
		Keep     []string `json:"keep_b64"`
		Result   *string  `json:"result_b64"`
		Error    *pyErr   `json:"error"`
	} `json:"select_versions"`
	MergeDocuments []struct {
		Documents []string `json:"documents"`
		Result    *string  `json:"result_b64"`
	} `json:"merge_documents"`
	RewriteTarballs []struct {
		Document string  `json:"document"`
		Base     string  `json:"base_b64"`
		Result   *string `json:"result_b64"`
		Error    *pyErr  `json:"error"`
	} `json:"rewrite_tarballs"`
	VersionObject []struct {
		Document  string  `json:"document"`
		Reference string  `json:"reference_b64"`
		Result    *string `json:"result_b64"`
	} `json:"version_object"`
	PackageManifest []struct {
		Name     string  `json:"name"`
		Data     string  `json:"data_b64"`
		Manifest *string `json:"manifest_b64"`
		Error    *pyErr  `json:"error"`
	} `json:"package_manifest"`
	DocumentFromTarballs []struct {
		Name     string `json:"name"`
		Tarballs []struct {
			FileName  string `json:"file_name"`
			Data      string `json:"data_b64"`
			CreatedMS int64  `json:"created_ms"`
		} `json:"tarballs"`
		Result *string `json:"result_b64"`
		Error  *pyErr  `json:"error"`
	} `json:"document_from_tarballs"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/npm.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v.Python == "" || len(v.PackageManifest) == 0 || len(v.ParsePath) == 0 {
		t.Fatal("vector file is incomplete")
	}
	return &v
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unb64s(t *testing.T, s string) string {
	return string(unb64(t, s))
}

// document decodes one of the stored documents of the vector file afresh.
func (v *vectorFile) document(t *testing.T, name string) *pyjson.Object {
	t.Helper()
	encoded, ok := v.Documents[name]
	if !ok {
		t.Fatalf("no document %q", name)
	}
	value, err := pyjson.Decode(unb64(t, encoded))
	if err != nil {
		t.Fatal(err)
	}
	doc, ok := value.(*pyjson.Object)
	if !ok {
		t.Fatalf("document %q is no object", name)
	}
	return doc
}

func dump(t *testing.T, value any) []byte {
	t.Helper()
	out, err := pyjson.Dumps(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameError checks err against the class and message of the reference.
func sameError(t *testing.T, label string, err error, want *pyErr) {
	t.Helper()
	var got *pyjson.Error
	if !errors.As(err, &got) {
		t.Errorf("%s: error %v (%T), want %s: %s", label, err, err, want.Class, want.Message)
		return
	}
	if got.Class != want.Class || got.Message != want.Message {
		t.Errorf("%s: error %s: %q, want %s: %q", label, got.Class, got.Message, want.Class, want.Message)
	}
}

// sameBytes compares a result with the json.dumps bytes of the reference;
// want nil stands for None.
func sameBytes(t *testing.T, label string, got *pyjson.Object, want *string) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s: got %s, want None", label, dump(t, got))
		}
		return
	}
	if got == nil {
		t.Errorf("%s: got None, want %s", label, unb64(t, *want))
		return
	}
	if out, w := dump(t, got), unb64(t, *want); !bytes.Equal(out, w) {
		t.Errorf("%s:\n got %s\nwant %s", label, out, w)
	}
}

func TestConstantsVectors(t *testing.T) {
	v := load(t)
	c := v.Constants
	if TarballDirectory != c.TarballDirectory || PackageJSON != c.PackageJSON ||
		MaxPackageJSONBytes != c.MaxPackageJSONBytes || MaxNameLength != c.MaxNameLength {
		t.Errorf("constants differ from the reference: %+v", c)
	}
	if !reflect.DeepEqual(MergedKeys[:], c.MergedKeys) {
		t.Errorf("MergedKeys %v, want %v", MergedKeys, c.MergedKeys)
	}
	// The chain limit is the deepest nesting that leaves frames for the
	// innermost header at the depth the server calls package_manifest.
	if c.ServerManifestDepth != 10 || c.RecursionLimit != 1000 {
		t.Errorf("the vectors were made at depth %d with limit %d; maxHeaderChain was measured at 10 and 1000",
			c.ServerManifestDepth, c.RecursionLimit)
	}
}

func TestParsePathVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.ParsePath {
		path := unb64s(t, c.Path)
		got, ok := ParsePath(path)
		switch {
		case c.Result == nil && ok:
			t.Errorf("ParsePath(%q) = %+v, want None", path, got)
		case c.Result != nil && !ok:
			t.Errorf("ParsePath(%q) = None, want %+v", path, *c.Result)
		case c.Result != nil && (got.Name != c.Result.Name || got.Version != c.Result.Version):
			t.Errorf("ParsePath(%q) = %+v, want %+v", path, got, *c.Result)
		}
		name, ok := PackageName(path)
		if (c.PackageName == nil) == ok || ok && name != *c.PackageName {
			t.Errorf("PackageName(%q) = %q, %v, want %v", path, name, ok, c.PackageName)
		}
		if got, want := Unscoped(path), unb64s(t, c.Unscoped); got != want {
			t.Errorf("Unscoped(%q) = %q, want %q", path, got, want)
		}
	}
}

// keyJSON is the form tools/vectors/npm.py gives a key: numbers as decimal
// text, the release flag, and the pre-release parts.
func keyJSON(k Key) []any {
	numbers := []any{}
	for _, n := range k.Numbers {
		numbers = append(numbers, n.String())
	}
	parts := []any{}
	for _, p := range k.Parts {
		number := "0"
		if p.Number != nil {
			number = p.Number.String()
		}
		parts = append(parts, []any{float64(p.Kind), number, base64.StdEncoding.EncodeToString([]byte(p.Name))})
	}
	return []any{numbers, float64(k.Release), parts}
}

func TestVersionKeyVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.VersionKey {
		version := unb64s(t, c.Version)
		key, err := VersionKey(version)
		if c.Error != nil {
			sameError(t, "VersionKey("+version[:min(len(version), 40)]+")", err, c.Error)
			continue
		}
		if err != nil {
			t.Errorf("VersionKey(%.40q): %v", version, err)
			continue
		}
		if got := keyJSON(key); !reflect.DeepEqual(got, c.Key) {
			t.Errorf("VersionKey(%.40q) = %v, want %v", version, got, c.Key)
		}
	}
}

func TestVersionOrderVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.VersionOrder {
		versions := make([]string, len(c.Versions))
		for i, s := range c.Versions {
			versions[i] = unb64s(t, s)
		}
		before := append([]string(nil), versions...)
		sorted, err := SortVersions(versions)
		if c.SortedError != nil {
			sameError(t, "SortVersions", err, c.SortedError)
		} else if err != nil {
			t.Errorf("SortVersions(%.60q): %v", versions, err)
		} else {
			want := make([]string, len(c.Sorted))
			for i, s := range c.Sorted {
				want[i] = unb64s(t, s)
			}
			if !slices.Equal(sorted, want) {
				t.Errorf("SortVersions(%.60q) = %.60q, want %.60q", versions, sorted, want)
			}
		}
		if !slices.Equal(versions, before) {
			t.Errorf("SortVersions changed its input: %.60q", versions)
		}
		highest, err := HighestVersion(versions)
		if c.HighestError != nil {
			sameError(t, "HighestVersion", err, c.HighestError)
		} else if err != nil {
			t.Errorf("HighestVersion(%.60q): %v", versions, err)
		} else if want := unb64s(t, *c.Highest); highest != want {
			t.Errorf("HighestVersion(%.60q) = %.60q, want %.60q", versions, highest, want)
		}
	}
}

func TestIsoTimeVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.IsoTime {
		got, err := IsoTime(c.CreatedMS)
		if c.Error != nil {
			sameError(t, "IsoTime", err, c.Error)
			continue
		}
		if err != nil || got != *c.ISO {
			t.Errorf("IsoTime(%d) = %q, %v, want %q", c.CreatedMS, got, err, *c.ISO)
		}
	}
}

func TestDigitsWholeCodeSpace(t *testing.T) {
	v := load(t)
	want := map[rune]bool{}
	for _, run := range v.DigitRuns {
		for r := run[0]; r <= run[1]; r++ {
			want[r] = true
		}
	}
	if len(want) < 700 {
		t.Fatalf("only %d digits in the vectors", len(want))
	}
	bad := 0
	for r := rune(0); r <= 0x10FFFF; r++ {
		if isDigitRune(r) != want[r] {
			if bad < 10 {
				t.Errorf("isDigitRune(U+%04X) = %v, want %v", r, isDigitRune(r), want[r])
			}
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("%d code points differ", bad)
	}
}

func TestFileNameVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.FileName {
		url := unb64s(t, c.URL)
		got, ok := fileName(url)
		if c.Result == nil {
			if ok {
				t.Errorf("fileName(%q) = %q, want None", url, got)
			}
			continue
		}
		if want := unb64s(t, *c.Result); !ok || got != want {
			t.Errorf("fileName(%q) = %q, %v, want %q", url, got, ok, want)
		}
	}
}

func TestTarballPathsVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.TarballPaths {
		doc := v.document(t, c.Document)
		paths := TarballPaths(doc)
		out := pyjson.NewObject()
		for _, p := range paths {
			out.Set(p.Version, p.Path)
		}
		if got, want := dump(t, out), unb64(t, c.Result); !bytes.Equal(got, want) {
			t.Errorf("TarballPaths(%s):\n got %s\nwant %s", c.Document, got, want)
		}
	}
}

func TestSelectVersionsVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.SelectVersions {
		doc := v.document(t, c.Document)
		before := dump(t, doc)
		keep := make([]string, len(c.Keep))
		for i, k := range c.Keep {
			keep[i] = unb64s(t, k)
		}
		got, err := SelectVersions(doc, keep)
		label := "SelectVersions(" + c.Document + ")"
		if c.Error != nil {
			sameError(t, label, err, c.Error)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		sameBytes(t, label, got, c.Result)
		if !bytes.Equal(dump(t, doc), before) {
			t.Errorf("%s changed its input", label)
		}
	}
}

func TestMergeDocumentsVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.MergeDocuments {
		docs := make([]*pyjson.Object, len(c.Documents))
		before := make([][]byte, len(c.Documents))
		for i, name := range c.Documents {
			docs[i] = v.document(t, name)
			before[i] = dump(t, docs[i])
		}
		got := MergeDocuments(docs)
		sameBytes(t, "MergeDocuments", got, c.Result)
		for i := range docs {
			if !bytes.Equal(dump(t, docs[i]), before[i]) {
				t.Errorf("MergeDocuments(%v) changed input %d", c.Documents, i)
			}
		}
	}
}

func TestRewriteTarballsVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.RewriteTarballs {
		doc := v.document(t, c.Document)
		before := dump(t, doc)
		got, err := RewriteTarballs(doc, unb64s(t, c.Base))
		label := "RewriteTarballs(" + c.Document + ")"
		if c.Error != nil {
			sameError(t, label, err, c.Error)
		} else if err != nil {
			t.Errorf("%s: %v", label, err)
		} else {
			sameBytes(t, label, got, c.Result)
		}
		if !bytes.Equal(dump(t, doc), before) {
			t.Errorf("%s changed its input", label)
		}
	}
}

func TestVersionObjectVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.VersionObject {
		doc := v.document(t, c.Document)
		ref := unb64s(t, c.Reference)
		sameBytes(t, "VersionObject("+c.Document+", "+ref+")", VersionObject(doc, ref), c.Result)
	}
}

func TestPackageManifestVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.PackageManifest {
		got, err := PackageManifest(unb64(t, c.Data))
		label := "PackageManifest(" + c.Name + ")"
		if c.Error != nil {
			sameError(t, label, err, c.Error)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		sameBytes(t, label, got, c.Manifest)
	}
}

func TestDocumentFromTarballsVectors(t *testing.T) {
	v := load(t)
	for i, c := range v.DocumentFromTarballs {
		tarballs := make([]Tarball, len(c.Tarballs))
		for j, tb := range c.Tarballs {
			tarballs[j] = Tarball{FileName: tb.FileName, Data: unb64(t, tb.Data), CreatedMS: tb.CreatedMS}
		}
		got, err := DocumentFromTarballs(c.Name, tarballs)
		label := "DocumentFromTarballs#" + string(rune('0'+i%10)) + "(" + c.Name + ")"
		if c.Error != nil {
			sameError(t, label, err, c.Error)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		sameBytes(t, label, got, c.Result)
	}
}
