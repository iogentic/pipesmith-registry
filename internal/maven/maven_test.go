package maven_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The vectors come from tools/vectors/maven.py, which runs maven.py of the
// reference: split_checksum, is_metadata_path, content_type_for,
// coordinates, compare_versions and merge_metadata.

type mergeCase struct {
	Name        string   `json:"name"`
	Documents   []string `json:"documents"`
	Value       *string  `json:"value"`
	ValueSHA256 string   `json:"value_sha256"`
	ValueLength int      `json:"value_length"`
	Error       string   `json:"error"`
	Message     string   `json:"message"`
}

type coordinatesValue struct {
	GroupID    string  `json:"group_id"`
	ArtifactID string  `json:"artifact_id"`
	Version    string  `json:"version"`
	Classifier *string `json:"classifier"`
	Extension  string  `json:"extension"`
}

type vectorFile struct {
	Python    string `json:"python"`
	Expat     string `json:"expat"`
	Constants struct {
		ChecksumSuffixes   []string `json:"checksum_suffixes"`
		MetadataName       string   `json:"metadata_name"`
		XMLDeclaration     string   `json:"xml_declaration"`
		DefaultContentType string   `json:"default_content_type"`
	} `json:"constants"`
	SplitChecksum []struct {
		Path  string     `json:"path"`
		Value [2]*string `json:"value"`
	} `json:"split_checksum"`
	IsMetadataPath []struct {
		Path  string `json:"path"`
		Value bool   `json:"value"`
	} `json:"is_metadata_path"`
	ContentTypeFor []struct {
		Path  string `json:"path"`
		Value string `json:"value"`
	} `json:"content_type_for"`
	Coordinates []struct {
		Path  string            `json:"path"`
		Value *coordinatesValue `json:"value"`
	} `json:"coordinates"`
	Versions struct {
		Compare []struct {
			A       string `json:"a"`
			B       string `json:"b"`
			Value   *int   `json:"value"`
			Error   string `json:"error"`
			Message string `json:"message"`
		} `json:"compare"`
	} `json:"versions"`
	Merge []mergeCase `json:"merge"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/maven.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Merge) < 300 || len(v.Versions.Compare) < 500 || len(v.Coordinates) < 30 {
		t.Fatalf("%d merges, %d comparisons, %d coordinates", len(v.Merge), len(v.Versions.Compare), len(v.Coordinates))
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

func pyClass(err error) string {
	var pyErr *pyjson.Error
	if errors.As(err, &pyErr) {
		return pyErr.Class
	}
	return "not a Python error"
}

func TestConstants(t *testing.T) {
	v := load(t)
	if !reflect.DeepEqual(maven.ChecksumSuffixes(), v.Constants.ChecksumSuffixes) {
		t.Errorf("checksum suffixes %v, want %v", maven.ChecksumSuffixes(), v.Constants.ChecksumSuffixes)
	}
	if maven.MetadataName != v.Constants.MetadataName || maven.XMLDeclaration != v.Constants.XMLDeclaration ||
		maven.DefaultContentType != v.Constants.DefaultContentType {
		t.Errorf("constants %q %q %q", maven.MetadataName, maven.XMLDeclaration, maven.DefaultContentType)
	}
}

func TestSplitChecksum(t *testing.T) {
	for _, c := range load(t).SplitChecksum {
		base, suffix := maven.SplitChecksum(c.Path)
		want := ""
		if c.Value[1] != nil {
			want = *c.Value[1]
		}
		if base != *c.Value[0] || suffix != want {
			t.Errorf("SplitChecksum(%q) = %q, %q; want %q, %q", c.Path, base, suffix, *c.Value[0], want)
		}
	}
}

func TestIsMetadataPath(t *testing.T) {
	for _, c := range load(t).IsMetadataPath {
		if got := maven.IsMetadataPath(c.Path); got != c.Value {
			t.Errorf("IsMetadataPath(%q) = %v", c.Path, got)
		}
	}
}

func TestContentTypeFor(t *testing.T) {
	for _, c := range load(t).ContentTypeFor {
		if got := maven.ContentTypeFor(c.Path); got != c.Value {
			t.Errorf("ContentTypeFor(%q) = %q, want %q", c.Path, got, c.Value)
		}
	}
}

func TestCoordinates(t *testing.T) {
	seen := 0
	for _, c := range load(t).Coordinates {
		got := maven.CoordinatesOf(c.Path)
		if (got == nil) != (c.Value == nil) {
			t.Errorf("CoordinatesOf(%q) = %+v, want %+v", c.Path, got, c.Value)
			continue
		}
		if got == nil {
			continue
		}
		seen++
		want := maven.Coordinates{GroupID: c.Value.GroupID, ArtifactID: c.Value.ArtifactID, Version: c.Value.Version,
			Classifier: c.Value.Classifier, Extension: c.Value.Extension}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("CoordinatesOf(%q) = %+v, want %+v", c.Path, *got, want)
		}
	}
	if seen < 10 {
		t.Fatalf("only %d paths with coordinates", seen)
	}
}

func TestCompareVersions(t *testing.T) {
	errorsSeen := 0
	for _, c := range load(t).Versions.Compare {
		got, err := maven.CompareVersions(c.A, c.B)
		if c.Error != "" {
			errorsSeen++
			if err == nil || pyClass(err) != c.Error || err.Error() != c.Message {
				t.Errorf("CompareVersions(%.40q, %.40q) = %d, %v; want %s %q", c.A, c.B, got, err, c.Error, c.Message)
			}
			continue
		}
		if err != nil || got != *c.Value {
			t.Errorf("CompareVersions(%.40q, %.40q) = %d, %v; want %d", c.A, c.B, got, err, *c.Value)
		}
	}
	if errorsSeen < 4 {
		t.Fatalf("only %d comparisons that raise", errorsSeen)
	}
}

func checkMerge(t *testing.T, c mergeCase) {
	t.Helper()
	var documents [][]byte
	for _, d := range c.Documents {
		documents = append(documents, unb64(t, d))
	}
	got, err := maven.MergeMetadata(documents)
	switch {
	case c.Error != "":
		if err == nil || pyClass(err) != c.Error || err.Error() != c.Message {
			t.Errorf("%s: got %.200q, %v; want %s %q", c.Name, got, err, c.Error, c.Message)
		}
	case err != nil:
		t.Errorf("%s: error %s %v", c.Name, pyClass(err), err)
	case c.ValueSHA256 != "":
		sum := sha256.Sum256(got)
		if hex.EncodeToString(sum[:]) != c.ValueSHA256 || len(got) != c.ValueLength {
			t.Errorf("%s: %d bytes with another digest, want %d", c.Name, len(got), c.ValueLength)
		}
	case c.Value == nil:
		if got != nil {
			t.Errorf("%s: got %.300q, want None", c.Name, got)
		}
	default:
		want := unb64(t, *c.Value)
		if !bytes.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, want)
		}
	}
}

func TestMergeMetadata(t *testing.T) {
	v := load(t)
	none, raised := 0, 0
	for _, c := range v.Merge {
		if c.Error != "" {
			raised++
		} else if c.Value == nil && c.ValueSHA256 == "" {
			none++
		}
		checkMerge(t, c)
	}
	if none < 100 || raised < 10 || len(v.Merge)-none-raised < 150 {
		t.Fatalf("%d merges give None, %d raise, of %d", none, raised, len(v.Merge))
	}
}

func TestMergeIsDeterministic(t *testing.T) {
	one := []byte("<metadata><groupId>com.example</groupId><versioning><versions><version>1.2</version>" +
		"<version>1.0</version></versions></versioning></metadata>")
	first, err := maven.MergeMetadata([][]byte{one, one})
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	second, _ := maven.MergeMetadata([][]byte{one, one})
	if !bytes.Equal(first, second) || !bytes.HasPrefix(first, []byte(maven.XMLDeclaration)) {
		t.Fatalf("%q then %q", first, second)
	}
	if got, err := maven.MergeMetadata(nil); got != nil || err != nil {
		t.Fatalf("no documents: %q, %v", got, err)
	}
}

// TestMergeFuzz compares the merge of many mutated documents with the one of
// Python. The file comes from tools/vectors/maven.py --fuzz COUNT OUT and is
// not committed; without PSR_MAVEN_FUZZ the test does nothing.
func TestMergeFuzz(t *testing.T) {
	path := os.Getenv("PSR_MAVEN_FUZZ")
	if path == "" {
		t.Skip("PSR_MAVEN_FUZZ names no fuzz file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Merge []mergeCase `json:"merge"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Merge) == 0 {
		t.Fatal("no merges in the fuzz file")
	}
	for _, c := range v.Merge {
		checkMerge(t, c)
	}
	t.Logf("%d fuzz merges", len(v.Merge))
}
