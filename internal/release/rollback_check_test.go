package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSums writes SHA256SUMS into releases/<name>/ with the line of the
// platform binary for data.
func (r *rig) writeSums(name string, data []byte) {
	r.t.Helper()
	line := hexSum(data) + "  " + platformBinary() + "\n"
	if err := os.WriteFile(filepath.Join(r.l.Release(name), SumsFile), []byte(line), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// rollback runs the binary of the release before only when that directory,
// the binary and SHA256SUMS belong to the owner of the root or uid 0 and
// neither group nor others may write them, and when the binary has the
// SHA-256 that SHA256SUMS lists for the platform (finding A-2).
func TestRollbackChecksTheReleaseBefore(t *testing.T) {
	bin := versionScript("0.1.0")
	t.Run("sums fit", func(t *testing.T) {
		r := rollbackRig(t)
		r.writeSums("0.1.0", bin)
		if err := r.u.Rollback(); err != nil {
			t.Fatalf("rollback: %v\n%s", err, r.out.String())
		}
		if !strings.Contains(r.out.String(), "checked: releases/0.1.0/pipesmith-registry has the SHA-256 that SHA256SUMS lists for "+platformBinary()) {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
	t.Run("other sum", func(t *testing.T) {
		r := rollbackRig(t)
		r.writeSums("0.1.0", []byte("another binary"))
		err := r.unchanged(ErrDigest, r.u.Rollback)
		for _, w := range []string{"releases/0.1.0/pipesmith-registry", SumsFile, "rollback changed nothing"} {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("message %q lacks %q", err, w)
			}
		}
	})
	t.Run("no line of the platform", func(t *testing.T) {
		r := rollbackRig(t)
		if err := os.WriteFile(filepath.Join(r.l.Release("0.1.0"), SumsFile), []byte(hexSum(bin)+"  other-name\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r.unchanged(ErrNotListed, r.u.Rollback)
	})
	t.Run("no sums", func(t *testing.T) {
		r := rollbackRig(t)
		if err := r.u.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if !strings.Contains(r.out.String(), "releases/0.1.0 holds no SHA256SUMS; the binary is not compared") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
	for name, path := range map[string]func(r *rig) string{
		"directory": func(r *rig) string { return r.l.Release("0.1.0") },
		"binary":    func(r *rig) string { return filepath.Join(r.l.Release("0.1.0"), InstalledBinary) },
		"sums":      func(r *rig) string { return filepath.Join(r.l.Release("0.1.0"), SumsFile) },
	} {
		t.Run(name+" writable by the group", func(t *testing.T) {
			r := rollbackRig(t)
			r.writeSums("0.1.0", bin)
			p := path(r)
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, info.Mode().Perm()|0o020); err != nil {
				t.Fatal(err)
			}
			err = r.unchanged(ErrPermissions, r.u.Rollback)
			if !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "chmod go-w") {
				t.Fatalf("message %q", err)
			}
		})
	}
}
