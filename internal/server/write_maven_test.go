package server

import (
	"crypto/md5" // #nosec G501 -- the checksum file of a test deploy.
	"encoding/hex"
	"strings"
	"testing"
)

// A maven2 deploy: the files and checksum files answer 201 and are served
// as sent; a checksum file that disagrees with its file is stored and
// named in a warning (W7); metadata passes ALLOW_ONCE again.
func TestMavenDeployAndTheChecksumWarning(t *testing.T) {
	records := captureLog(t)
	w := startIngest(t, map[string]map[string]any{"demo-releases": {"storage": map[string]any{"writePolicy": "ALLOW_ONCE"},
		"maven": map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}}})
	base := "/repository/demo-releases/com/example/demo-lib/1.0.0/demo-lib-1.0.0"
	jar := []byte("PK\x03\x04 a jar")
	sum := md5.Sum(jar) // #nosec G401 -- see the imports.
	for _, c := range []struct {
		target string
		body   []byte
		status int
	}{
		{base + ".jar", jar, 201},
		{base + ".jar.sha1", []byte(sha1Hex(jar)), 201},
		{base + ".jar.md5", []byte(strings.Repeat("0", 32)), 201},
		{base + ".jar.sha256", []byte(strings.Repeat("1", 64) + "  demo-lib-1.0.0.jar\n"), 201},
		{base + ".jar", jar, 400},
		{base + ".jar.md5", []byte(hex.EncodeToString(sum[:])), 400},
		{"/repository/demo-releases/com/example/demo-lib/maven-metadata.xml", []byte("<metadata/>\n"), 201},
		{"/repository/demo-releases/com/example/demo-lib/maven-metadata.xml", []byte("<metadata></metadata>\n"), 201},
		{"/repository/demo-releases/com/example/demo-lib/1.1.0-SNAPSHOT/demo-lib-1.1.0-20261118.101530-1.jar", jar, 400},
		{"/repository/demo-releases/docs/readme.txt", []byte("text"), 400},
	} {
		r := do(t, w.addr(), "PUT", c.target, c.body, "basic", asAdmin)
		if r.status != c.status {
			t.Fatalf("PUT %s: %d %q, want %d", c.target, r.status, r.body, c.status)
		}
	}
	if r := do(t, w.addr(), "GET", base+".jar.md5", nil, "basic", asAdmin); string(r.body) != strings.Repeat("0", 32) {
		t.Fatalf("the checksum file is not stored as sent: %q", r.body)
	}
	log := strings.Join(records.all(), "\n")
	for _, suffix := range []string{".jar.md5", ".jar.sha256"} {
		want := "the checksum file com/example/demo-lib/1.0.0/demo-lib-1.0.0" + suffix + " of repository demo-releases does not hold"
		if !strings.Contains(log, "WARN "+serverLogger+": "+want) {
			t.Errorf("no warning for %s in %q", suffix, log)
		}
	}
	if strings.Contains(log, "demo-lib-1.0.0.jar.sha1 of repository") {
		t.Errorf("a warning for the right SHA-1: %q", log)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}

// DELETE of a maven2 path marks its blob; other methods answer 405.
func TestMavenDeleteByPath(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
	target := "/repository/demo-releases/com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom"
	if r := do(t, w.addr(), "PUT", target, []byte("<project/>"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "DELETE", target, nil, "basic", asAdmin); r.status != 204 {
		t.Fatalf("DELETE: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "GET", target, nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("GET: %d", r.status)
	}
	if r := do(t, w.addr(), "PATCH", target, []byte("x"), "basic", asAdmin); r.status != 405 || r.header.Get("Allow") != fileWriteMethods {
		t.Fatalf("PATCH: %d %v", r.status, r.header)
	}
	counts, _ := w.scan()
	if counts.Live != 0 || counts.Deleted != 1 {
		t.Fatalf("%+v", counts)
	}
}
