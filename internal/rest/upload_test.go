package rest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The field names of the public documentation of the old product, page
// Components API, section Upload Component (read 2026-11-18), with assetN
// for N from 1 to 3 as the REST description of the old product names them.
func TestUploadFieldNames(t *testing.T) {
	for format, want := range map[string][]string{
		"maven2": {"maven2.groupId", "maven2.artifactId", "maven2.version", "maven2.generate-pom", "maven2.packaging",
			"maven2.asset1", "maven2.asset1.classifier", "maven2.asset1.extension",
			"maven2.asset2", "maven2.asset2.classifier", "maven2.asset2.extension",
			"maven2.asset3", "maven2.asset3.classifier", "maven2.asset3.extension"},
		"raw": {"raw.directory", "raw.asset1", "raw.asset1.filename", "raw.asset2", "raw.asset2.filename", "raw.asset3", "raw.asset3.filename"},
		"npm": {"npm.asset"},
	} {
		got := UploadFieldNames(format)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v", format, got)
		}
		for _, name := range got {
			f, ok := ParseUploadField(format, name)
			if !ok || UploadFieldName(format, f) != name {
				t.Errorf("%s does not round trip: %+v %v", name, f, ok)
			}
		}
	}
	for _, c := range []struct{ format, name string }{
		{"raw", "raw.asset0"}, {"raw", "raw.asset4"}, {"raw", "raw.asset01"}, {"raw", "raw.asset"}, {"raw", "raw.asset1.extension"},
		{"raw", "maven2.groupId"}, {"raw", "raw.colour"}, {"npm", "npm.asset1"}, {"npm", "npm.asset.filename"}, {"docker", "docker.asset"},
		{"maven2", "maven2."}, {"maven2", ""},
	} {
		if f, ok := ParseUploadField(c.format, c.name); ok {
			t.Errorf("%s of %s is taken: %+v", c.name, c.format, f)
		}
	}
	if UploadFieldNames("docker") != nil || Uploadable("docker") || !Uploadable("raw") {
		t.Fatal("docker takes an upload")
	}
}

// The field list in the shape UploadDefinitionXO: every key, the group of a
// component field, null for an asset field.
func TestUploadSpecShape(t *testing.T) {
	data, err := json.Marshal(UploadSpec("raw").Body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"format":"raw","multipleUpload":true,"componentFields":[{"name":"directory","type":"STRING",` +
		`"description":"The folder of the files in the repository","optional":false,"group":"Folder"}],"assetFields":[{"name":"filename",` +
		`"type":"STRING","description":"The name of the file in the folder","optional":false,"group":null}]}`
	if string(data) != want {
		t.Fatalf("%s", data)
	}
	if UploadSpec("docker").Status != 404 || UploadSpecs().Status != 200 {
		t.Fatal("the answers")
	}
}

func TestRawPaths(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "docs": "docs", "/docs/up/": "docs/up", "docs//up": "docs/up"} {
		if got, problem := RawDirectory(in); got != want || problem != "" {
			t.Errorf("%q: %q %q", in, got, problem)
		}
	}
	for _, bad := range []string{"docs/..", "../docs", "./docs", `docs\up`, "docs/\x01"} {
		if _, problem := RawDirectory(bad); problem == "" {
			t.Errorf("%q is taken", bad)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b.txt", `a\b.txt`, "../a.txt", "a\x7f.txt", "\xff"} {
		if RawFileName(bad) == "" {
			t.Errorf("%q is taken", bad)
		}
	}
	if RawFileName("a b.txt") != "" || RawFileName("..hidden") != "" {
		t.Fatal("good names are refused")
	}
}

func TestMavenUploadPaths(t *testing.T) {
	c := MavenCoordinates{GroupID: "com.example.deep", ArtifactID: "demo-lib", Version: "1.2.0"}
	if got := MavenPath(c, "sources", "jar"); got != "com/example/deep/demo-lib/1.2.0/demo-lib-1.2.0-sources.jar" {
		t.Fatal(got)
	}
	if got := MavenPath(c, "", "tar.gz"); got != "com/example/deep/demo-lib/1.2.0/demo-lib-1.2.0.tar.gz" {
		t.Fatal(got)
	}
	if errs := c.Check("maven2."); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	for _, bad := range []MavenCoordinates{
		{GroupID: "com/example", ArtifactID: "a", Version: "1"}, {GroupID: "com..example", ArtifactID: "a", Version: "1"},
		{GroupID: "com.example", ArtifactID: "..", Version: "1"}, {GroupID: "com.example", ArtifactID: "a", Version: "1/2"},
		{GroupID: "com.example", ArtifactID: "a"}, {GroupID: "com.example", ArtifactID: "a", Version: "1", Packaging: "a/b"},
	} {
		if errs := bad.Check("maven2."); len(errs) == 0 {
			t.Errorf("%+v is taken", bad)
		}
	}
	for _, c := range []struct{ name, value string }{{"extension", ".jar"}, {"extension", "jar."}, {"classifier", "-x"}, {"classifier", "a/b"}} {
		if CheckAssetPart(c.name, c.value) == "" {
			t.Errorf("%s %q is taken", c.name, c.value)
		}
	}
}

// The POM made from coordinates, and the coordinates read back from it and
// from a POM whose parent gives group and version.
func TestGeneratedPOM(t *testing.T) {
	c := MavenCoordinates{GroupID: "com.example", ArtifactID: "demo<lib>", Version: "1.0&2", Packaging: "war"}
	pom := GeneratedPOM(c)
	if !strings.Contains(string(pom), "<artifactId>demo&lt;lib&gt;</artifactId>") || !strings.Contains(string(pom), "<packaging>war</packaging>") {
		t.Fatalf("%s", pom)
	}
	back, err := ReadPOM(strings.NewReader(string(pom)))
	if err != nil || back != c {
		t.Fatalf("%+v %v", back, err)
	}
	if strings.Contains(string(GeneratedPOM(MavenCoordinates{GroupID: "g", ArtifactID: "a", Version: "1"})), "packaging") {
		t.Fatal("a packaging without one")
	}
	parent := `<project><parent><groupId>com.example</groupId><version>3.0</version></parent><artifactId>child</artifactId></project>`
	if got, err := ReadPOM(strings.NewReader(parent)); err != nil || got != (MavenCoordinates{GroupID: "com.example", ArtifactID: "child", Version: "3.0"}) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ReadPOM(strings.NewReader("no xml")); err == nil {
		t.Fatal("no XML is taken")
	}
	if _, err := ReadPOM(strings.NewReader("<project>" + strings.Repeat(" ", MaxPOMBytes) + "</project>")); err == nil {
		t.Fatal("a POM above the bound is taken")
	}
}

// A component id round trips: DecodeComponentID gives back the repository
// of the id the search makes; an asset id and texts that are no id do not
// decode.
func TestComponentIDs(t *testing.T) {
	id, ok := ComponentID("demo-maven", "maven2", "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar")
	if !ok {
		t.Fatal("no component")
	}
	repo, key, valid := DecodeComponentID(id)
	if !valid || repo != "demo-maven" || len(key) != 16 {
		t.Fatalf("%q %x %v", repo, key, valid)
	}
	if again, _ := ComponentID("demo-maven", "maven2", "com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom"); again != id {
		t.Fatal("two files of one version are two components")
	}
	if other, _ := ComponentID("demo-maven", "maven2", "com/example/demo-lib/1.1.0/demo-lib-1.1.0.pom"); other == id {
		t.Fatal("two versions are one component")
	}
	for _, bad := range []string{"", "!", AssetID("demo-maven", "a/b.jar"), id + "A", id[:len(id)-1]} {
		if _, _, ok := DecodeComponentID(bad); ok {
			t.Errorf("%q decodes", bad)
		}
	}
	if r, ok := IDRepository(AssetID("demo-raw", "docs/a.txt")); !ok || r != "demo-raw" {
		t.Fatalf("%q %v", r, ok)
	}
	if r, ok := IDRepository(id); !ok || r != "demo-maven" {
		t.Fatalf("%q %v", r, ok)
	}
	if _, ok := IDRepository("x1"); ok {
		t.Fatal("x1 names a repository")
	}
	if _, ok := ComponentID("demo-maven", "maven2", "com/example/demo-lib/maven-metadata.xml"); ok {
		t.Fatal("metadata is a component")
	}
}
