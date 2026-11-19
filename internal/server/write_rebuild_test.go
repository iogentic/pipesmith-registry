package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/maven"
)

// The rebuild writes the metadata of the files a repository holds, a
// second run writes no blob, a deleted version is no longer listed, and
// metadata without files below it is removed.
func TestRebuildMavenMetadata(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": {"storage": map[string]any{"writePolicy": "ALLOW_ONCE"},
		"maven": map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}}})
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if _, err := w.ingest("demo-releases", "com/example/demo-lib/"+v+"/demo-lib-"+v+".pom", "<project/>"); err != nil {
			t.Fatal(err)
		}
		w.clock.add(time.Minute)
	}
	if _, err := w.ingest("demo-releases", "com/example/demo-gone/maven-metadata.xml", "<metadata/>\n"); err != nil {
		t.Fatal(err)
	}
	system := subject("system", "")
	report, err := w.srv.RebuildMavenMetadata("demo-releases", maven.Scope{}, system, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	meta := "com/example/demo-lib/maven-metadata.xml"
	if !slices.Equal(report.Written, []string{meta, meta + ".md5", meta + ".sha1"}) || len(report.Unchanged) != 0 ||
		!slices.Equal(report.Removed, []string{"com/example/demo-gone/maven-metadata.xml"}) {
		t.Fatalf("%+v", report)
	}
	r := do(t, w.addr(), "GET", "/repository/demo-releases/"+meta, nil, "basic", asAdmin)
	if r.status != 200 || !strings.Contains(string(r.body), "<version>1.0.0</version>\n      <version>1.1.0</version>") ||
		!strings.Contains(string(r.body), "<lastUpdated>20261118101630</lastUpdated>") {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if c := do(t, w.addr(), "GET", "/repository/demo-releases/"+meta+".sha1", nil, "basic", asAdmin); string(c.body) != sha1Hex(r.body) {
		t.Fatalf("%q", c.body)
	}
	row, _ := w.srv.rp.index.Lookup("demo-releases", meta)
	if props := w.properties(row.Location); props["@BlobStore.created-by"] != "system" || props["@BlobStore.created-by-ip"] != "127.0.0.1" {
		t.Fatalf("%q", props)
	}
	before, _ := w.scan()
	again, err := w.srv.RebuildMavenMetadata("demo-releases", maven.Scope{}, system, "127.0.0.1")
	if err != nil || len(again.Written) != 0 || len(again.Unchanged) != 3 || len(again.Removed) != 0 {
		t.Fatalf("second run %+v %v", again, err)
	}
	if after, _ := w.scan(); after != before {
		t.Fatalf("the second run wrote: %+v, before %+v", after, before)
	}
	if _, err := w.srv.erase(w.repo("demo-releases"), "com/example/demo-lib/1.1.0/demo-lib-1.1.0.pom"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.RebuildMavenMetadata("demo-releases", maven.Scope{GroupID: "com.example", ArtifactID: "demo-lib"}, system, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	r = do(t, w.addr(), "GET", "/repository/demo-releases/"+meta, nil, "basic", asAdmin)
	if strings.Contains(string(r.body), "1.1.0") || !strings.Contains(string(r.body), "<version>1.0.0</version>") {
		t.Fatalf("after the delete of 1.1.0: %q", r.body)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}

// The rebuild takes a hosted maven2 repository of this server only.
func TestRebuildRefusals(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW"), "demo-raw": storage("ALLOW")})
	for _, name := range []string{"demo-raw", "missing"} {
		if _, err := w.srv.RebuildMavenMetadata(name, maven.Scope{}, nil, ""); err == nil {
			t.Errorf("%s is rebuilt", name)
		}
	}
	if _, err := w.srv.RebuildMavenMetadata("demo-releases", maven.Scope{ArtifactID: "demo-lib"}, nil, ""); err == nil {
		t.Error("an artifact without its group")
	}
	off := startWriting(t, newFakeSecurity(), false)
	if _, err := off.srv.RebuildMavenMetadata("demo-maven", maven.Scope{}, nil, ""); err == nil {
		t.Error("a rebuild with writes off")
	}
}
