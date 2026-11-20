package release

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The free space falls between the space check of the update and the start
// of the new release: the copy of catalog.sqlite that the update counted on
// is written all the same, and the failed update ends with the old release
// running on the catalog of before (finding A-5).
func TestCatalogCopyAfterTheSpaceFell(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	sr.svc.catalogFree = func(string) (uint64, error) { return 1, nil }
	err := sr.u.Update(sr.dir, false)
	if !errors.Is(err, ErrHealth) || !strings.Contains(err.Error(), "which answers again") {
		t.Fatalf("update: %v\n%s", err, sr.out.String())
	}
	for _, name := range []string{StateFile, CatalogFile} {
		if got := dump(t, sr.path(name)); got != sr.before[name] {
			t.Fatalf("%s after the way back:\n%s", name, got)
		}
	}
	if sr.link(CurrentLink) != "releases/0.1.0" || sr.svc.oldStarts == 0 || len(sr.svc.oldRefusal) != 0 {
		t.Fatalf("current %q, old starts %d, refusals %v", sr.link(CurrentLink), sr.svc.oldStarts, sr.svc.oldRefusal)
	}
	if _, err := os.Lstat(sr.path(CatalogFile) + ".copy-required"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the request of the copy stays: %v", err)
	}
}

// With --no-catalog-backup and too little space for the copy of
// catalog.sqlite the update asks for no copy; with the space it does.
func TestNoCatalogBackupAsksForNoCopy(t *testing.T) {
	for free, want := range map[uint64]bool{20000: false, 1 << 30: true} {
		sr := newSchemaRig(t, schemaN+1)
		sr.u.freeSpace = func(string) (uint64, error) { return free, nil }
		_ = sr.u.UpdateWith(sr.dir, UpdateOptions{NoCatalogBackup: true})
		if got := strings.Contains(sr.out.String(), "asked: catalog.sqlite.copy-required"); got != want {
			t.Fatalf("%d bytes free: asked %v\n%s", free, got, sr.out.String())
		}
	}
}

// A request of a copy that a killed update left in the data directory does
// not reach the start of an update with --no-catalog-backup that asks for no
// copy: the update removes it before the switch (finding R3-5).
func TestStaleCopyRequestIsRemoved(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	request := sr.path(CatalogFile) + ".copy-required"
	if err := os.WriteFile(request, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sr.u.freeSpace = func(string) (uint64, error) { return 20000, nil }
	var seen error
	sr.u.hook = func(p string) error {
		if p == "kill-after-switch" {
			_, seen = os.Lstat(request)
		}
		return nil
	}
	_ = sr.u.UpdateWith(sr.dir, UpdateOptions{NoCatalogBackup: true})
	if !errors.Is(seen, os.ErrNotExist) {
		t.Fatalf("the request at the switch: %v\n%s", seen, sr.out.String())
	}
	if !strings.Contains(sr.out.String(), "switched: current -> releases/0.2.0") {
		t.Fatalf("no switch:\n%s", sr.out.String())
	}
}
