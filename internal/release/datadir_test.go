package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update, the switch and rollback take the data directory from data_dir of
// update.json only, and it has to be an existing directory.
func TestDataDirRequired(t *testing.T) {
	for name, member := range map[string]func(r *rig) string{
		"missing":      func(*rig) string { return "" },
		"empty":        func(*rig) string { return `, "data_dir": ""` },
		"not existing": func(r *rig) string { return fmt.Sprintf(`, "data_dir": %q`, filepath.Join(r.data, "none")) },
		"a file": func(r *rig) string {
			file := filepath.Join(r.data, "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				r.t.Fatal(err)
			}
			return fmt.Sprintf(`, "data_dir": %q`, file)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0", "0.0.9")
			if err := os.Symlink("releases/0.0.9", filepath.Join(r.l.Root, PreviousLink)); err != nil {
				t.Fatal(err)
			}
			writeConfig(t, r.l.Root, fmt.Sprintf(`{"trusted_key": %q%s}`, r.s.pub, member(r)))
			dir := r.release(versionScript("0.2.0"))
			r.stage("0.3.0", versionScript("0.3.0"))
			for form, f := range map[string]func() error{
				"update":   func() error { return r.u.Update(dir, false) },
				"switch":   func() error { return r.u.Switch("0.3.0", UpdateOptions{}) },
				"rollback": r.u.Rollback,
			} {
				err := r.unchanged(ErrConfig, f)
				if !strings.Contains(err.Error(), "data_dir") || !strings.Contains(err.Error(), ConfigFile) {
					t.Fatalf("%s: message %q", form, err)
				}
			}
		})
	}
}

// With a migration in the plan the update checks that it can write the data
// directory before any link changes.
func TestDataDirWriteCheck(t *testing.T) {
	migrates := answering("0.2.0", `{"catalog":2,"state":2}`)
	r := newRig(t, "0.1.0")
	r.dataFiles(1, 1)
	r.u.hook = func(p string) error {
		if p == "write-check" {
			return errors.New("read-only file system")
		}
		return nil
	}
	r.refused(ErrSpace, func() error { return r.u.Update(r.release(migrates), false) }, "cannot write", r.data, "read-only file system")

	// Without a migration the check does not run.
	r = newRig(t, "0.1.0")
	r.dataFiles(2, 2)
	checked := 0
	r.u.hook = func(p string) error {
		if p == "write-check" {
			checked++
		}
		return nil
	}
	if err := r.u.Update(r.release(migrates), false); err != nil || checked != 0 {
		t.Fatalf("no migration: %v, %d checks", err, checked)
	}

	// The check leaves no file behind.
	r = newRig(t, "0.1.0")
	r.dataFiles(1, 1)
	r.u.hook = func(p string) error {
		if p == "write-check" {
			checked++
		}
		return nil
	}
	if err := r.u.Update(r.release(migrates), false); err != nil || checked != 1 {
		t.Fatalf("migration: %v, %d checks", err, checked)
	}
	entries, err := os.ReadDir(r.data)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != CatalogFile+","+StateFile {
		t.Fatalf("the data directory holds %v", names)
	}
}
