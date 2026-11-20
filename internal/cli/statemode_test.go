package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Finding sa-21: serve warns once at start when state.sqlite, which holds
// password hashes, has a right for the group or others; a file of mode 0600
// gives no warning. The start goes on either way.
func TestSecurityWarnsOfAWideStateFile(t *testing.T) {
	dir := importedFixture(t)
	path := state.Path(dir)
	start := func() string {
		t.Helper()
		var log bytes.Buffer
		sec, err := buildSecurity(dir, nil, nil, logTo(&log))
		if err != nil || sec == nil {
			t.Fatalf("%v %v", sec, err)
		}
		closeSecurity(sec)
		return log.String()
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if text := start(); strings.Count(text, "has mode") != 1 || !strings.Contains(text, path+" has mode 0640") ||
		!strings.Contains(text, "chmod 600 "+path) {
		t.Fatalf("mode 0640: log %q", text)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if text := start(); strings.Contains(text, "has mode") {
		t.Fatalf("mode 0600: log %q", text)
	}
}
