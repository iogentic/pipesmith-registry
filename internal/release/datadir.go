package release

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// RequireDataDir is data_dir of update.json, which has to name an existing
// directory. update and rollback take the data directory of the unit from
// there and never from the environment of the caller.
func RequireDataDir(cfg *UpdateConfig) (string, error) {
	if cfg.DataDir == "" {
		return "", fmt.Errorf("%w: %s has no data_dir; set it to the data directory of the unit", ErrConfig, cfg.Path)
	}
	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		return "", fmt.Errorf("%w: data_dir of %s: %v", ErrConfig, cfg.Path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: data_dir of %s: %s is not a directory", ErrConfig, cfg.Path, cfg.DataDir)
	}
	return cfg.DataDir, nil
}

// writeCheck creates a file in the data directory, links it, renames the
// link and removes both, when the plan holds a migration: the way back
// links the migrated file and renames the copy there.
func (u *Updater) writeCheck(p *schemaPlan) error {
	if len(p.files) == 0 {
		return nil
	}
	refuse := func(step string, err error) error {
		return fmt.Errorf("%w: cannot write %s (%s): %v; the way back of a migration writes there", ErrSpace, p.dataDir, step, err)
	}
	if err := u.at("write-check"); err != nil {
		return refuse("create", err)
	}
	f, err := os.CreateTemp(p.dataDir, ".pipesmith-registry-write-check-")
	if err != nil {
		return refuse("create", err)
	}
	name := f.Name()
	link, renamed := name+".link", name+".renamed"
	defer func() {
		for _, n := range []string{name, link, renamed} {
			os.Remove(n)
		}
	}()
	if err := f.Close(); err != nil {
		return refuse("create", err)
	}
	err = u.at("write-check-link")
	if err == nil {
		err = os.Link(name, link)
	}
	if err != nil {
		return refuse("hard link", err)
	}
	err = u.at("write-check-rename")
	if err == nil {
		err = os.Rename(link, renamed)
	}
	if err != nil {
		return refuse("rename", err)
	}
	for _, n := range []string{name, renamed} {
		if err := os.Remove(n); err != nil {
			return refuse("remove", err)
		}
	}
	return nil
}

// requestCopies writes <file>.copy-required beside each file of the plan
// whose copy the space check counted, so the start of the new release writes
// that copy whatever the free space is then. A request a killed run left
// beside another file of the plan goes first, so the start copies only what
// this update counted (finding R3-5).
func (u *Updater) requestCopies(p *schemaPlan) error {
	for _, f := range p.files {
		if f.name == CatalogFile && f.copy {
			continue
		}
		path := f.path + migrate.CopyRequest
		err := os.Remove(path)
		switch {
		case err == nil:
			u.step("removed: %s, a request of a copy that a run which stopped left", filepath.Base(path))
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%w: %v", ErrSpace, err)
		}
	}
	for _, f := range p.files {
		if f.name != CatalogFile || !f.copy {
			continue
		}
		path := f.path + migrate.CopyRequest
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %v", ErrSpace, err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G304 G302 -- the request beside a data file; the server reads its name only.
		if err == nil {
			err = file.Close()
		}
		if err == nil {
			err = syncDir(filepath.Dir(path))
		}
		if err == nil {
			err = u.at("copy-rule")
		}
		if err != nil {
			os.Remove(path)
			return fmt.Errorf("%w: the request %s: %v", ErrSpace, path, err)
		}
		u.step("asked: %s for the copy %s", filepath.Base(path), filepath.Base(f.copyPath()))
	}
	return nil
}

// dropRequests removes the requests of requestCopies.
func (u *Updater) dropRequests(p *schemaPlan) {
	for _, f := range p.files {
		if err := os.Remove(f.path + migrate.CopyRequest); err != nil && !errors.Is(err, fs.ErrNotExist) {
			u.step("warning: %s%s was not removed: %v", f.path, migrate.CopyRequest, err)
		}
	}
}

var copyName = regexp.MustCompile(`^(state|catalog)\.sqlite\.(v[1-9][0-9]*|failed)$`)

// listCopies prints the copies <file>.v<n> and the files <file>.failed of
// the data directory in one line, when there are any. They stay until an
// operator moves them away (docs/RUNBOOK-HOST.md, section Copies).
func (u *Updater) listCopies(dataDir string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if copyName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) > 0 {
		u.step("left: %s in %s; they stay until an operator moves them away", strings.Join(names, ", "), dataDir)
	}
}
