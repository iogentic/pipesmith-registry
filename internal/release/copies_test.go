package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update and rollback list the copies <file>.v<n> and the files
// <file>.failed of the data directory, which stay until an operator moves
// them (finding R-13).
func TestCopiesAreListed(t *testing.T) {
	r := rollbackRig(t)
	for _, name := range []string{"state.sqlite.v1", "catalog.sqlite.failed", "catalog.sqlite.v12", "other.v1", "state.sqlite.vx"} {
		if err := os.WriteFile(filepath.Join(r.data, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.u.Rollback(); err != nil {
		t.Fatal(err)
	}
	want := "left: catalog.sqlite.failed, catalog.sqlite.v12, state.sqlite.v1 in " + r.data + "; they stay until an operator moves them away\n"
	if !strings.Contains(r.out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, r.out.String())
	}
	r2 := newRig(t, "0.1.0")
	if err := r2.u.Update(r2.release(versionScript("0.2.0")), false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r2.out.String(), "left:") {
		t.Fatalf("a line without copies:\n%s", r2.out.String())
	}
}
