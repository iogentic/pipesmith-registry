package channel

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoProductImportOfTestTools keeps the stub of the forge and the test
// support package out of the product: no file below cmd/ or internal/ that
// is not a test file imports them.
func TestNoProductImportOfTestTools(t *testing.T) {
	forbidden := []string{"/tools/forgestub", "/internal/release/releasetest"}
	checked := 0
	for _, top := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if strings.Contains(filepath.ToSlash(path), "/internal/release/releasetest/") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range forbidden {
					if strings.Contains(p, bad) {
						t.Errorf("%s imports %s", path, p)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 50 {
		t.Fatalf("only %d files checked", checked)
	}
}
