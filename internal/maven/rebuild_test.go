package maven

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type rebuildCase struct {
	Name  string `json:"name"`
	Scope struct {
		Group    string `json:"group"`
		Artifact string `json:"artifact"`
		Version  string `json:"version"`
	} `json:"scope"`
	Files []struct {
		Path    string `json:"path"`
		Created string `json:"created"`
		Text    string `json:"text"`
		Base64  string `json:"base64"`
	} `json:"files"`
	Documents map[string]string `json:"documents"`
	Stale     []string          `json:"stale"`
}

// memory is a Content of bytes in memory.
type memory struct{ *bytes.Reader }

func (memory) Close() error { return nil }

// sourceOf is the Source of the files of a case: each with its time and its
// bytes, empty when the case gives none.
func sourceOf(t *testing.T, c rebuildCase) Source {
	t.Helper()
	contents := map[string][]byte{}
	var files []File
	for _, f := range c.Files {
		at, err := time.Parse(time.RFC3339Nano, f.Created)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, File{Path: f.Path, CreatedMS: at.UnixMilli()})
		data := []byte(f.Text)
		if f.Base64 != "" {
			if data, err = base64.StdEncoding.DecodeString(f.Base64); err != nil {
				t.Fatal(err)
			}
		}
		contents[f.Path] = data
	}
	return Source{Files: files, Open: func(path string) (Content, int64, error) {
		data, ok := contents[path]
		if !ok {
			return nil, 0, nil
		}
		return memory{bytes.NewReader(data)}, int64(len(data)), nil
	}}
}

// The metadata of the artifact, snapshot version and plugin group levels
// written from invented files, byte for byte as the golden documents of
// testdata/vectors/maven_rebuild.json, with their checksum files; the stale
// metadata of a path without files; the scope of a group and a version.
func TestRebuildVectors(t *testing.T) {
	data, err := os.ReadFile("../../testdata/vectors/maven_rebuild.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []rebuildCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) < 6 {
		t.Fatalf("%d cases", len(vectors.Cases))
	}
	for _, c := range vectors.Cases {
		t.Run(c.Name, func(t *testing.T) {
			rebuilt, err := Rebuild(sourceOf(t, c), Scope{GroupID: c.Scope.Group, ArtifactID: c.Scope.Artifact, BaseVersion: c.Scope.Version})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			var paths []string
			for _, d := range rebuilt.Documents {
				got[d.Path] = string(d.Data)
				paths = append(paths, d.Path)
			}
			if !slices.IsSorted(paths) {
				t.Errorf("documents not in path order: %q", paths)
			}
			for path, want := range c.Documents {
				if got[path] != want {
					t.Errorf("%s:\n got %q\nwant %q", path, got[path], want)
				}
			}
			for path := range got {
				if _, ok := c.Documents[path]; !ok {
					t.Errorf("a document %s the case does not expect", path)
				}
			}
			if !slices.Equal(rebuilt.Stale, c.Stale) && len(rebuilt.Stale)+len(c.Stale) > 0 {
				t.Errorf("stale %q, want %q", rebuilt.Stale, c.Stale)
			}
		})
	}
}

// A scope without its group, or a version without its artifact, is refused.
func TestRebuildScopeNeedsItsParts(t *testing.T) {
	for _, s := range []Scope{{ArtifactID: "demo-lib"}, {GroupID: "com.example", BaseVersion: "1.0"}, {GroupID: "com..example"},
		{GroupID: "com.example", ArtifactID: "a/b"}} {
		if _, err := Rebuild(Source{}, s); err == nil {
			t.Errorf("%+v is taken", s)
		}
	}
	if dir, err := (Scope{GroupID: "com.example", ArtifactID: "demo-lib", BaseVersion: "1.0"}).Dir(); err != nil || dir != "com/example/demo-lib/1.0" {
		t.Fatalf("%q %v", dir, err)
	}
}

// The rebuild reads at most a bounded part of a POM and of the plugin
// descriptor; a POM above the bound counts as no plugin.
func TestRebuildReadsPOMsBounded(t *testing.T) {
	huge := "<project><packaging>maven-plugin</packaging>" + strings.Repeat(" ", pomLimit) + "</project>"
	c := rebuildCase{}
	c.Files = append(c.Files, struct {
		Path    string `json:"path"`
		Created string `json:"created"`
		Text    string `json:"text"`
		Base64  string `json:"base64"`
	}{Path: "com/example/demo-maven-plugin/1.0/demo-maven-plugin-1.0.pom", Created: "2026-11-18T10:00:00Z", Text: huge})
	rebuilt, err := Rebuild(sourceOf(t, c), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rebuilt.Documents {
		if d.Path == "com/example/maven-metadata.xml" {
			t.Fatalf("a plugin from a POM above the bound: %q", d.Data)
		}
	}
}
