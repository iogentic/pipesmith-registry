package server

import (
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// R10: while the catalog is behind the index, a read that comes from the
// catalog answers 503 naming the scan; the delete of a component walks the
// index instead.
func TestAComponentReadAnswers503WhileTheCatalogIsBehind(t *testing.T) {
	w := startCataloged(t)
	for _, path := range []string{"docs/a.txt", "docs/b.txt"} {
		if _, err := w.ingest("demo-raw", path, "bee"); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := rest.ComponentID("demo-raw", "raw", "docs/b.txt")
	if r := do(t, w.addr(), "GET", "/service/rest/v1/components/"+id, nil, "basic", asAdmin); r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	// the catalog loses a row the index keeps
	if err := w.opts.catalogFile.Delete("demo-raw", "docs/b.txt"); err != nil {
		t.Fatal(err)
	}
	r := do(t, w.addr(), "GET", "/service/rest/v1/components/"+id, nil, "basic", asAdmin)
	if r.status != 503 || !strings.Contains(string(r.body), "scan") || !strings.Contains(string(r.body), "local-write") {
		t.Fatalf("%d %s", r.status, r.body)
	}
	other, _ := rest.ComponentID("demo-raw", "raw", "docs/a.txt")
	if r := do(t, w.addr(), "GET", "/service/rest/v1/components/"+other, nil, "basic", asAdmin); r.status != 503 {
		t.Fatalf("%d %s", r.status, r.body)
	}
}
