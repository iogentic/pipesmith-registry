package server

import (
	"strings"
	"testing"
)

// A checksum file of a file larger than the compare limit is stored without
// hashing that file again (N4); each such PUT would hash it once more.
func TestTheChecksumOfALargeFileIsNotCompared(t *testing.T) {
	records := captureLog(t)
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
	compareLimit.Store(8)
	t.Cleanup(func() { compareLimit.Store(0) })
	base := "/repository/demo-releases/com/example/demo-lib/1.0.0/demo-lib-1.0.0"
	for _, c := range []struct {
		target string
		body   []byte
	}{
		{base + ".jar", []byte("PK\x03\x04 a jar")},
		{base + ".jar.sha256", []byte(strings.Repeat("1", 64))},
		{base + ".pom", []byte("<p/>")},
		{base + ".pom.md5", []byte(strings.Repeat("0", 32))},
	} {
		if r := do(t, w.addr(), "PUT", c.target, c.body, "basic", asAdmin); r.status != 201 {
			t.Fatalf("PUT %s: %d %q", c.target, r.status, r.body)
		}
	}
	if r := do(t, w.addr(), "GET", base+".jar.sha256", nil, "basic", asAdmin); string(r.body) != strings.Repeat("1", 64) {
		t.Fatalf("the checksum file is not stored as sent: %q", r.body)
	}
	log := strings.Join(records.all(), "\n")
	jar := "the checksum file com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar.sha256 of repository demo-releases"
	if strings.Contains(log, jar+" does not hold") || !strings.Contains(log, jar+" is not compared") {
		t.Errorf("the large file was compared: %q", log)
	}
	pom := "the checksum file com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom.md5 of repository demo-releases does not hold"
	if !strings.Contains(log, "WARN "+serverLogger+": "+pom) {
		t.Errorf("no warning for the small file: %q", log)
	}
}
