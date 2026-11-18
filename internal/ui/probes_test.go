package ui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The probes of preview.js tell the interface which areas the server does not
// serve yet. In 0.1.1 the page waited for all of them after every sign in and
// every reload. The probe of the search screen was a search. A search reads
// every row of the index when nothing matches, so on a large index the page
// stood still until that walk ended. These two tests read the embedded
// scripts as text and only catch those two forms of the defect. The proof of
// the behaviour is the browser test tests/page/check_probes.py.

func script(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(files, "web/js/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestNoProbeIsASearch turns red when a probe of preview.js asks a search.
func TestNoProbeIsASearch(t *testing.T) {
	src := script(t, "preview.js")
	if strings.Contains(src, "api.search") {
		t.Error("preview.js calls api.search; no probe may be a search")
	}
	probes := 0
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "probe: () =>") {
			continue
		}
		probes++
		if strings.Contains(strings.ToLower(line), "search") {
			t.Errorf("a probe of preview.js asks a search: %s", strings.TrimSpace(line))
		}
	}
	if probes == 0 {
		t.Error("preview.js holds no probe of the form `probe: () =>`, so the check above saw nothing")
	}
}

// TestShellDoesNotAwaitTheProbes turns red when app.js waits for the probes.
func TestShellDoesNotAwaitTheProbes(t *testing.T) {
	src := script(t, "app.js")
	if waits := regexp.MustCompile(`await\s+(runProbes|preview\.probe)\(`).FindAllString(src, -1); len(waits) > 0 {
		t.Errorf("app.js waits for the probes %d times: %q", len(waits), waits)
	}
	if !regexp.MustCompile(`\brunProbes\(\);`).MatchString(src) {
		t.Error("app.js starts no probe, which a server without the areas answer needs")
	}
}
