package release

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The way back to a release directory named otherwise than its version runs
// its binary as the owner of the root only after the owner and mode check of
// rollback: a binary that others may write is not run (finding R3-3).
func TestWayBackChecksTheReleaseBefore(t *testing.T) {
	r := newRig(t, "first")
	ran := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(r.l.Release("first"), InstalledBinary)
	if err := os.WriteFile(bin, script(`touch '`+ran+`'; echo "pipesmith-registry 0.1.0"; exit 0`), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	r.svc.versions = map[string]string{"first": "0.1.0"}
	r.svc.unhealthy["0.2.0"] = true
	r.u.HealthTimeout = 200e6
	err := r.u.Update(r.release(versionScript("0.2.0")), true)
	if err == nil || r.link(CurrentLink) != "releases/first" {
		t.Fatalf("update: %v, current %q\n%s", err, r.link(CurrentLink), r.out.String())
	}
	if _, serr := os.Lstat(ran); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the binary of releases/first ran as the owner of the root: %v\n%s", serr, r.out.String())
	}
	if !strings.Contains(r.out.String(), "not run: releases/first/"+InstalledBinary) || !strings.Contains(r.out.String(), "chmod go-w "+bin) {
		t.Fatalf("output:\n%s", r.out.String())
	}
	// with the mode in order the way back asks the binary for its version
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	r.out.Reset()
	err = r.u.Update(r.release(versionScript("0.2.0")), true)
	if err == nil || !strings.Contains(err.Error(), "went back to releases/first, which answers again") {
		t.Fatalf("second update: %v\n%s", err, r.out.String())
	}
	if _, serr := os.Lstat(ran); serr != nil {
		t.Fatalf("the binary did not run: %v", serr)
	}
}
