package release

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rollbackRig runs 0.2.0 with previous pointing to 0.1.0.
func rollbackRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, "0.2.0", "0.1.0")
	if err := os.Symlink("releases/0.1.0", filepath.Join(r.l.Root, PreviousLink)); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRollbackPasses(t *testing.T) {
	r := rollbackRig(t)
	if err := r.u.Rollback(); err != nil {
		t.Fatalf("rollback: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.1.0" || r.link(PreviousLink) != "releases/0.2.0" {
		t.Fatalf("current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
	}
	if r.svc.restartCount() != 1 || r.svc.running != "0.1.0" {
		t.Fatalf("%d restarts, running %q", r.svc.restartCount(), r.svc.running)
	}
	lines := strings.Split(strings.TrimSpace(r.out.String()), "\n")
	for i, prefix := range []string{"owner: ", "current: releases/0.2.0", "previous: releases/0.1.0", "schema: state.sqlite none, catalog.sqlite none",
		"switched: current -> releases/0.1.0, previous -> releases/0.2.0", "restarted: " + DefaultUnit, "healthy: "} {
		if i >= len(lines) || !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d of the output, want %q:\n%s", i+1, prefix, r.out.String())
		}
	}
	r.noLeftovers()
	// A second rollback goes forward again.
	if err := r.u.Rollback(); err != nil {
		t.Fatal(err)
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" {
		t.Fatalf("current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
	}
}

func TestRollbackAfterUpdate(t *testing.T) {
	r := newRig(t, "0.1.0")
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatal(err)
	}
	if err := r.u.Rollback(); err != nil {
		t.Fatal(err)
	}
	if r.link(CurrentLink) != "releases/0.1.0" || r.link(PreviousLink) != "releases/0.2.0" || r.svc.running != "0.1.0" {
		t.Fatalf("current %q previous %q running %q", r.link(CurrentLink), r.link(PreviousLink), r.svc.running)
	}
}

func TestRollbackRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		previous func(root string) error
		message  string
	}{
		"no previous": {func(string) error { return nil }, "no release to go back to"},
		"previous equals current": {func(root string) error {
			return os.Symlink("releases/0.2.0", filepath.Join(root, PreviousLink))
		}, "both point to releases/0.2.0"},
		"previous to nothing": {func(root string) error {
			return os.Symlink("releases/0.0.1", filepath.Join(root, PreviousLink))
		}, "not an installed release directory"},
		"previous a file": {func(root string) error {
			return os.WriteFile(filepath.Join(root, PreviousLink), []byte("releases/0.1.0"), 0o644)
		}, "not a symlink"},
		"previous absolute": {func(root string) error {
			return os.Symlink(filepath.Join(root, ReleasesDir, "0.1.0"), filepath.Join(root, PreviousLink))
		}, "not to releases/<version>"},
		"previous without binary": {func(root string) error {
			if err := os.Remove(filepath.Join(root, ReleasesDir, "0.1.0", InstalledBinary)); err != nil {
				return err
			}
			return os.Symlink("releases/0.1.0", filepath.Join(root, PreviousLink))
		}, "holds no pipesmith-registry"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.2.0", "0.1.0")
			if err := c.previous(r.l.Root); err != nil {
				t.Fatal(err)
			}
			err := r.unchanged(ErrLayout, r.u.Rollback)
			if !strings.Contains(err.Error(), c.message) {
				t.Fatalf("message %q, want %q", err, c.message)
			}
		})
	}
	t.Run("no current", func(t *testing.T) {
		r := rollbackRig(t)
		if err := os.Remove(filepath.Join(r.l.Root, CurrentLink)); err != nil {
			t.Fatal(err)
		}
		r.unchanged(ErrLayout, r.u.Rollback)
	})
	t.Run("no update.json", func(t *testing.T) {
		r := rollbackRig(t)
		if err := os.Remove(filepath.Join(r.l.Root, ConfigFile)); err != nil {
			t.Fatal(err)
		}
		r.unchanged(ErrConfig, r.u.Rollback)
	})
}

func TestRollbackWayBack(t *testing.T) {
	t.Run("health", func(t *testing.T) {
		r := rollbackRig(t)
		r.u.HealthTimeout = 300 * time.Millisecond
		r.svc.unhealthy["0.1.0"] = true
		before := tree(t, r.l.Root)
		err := r.u.Rollback()
		r.back(err, ErrHealth, before)
		if !strings.Contains(err.Error(), "went back to releases/0.2.0, which answers again") {
			t.Fatalf("message %q", err)
		}
	})
	t.Run("restart", func(t *testing.T) {
		r := rollbackRig(t)
		r.svc.failNext = 1
		before := tree(t, r.l.Root)
		err := r.u.Rollback()
		r.back(err, ErrRestart, before)
	})
	t.Run("swap", func(t *testing.T) {
		r := rollbackRig(t)
		r.u.hook = func(p string) error {
			if p == "swap-current" {
				return os.ErrPermission
			}
			return nil
		}
		r.unchanged(ErrInstall, r.u.Rollback)
	})
}

func (r *rig) back(err, want error, before string) {
	r.t.Helper()
	if !errors.Is(err, want) {
		r.t.Fatalf("error %v, want %v", err, want)
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" {
		r.t.Fatalf("current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
	}
	if after := tree(r.t, r.l.Root); after != before {
		r.t.Fatalf("the tree changed:\nbefore\n%s\nafter\n%s", before, after)
	}
	if r.svc.restartCount() != 2 || r.svc.running != "0.2.0" {
		r.t.Fatalf("%d restarts, running %q", r.svc.restartCount(), r.svc.running)
	}
}

// rollback asks the release before for schema and refuses, before any
// change, when a data file is newer than that release writes.
func TestRollbackSchemaRule(t *testing.T) {
	prevAnswers := func(r *rig, schema string) {
		r.t.Helper()
		bin := schemaScript(`echo "pipesmith-registry 0.1.0"; exit 0`, schema)
		if err := os.WriteFile(filepath.Join(r.l.Release("0.1.0"), InstalledBinary), bin, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	t.Run("newer file", func(t *testing.T) {
		r := rollbackRig(t)
		prevAnswers(r, `echo '{"catalog":1,"state":1}'; exit 0`)
		r.dataFiles(2, 1)
		copyPath := filepath.Join(r.data, StateFile+".v1")
		if err := os.WriteFile(copyPath, []byte("copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.refused(ErrSchema, r.u.Rollback, filepath.Join(r.data, StateFile), "schema 2", "releases/0.2.0", "releases/0.1.0", "writes schema 1",
			copyPath, "loses every change made since the update")
		if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" {
			t.Fatalf("current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
		}
	})
	t.Run("files fit", func(t *testing.T) {
		r := rollbackRig(t)
		prevAnswers(r, `echo '{"catalog":1,"state":2}'; exit 0`)
		r.dataFiles(2, 1)
		if err := r.u.Rollback(); err != nil {
			t.Fatalf("rollback: %v\n%s", err, r.out.String())
		}
		if r.link(CurrentLink) != "releases/0.1.0" || r.svc.restartCount() != 1 {
			t.Fatalf("current %q, %d restarts", r.link(CurrentLink), r.svc.restartCount())
		}
	})
	t.Run("no schema command", func(t *testing.T) {
		r := rollbackRig(t)
		prevAnswers(r, `echo "error: pipesmith-registry: argument command: invalid choice: 'schema'" >&2; exit 2`)
		r.dataFiles(2, 2)
		if err := r.u.Rollback(); err != nil {
			t.Fatalf("rollback: %v\n%s", err, r.out.String())
		}
		if r.link(CurrentLink) != "releases/0.1.0" || r.svc.restartCount() != 1 {
			t.Fatalf("current %q, %d restarts", r.link(CurrentLink), r.svc.restartCount())
		}
		if !strings.Contains(r.out.String(), "does not know the command schema") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
}
