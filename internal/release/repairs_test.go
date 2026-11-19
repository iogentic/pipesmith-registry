package release

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sameVersionRig runs 0.2.0 with previous at 0.1.0; the binary of 0.2.0 is
// the one the release directory holds.
func sameVersionRig(t *testing.T) (*rig, string) {
	t.Helper()
	r := newRig(t, "0.2.0", "0.1.0")
	bin := versionScript("0.2.0")
	if err := os.WriteFile(filepath.Join(r.l.Release("0.2.0"), InstalledBinary), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.1.0", filepath.Join(r.l.Root, PreviousLink)); err != nil {
		t.Fatal(err)
	}
	return r, r.release(bin)
}

// The version current runs, installed again: the links stay, the unit is
// restarted and the health wait runs.
func TestUpdateSameVersionKeepsTheLinks(t *testing.T) {
	r, dir := sameVersionRig(t)
	before := tree(t, r.l.Root)
	if err := r.u.Update(dir, true); err != nil {
		t.Fatalf("update: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" {
		t.Fatalf("current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
	}
	if after := tree(t, r.l.Root); after != before {
		t.Fatalf("the tree changed:\nbefore\n%s\nafter\n%s", before, after)
	}
	if r.svc.restartCount() != 1 || r.svc.running != "0.2.0" {
		t.Fatalf("%d restarts, running %q", r.svc.restartCount(), r.svc.running)
	}
	out := r.out.String()
	if !strings.Contains(out, "links kept: current runs 0.2.0 already\n") || !strings.Contains(out, "healthy: ") || strings.Contains(out, "switched") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestUpdateSameVersionFailureChangesNoLink(t *testing.T) {
	for name, set := range map[string]func(r *rig){
		"health":  func(r *rig) { r.svc.unhealthy["0.2.0"] = true },
		"restart": func(r *rig) { r.svc.failNext = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			r, dir := sameVersionRig(t)
			r.u.HealthTimeout = 200 * time.Millisecond
			set(r)
			before := tree(t, r.l.Root)
			err := r.u.Update(dir, true)
			if !errors.Is(err, ErrHealth) && !errors.Is(err, ErrRestart) {
				t.Fatalf("error %v", err)
			}
			if !strings.Contains(err.Error(), "current and previous are unchanged") {
				t.Fatalf("message %q", err)
			}
			if after := tree(t, r.l.Root); after != before {
				t.Fatalf("the tree changed:\nbefore\n%s\nafter\n%s", before, after)
			}
			if r.svc.restartCount() != 1 {
				t.Fatalf("%d restarts", r.svc.restartCount())
			}
		})
	}
}
