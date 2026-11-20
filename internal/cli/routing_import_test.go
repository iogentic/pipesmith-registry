package cli

import (
	"strings"
	"testing"
)

// The import of the content fixture counts its routing rules and says that
// state.sqlite took them.
func TestImportNotesTheRoutingRules(t *testing.T) {
	dataDir(t)
	code, out, errOut := run(t, "import", fixtureContent)
	if code != 0 || errOut != "" {
		t.Fatalf("import: %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"repositories 6", "routing rules 3, routing links 1",
		"replaced in ", ": routing rules and their links"} {
		if !strings.Contains(out, want) {
			t.Errorf("import output lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = run(t, "import", fixtureExport)
	if code != 0 || strings.Contains(out, "routing") {
		t.Fatalf("an identity import names routing rules: %d\n%s", code, out)
	}
}
