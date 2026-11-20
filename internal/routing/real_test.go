package routing_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/routing"
)

// RealExport names an export folder of the copied database, written by
// tools/h2export.sh with the content set. No committed file names a value
// of it, and no test sets it. A path that is not absolute counts from the
// root of the repository, whose folders the container of tools/go.sh sees:
//
//	tools/go.sh run 'PSR_REAL_EXPORT=work/<folder> go test -run "^TestRealExportRules$" -v ./internal/routing'
const RealExport = "PSR_REAL_EXPORT"

// Behind PSR_REAL_EXPORT: how many routing rules of the export compile with
// the rules of decision 5.15, as two numbers only. Skipped without the
// variable.
func TestRealExportRules(t *testing.T) {
	dir := os.Getenv(RealExport)
	if dir == "" {
		t.Skip(RealExport + " is not set")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join("..", "..", dir)
	}
	export, err := importer.ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if export.Routing == nil {
		t.Fatal("the export holds no routing rules; export the content set again with the current tools/h2export.sh")
	}
	compile, fail := 0, 0
	for _, rule := range export.Routing.Rules {
		if _, err := routing.Test(rule, "/"); err != nil {
			fail++
		} else {
			compile++
		}
	}
	t.Logf("routing rules that compile: %d, that do not: %d", compile, fail)
}
