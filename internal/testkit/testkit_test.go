package testkit_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The vectors come from tools/vectors/testkit.py, which builds the same
// inputs with tests/fixtures.py of the reference.
type vectorFile struct {
	DefaultCreatedMS int64                        `json:"default_created_ms"`
	SkipCaseBlobs    []map[string]json.RawMessage `json:"skip_case_blobs"`
	Stores           []storeCase                  `json:"stores"`
	Trees            []treeCase                   `json:"trees"`
	Manifests        []manifestCase               `json:"manifests"`
	Properties       []propertiesCase             `json:"properties"`
}

type storeCase struct {
	Name       string                       `json:"name"`
	Specs      []map[string]json.RawMessage `json:"specs"`
	SkipCases  bool                         `json:"skip_cases"`
	Unparsable []string                     `json:"unparsable"`
	Error      *string                      `json:"error"`
	Locations  []string                     `json:"unparsable_locations"`
	Digest     string                       `json:"digest"`
	PerSpec    []struct {
		BlobUUID           string      `json:"blob_uuid"`
		SHA1               string      `json:"sha1"`
		BytesLocation      string      `json:"bytes_location"`
		PropertiesLocation string      `json:"properties_location"`
		Error              *string     `json:"error"`
		SpecProperties     [][2]string `json:"spec_properties"`
		Properties         string      `json:"properties_b64"`
	} `json:"per_spec"`
}

type treeCase struct {
	Name     string            `json:"name"`
	Missing  bool              `json:"missing"`
	FileRoot bool              `json:"file_root"`
	ViaLink  bool              `json:"via_link"`
	Files    map[string]string `json:"files"`
	Dirs     []string          `json:"dirs"`
	Links    [][2]string       `json:"links"`
	Digest   string            `json:"digest"`
	Maven    []struct {
		Args  [3]string         `json:"args"`
		Keys  []string          `json:"keys"`
		Files map[string]string `json:"files"`
	} `json:"maven"`
	Metadata []struct {
		Args []json.RawMessage `json:"args"`
		B64  string            `json:"b64"`
	} `json:"metadata"`
}

type manifestCase struct {
	Name  string              `json:"name"`
	Store *string             `json:"store"`
	Rows  [][]json.RawMessage `json:"rows"`
	B64   string              `json:"b64"`
}

type propertiesCase struct {
	Items     map[string]string `json:"items"`
	CreatedMS *int64            `json:"created_ms"`
	B64       string            `json:"b64"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/testkit.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Stores) == 0 || len(v.Trees) == 0 {
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

func field[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// specOf builds a spec as BlobSpec(repo, path, **fields) does in Python:
// a field that is absent keeps its default.
func specOf(t *testing.T, raw map[string]json.RawMessage) testkit.BlobSpec {
	t.Helper()
	s := testkit.NewBlobSpec(field[string](t, raw["repo"]), field[string](t, raw["path"]))
	for key, value := range raw {
		switch key {
		case "repo", "path":
		case "content_b64":
			s.Content = unb64(t, field[string](t, value))
		case "scheme":
			s.Scheme = field[string](t, value)
		case "created_ms":
			s.CreatedMS = field[int64](t, value)
		case "deleted":
			s.Deleted = field[bool](t, value)
		case "copy_only":
			s.CopyOnly = field[bool](t, value)
		case "no_bytes":
			s.NoBytes = field[bool](t, value)
		case "content_type":
			s.ContentType = field[*string](t, value)
		case "leading_slash":
			s.LeadingSlash = field[bool](t, value)
		default:
			t.Fatalf("unknown spec field %q", key)
		}
	}
	return s
}

func sortedPairs(m map[string]string) [][2]string {
	out := [][2]string{}
	for key, value := range m {
		out = append(out, [2]string{key, value})
	}
	slices.SortFunc(out, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return out
}

func sameSpec(a, b testkit.BlobSpec) bool {
	sameType := (a.ContentType == nil && b.ContentType == nil) ||
		(a.ContentType != nil && b.ContentType != nil && *a.ContentType == *b.ContentType)
	return a.Repo == b.Repo && a.Path == b.Path && bytes.Equal(a.Content, b.Content) && a.Scheme == b.Scheme &&
		a.CreatedMS == b.CreatedMS && a.Deleted == b.Deleted && a.CopyOnly == b.CopyOnly && a.NoBytes == b.NoBytes &&
		sameType && a.LeadingSlash == b.LeadingSlash
}

func TestDefaults(t *testing.T) {
	v := load(t)
	if testkit.DefaultCreatedMS != v.DefaultCreatedMS {
		t.Errorf("DefaultCreatedMS %d, want %d", testkit.DefaultCreatedMS, v.DefaultCreatedMS)
	}
	got := testkit.SkipCaseBlobs()
	if len(got) != len(v.SkipCaseBlobs) {
		t.Fatalf("%d skip case blobs, want %d", len(got), len(v.SkipCaseBlobs))
	}
	for i, raw := range v.SkipCaseBlobs {
		if want := specOf(t, raw); !sameSpec(got[i], want) {
			t.Errorf("skip case %d:\n got %+v\nwant %+v", i, got[i], want)
		}
	}
	s := testkit.NewBlobSpec("demo-releases", "a/b.jar")
	if string(s.Content) != "x" || s.Scheme != "date" || s.CreatedMS != testkit.DefaultCreatedMS || s.Deleted ||
		s.CopyOnly || s.NoBytes || s.ContentType == nil || *s.ContentType != "application/octet-stream" || !s.LeadingSlash {
		t.Errorf("defaults %+v", s)
	}
}

func TestStores(t *testing.T) {
	v := load(t)
	for _, c := range v.Stores {
		t.Run(c.Name, func(t *testing.T) {
			var specs []testkit.BlobSpec
			if c.SkipCases {
				specs = testkit.SkipCaseBlobs()
			} else {
				for _, raw := range c.Specs {
					specs = append(specs, specOf(t, raw))
				}
			}
			root := filepath.Join(t.TempDir(), "store")
			got, err := testkit.MakeBlobStore(root, specs)
			if c.Error != nil {
				if err == nil || err.Error() != *c.Error {
					t.Errorf("error %v, want %q", err, *c.Error)
				}
			} else {
				if err != nil || got != root {
					t.Fatalf("MakeBlobStore: %q %v", got, err)
				}
				locations := []string{}
				for _, name := range c.Unparsable {
					location, err := testkit.AddUnparsable(root, name)
					if err != nil {
						t.Fatal(err)
					}
					locations = append(locations, location)
				}
				if !slices.Equal(locations, c.Locations) {
					t.Errorf("unparsable locations %v, want %v", locations, c.Locations)
				}
			}
			digest, err := testkit.TreeDigest(root)
			if err != nil || digest != c.Digest {
				t.Errorf("digest %s %v, want %s", digest, err, c.Digest)
			}
			if len(c.PerSpec) != len(specs) {
				t.Fatalf("%d per spec entries for %d specs", len(c.PerSpec), len(specs))
			}
			for i, s := range specs {
				want := c.PerSpec[i]
				if got := testkit.BlobUUID(s); got != want.BlobUUID {
					t.Errorf("spec %d: uuid %s, want %s", i, got, want.BlobUUID)
				}
				if got := testkit.SHA1Hex(s.Content); got != want.SHA1 {
					t.Errorf("spec %d: sha1 %s, want %s", i, got, want.SHA1)
				}
				if want.Error != nil {
					func() {
						defer func() {
							if r := recover(); r == nil || fmt.Sprint(r) != *want.Error {
								t.Errorf("spec %d: panic %v, want %q", i, r, *want.Error)
							}
						}()
						testkit.BytesLocation(s)
					}()
					continue
				}
				if got := testkit.BytesLocation(s); got != want.BytesLocation {
					t.Errorf("spec %d: bytes location %s, want %s", i, got, want.BytesLocation)
				}
				if got := testkit.PropertiesLocation(s); got != want.PropertiesLocation {
					t.Errorf("spec %d: properties location %s, want %s", i, got, want.PropertiesLocation)
				}
				props := testkit.SpecProperties(s)
				if got := sortedPairs(props); !slices.Equal(got, want.SpecProperties) {
					t.Errorf("spec %d: properties\n got %q\nwant %q", i, got, want.SpecProperties)
				}
				if got := testkit.EncodeProperties(props, s.CreatedMS); !bytes.Equal(got, unb64(t, want.Properties)) {
					t.Errorf("spec %d: encoded\n got %q\nwant %q", i, got, unb64(t, want.Properties))
				}
			}
		})
	}
}

func TestTrees(t *testing.T) {
	v := load(t)
	for _, c := range v.Trees {
		t.Run(c.Name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "tree")
			if c.Missing {
				digest, err := testkit.TreeDigest(filepath.Join(root, "does-not-exist"))
				if err != nil || digest != c.Digest {
					t.Errorf("digest %s %v, want %s", digest, err, c.Digest)
				}
				return
			}
			if c.FileRoot {
				if err := os.MkdirAll(root, 0o777); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(root, "plain-file")
				if err := os.WriteFile(file, []byte("a file, not a directory"), 0o666); err != nil {
					t.Fatal(err)
				}
				digest, err := testkit.TreeDigest(file)
				if err != nil || digest != c.Digest {
					t.Errorf("digest %s %v, want %s", digest, err, c.Digest)
				}
				return
			}
			files := map[string][]byte{}
			for key, value := range c.Files {
				files[key] = unb64(t, value)
			}
			for _, m := range c.Maven {
				made := testkit.MavenArtifact(m.Args[0], m.Args[1], m.Args[2])
				keys := make([]string, 0, len(made))
				for key := range made {
					keys = append(keys, key)
				}
				slices.Sort(keys)
				wantKeys := slices.Sorted(slices.Values(m.Keys))
				if !slices.Equal(keys, wantKeys) {
					t.Errorf("maven %v: keys %v, want %v", m.Args, keys, wantKeys)
				}
				for key, value := range m.Files {
					if want := unb64(t, value); !bytes.Equal(made[key], want) {
						t.Errorf("maven %v: %s\n got %x\nwant %x", m.Args, key, made[key], want)
					}
				}
				for key, value := range made {
					files["demo-maven/"+key] = value
				}
			}
			for _, m := range c.Metadata {
				group, artifact := field[string](t, m.Args[0]), field[string](t, m.Args[1])
				versions, last := field[[]string](t, m.Args[2]), field[string](t, m.Args[3])
				data := testkit.MetadataXML(group, artifact, versions, last)
				if want := unb64(t, m.B64); !bytes.Equal(data, want) {
					t.Errorf("metadata %s:\n got %q\nwant %q", artifact, data, want)
				}
				files["demo-maven/"+strings.ReplaceAll(group, ".", "/")+"/"+artifact+"/maven-metadata.xml"] = data
			}
			if c.ViaLink {
				real := filepath.Join(root, "real")
				if got, err := testkit.MakeTree(real, files); err != nil || got != real {
					t.Fatalf("MakeTree: %q %v", got, err)
				}
				if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				// os.walk follows a root that is a symlink.
				digest, err := testkit.TreeDigest(filepath.Join(root, "link"))
				if err != nil || digest != c.Digest {
					t.Errorf("digest %s %v, want %s", digest, err, c.Digest)
				}
				return
			}
			if got, err := testkit.MakeTree(root, files); err != nil || got != root {
				t.Fatalf("MakeTree: %q %v", got, err)
			}
			for _, dir := range c.Dirs {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o777); err != nil {
					t.Fatal(err)
				}
			}
			for _, link := range c.Links {
				if err := os.Symlink(link[1], filepath.Join(root, link[0])); err != nil {
					t.Fatal(err)
				}
			}
			digest, err := testkit.TreeDigest(root)
			if err != nil || digest != c.Digest {
				t.Errorf("digest %s %v, want %s", digest, err, c.Digest)
			}
		})
	}
}

func TestManifests(t *testing.T) {
	v := load(t)
	for _, c := range v.Manifests {
		rows := []testkit.ManifestRow{}
		for _, raw := range c.Rows {
			row := testkit.ManifestRow{
				Repo:        field[string](t, raw[0]),
				Path:        field[string](t, raw[1]),
				Size:        field[int64](t, raw[2]),
				CreatedMS:   field[int64](t, raw[3]),
				SHA1:        field[*string](t, raw[5]),
				ContentType: field[*string](t, raw[6]),
			}
			// Python writes "1" for any true value, also 1 and True.
			deleted := field[any](t, raw[4])
			row.Deleted = deleted == true || (deleted != false && deleted != 0.0)
			rows = append(rows, row)
		}
		store := testkit.DefaultManifestStore
		if c.Store != nil {
			store = *c.Store
		}
		if got, want := testkit.BlobManifest(rows, store), unb64(t, c.B64); !bytes.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, want)
		}
	}
}

func TestEncodeProperties(t *testing.T) {
	v := load(t)
	for i, c := range v.Properties {
		created := testkit.DefaultCreatedMS
		if c.CreatedMS != nil {
			created = *c.CreatedMS
		}
		if got, want := testkit.EncodeProperties(c.Items, created), unb64(t, c.B64); !bytes.Equal(got, want) {
			t.Errorf("case %d:\n got %q\nwant %q", i, got, want)
		}
	}
}

func TestMakeStoreLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	live := testkit.NewBlobSpec("demo-releases", "com/example/live/1.0/live-1.0.jar")
	live.Content = []byte("live bytes")
	if _, err := testkit.MakeBlobStore(root, []testkit.BlobSpec{live}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"metadata.properties", "demo-node-metrics.properties", testkit.BytesLocation(live),
		testkit.PropertiesLocation(live), "reconciliation/2023-11-14"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(testkit.BytesLocation(live))))
	if err != nil || string(data) != "live bytes" {
		t.Errorf("bytes file %q %v", data, err)
	}
}
