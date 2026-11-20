package release

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// A way back killed between the link of <file>.failed and the rename of the
// copy: the next command finds the record of the switch, finishes the
// restore and starts the old release (finding A-4).
func TestNextCommandRepairsAKilledRestore(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	sr.u.hook = kill("back-copy:" + StateFile)
	if err := sr.u.Update(sr.dir, false); err == nil || !strings.Contains(err.Error(), "killed at") {
		t.Fatalf("killed way back: %v", err)
	}
	file := sr.path(StateFile)
	if _, err := os.Stat(file + ".failed"); err != nil {
		t.Fatalf("no link %s.failed after the kill: %v", file, err)
	}
	sr.u.hook = nil
	starts := sr.svc.oldStarts
	err := sr.u.Update(sr.dir, false)
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "went back to releases/0.1.0, which answers again") {
		t.Fatalf("next command: %v\n%s", err, sr.out.String())
	}
	for _, name := range []string{StateFile, CatalogFile} {
		if got := dump(t, sr.path(name)); got != sr.before[name] {
			t.Fatalf("%s after the repair:\n%s", name, got)
		}
		if !strings.Contains(dump(t, sr.path(name)+".failed"), fmt.Sprintf("meta: schema %d", schemaN+1)) {
			t.Fatalf("%s.failed holds no migrated file", name)
		}
	}
	if sr.svc.oldStarts <= starts || len(sr.svc.oldRefusal) != 0 || sr.link(CurrentLink) != "releases/0.1.0" {
		t.Fatalf("old starts %d, refusals %v, current %q", sr.svc.oldStarts-starts, sr.svc.oldRefusal, sr.link(CurrentLink))
	}
	sr.noPending()
}

// A <file>.failed that is not the migrated file stops the repair with a
// message that names both, and nothing is renamed.
func TestRepairRefusesAnotherFailedFile(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	sr.u.hook = kill("back-copy:" + StateFile)
	if err := sr.u.Update(sr.dir, false); err == nil {
		t.Fatal("killed way back passed")
	}
	file := sr.path(StateFile)
	if err := os.Remove(file + ".failed"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+".failed", []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	sr.u.hook = nil
	err := sr.u.Update(sr.dir, false)
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), file+".failed exists and is not") {
		t.Fatalf("next command: %v", err)
	}
	if _, err := os.Stat(fmt.Sprintf("%s.v%d", file, schemaN)); err != nil {
		t.Fatalf("the copy was moved: %v", err)
	}
}
