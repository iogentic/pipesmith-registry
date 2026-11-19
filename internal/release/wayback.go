package release

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// wayBackSchema is the way back after a switch whose plan held a migration,
// once the links point back: it stops the unit, links each file that the new
// release migrated as <file>.failed, renames its copy over <file>, and starts
// the old release. <file> exists at every moment. It stops at the first step
// that fails and says what is where; no file is removed but -wal and -shm.
func (u *Updater) wayBackSchema(s *session, created string, failed error) error {
	stopped := func(format string, args ...any) error {
		return fmt.Errorf("%w; went back to %s/%s, then the way back stopped: %s", failed, ReleasesDir, s.current, fmt.Sprintf(format, args...))
	}
	err := u.at("back-stop")
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), orDefault(u.RestartTimeout, s.cfg.restartTimeout()))
		err = u.Service.Stop(ctx, s.cfg.Unit)
		cancel()
	}
	if err != nil {
		return stopped("the unit %s did not stop: %v; no data file was restored, so the old release may refuse the migrated files", s.cfg.Unit, err)
	}
	u.step("stopped: %s", s.cfg.Unit)
	var restored, kept, removed, gone []string
	for _, f := range s.plan.files {
		done, err := u.restoreFile(f)
		if err != nil {
			return stopped("the unit %s is stopped; %v%s", s.cfg.Unit, err, restoredBefore(append(restored, removed...)))
		}
		switch done {
		case fileRestored:
			restored = append(restored, f.name)
		case fileKept:
			kept = append(kept, f.name)
		case fileBackFromCopy:
			removed = append(removed, f.name)
		case fileGone:
			gone = append(gone, f.name)
		}
	}
	if err := u.at("back-start"); err != nil {
		return stopped("the unit %s is stopped and was not started: %v%s", s.cfg.Unit, err, restoredBefore(restored))
	}
	err = u.restartAndWait(s, s.current)
	removeCreated(created)
	words := ""
	if len(restored) > 0 {
		words = fmt.Sprintf("; restored %s from the copies of the migration; changes of state made after the restart are lost; the migrated files are kept as %s",
			strings.Join(restored, ", "), strings.Join(suffixed(restored, ".failed"), ", "))
	}
	if len(kept) > 0 {
		words += fmt.Sprintf("; %s did not change and stay", strings.Join(kept, ", "))
	}
	for _, f := range s.plan.files {
		switch {
		case slices.Contains(removed, f.name):
			words += fmt.Sprintf("; restored %s from its copy %s, as the new release had removed %s", f.name, filepath.Base(f.copyPath()), f.name)
		case slices.Contains(gone, f.name):
			words += fmt.Sprintf("; %s is gone: the new release removed it and left no copy, so %s/%s starts without it", f.name, ReleasesDir, s.current)
			if f.name == CatalogFile {
				words += "; a scan fills again what a scan can"
			}
		}
	}
	if err != nil {
		return fmt.Errorf("%w; went back to %s/%s, which does not answer either%s: %v", failed, ReleasesDir, s.current, words, err)
	}
	return fmt.Errorf("%w; went back to %s/%s, which answers again%s", failed, ReleasesDir, s.current, words)
}

func suffixed(names []string, suffix string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + suffix
	}
	return out
}

func restoredBefore(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "; restored before: " + strings.Join(names, ", ")
}

// restoreResult is what restoreFile did with a file of the plan.
type restoreResult int

const (
	fileKept         restoreResult = iota // the file did not change
	fileRestored                          // the migrated file is <file>.failed, the copy is <file> again
	fileBackFromCopy                      // the new release removed the file, the copy is <file> again
	fileGone                              // the new release removed the file and left no copy
)

// restoreFile puts the copy of f back when the file is at a higher version
// than before, or missing although it existed, and the copy exists.
func (u *Updater) restoreFile(f planFile) (restoreResult, error) {
	if _, err := os.Lstat(f.path); f.from > 0 && errors.Is(err, fs.ErrNotExist) {
		return u.restoreRemoved(f)
	}
	now, err := migrate.FileVersion(f.path)
	if err != nil {
		return fileKept, fmt.Errorf("the version of %s is not readable: %v; %s and its copy %s are as they were", f.path, err, f.name, f.copyPath())
	}
	copyPath := f.copyPath()
	_, err = os.Lstat(copyPath)
	hasCopy := err == nil
	switch {
	case now <= f.from && hasCopy:
		u.step("kept: %s is at schema %d; the copy %s stays", f.name, now, copyPath)
		return fileKept, nil
	case now <= f.from:
		u.step("kept: %s is at schema %d", f.name, now)
		return fileKept, nil
	case !hasCopy:
		return fileKept, fmt.Errorf("%s is at schema %d and has no copy %s, so the old release refuses it; it stays as it is", f.path, now, copyPath)
	}
	if err := u.at("back-wal:" + f.name); err != nil {
		return fileKept, fmt.Errorf("%s is the migrated file and %s the copy: %v", f.path, copyPath, err)
	}
	for _, side := range []string{f.path + "-wal", f.path + "-shm"} {
		if err := os.Remove(side); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fileKept, fmt.Errorf("%s is the migrated file and %s the copy; %v", f.path, copyPath, err)
		}
	}
	failedPath := f.path + ".failed"
	err = u.at("back-failed:" + f.name)
	if err == nil {
		err = os.Link(f.path, failedPath)
	}
	if err != nil {
		return fileKept, fmt.Errorf("%s is the migrated file and %s the copy: %v", f.path, copyPath, err)
	}
	err = u.at("back-copy:" + f.name)
	if err == nil {
		err = os.Rename(copyPath, f.path)
	}
	if err != nil {
		return fileKept, fmt.Errorf("%s is the migrated file, also linked as %s, and the copy %s was not renamed back: %v; rename it to %s before the unit starts",
			f.path, failedPath, copyPath, err, f.path)
	}
	if err := syncDir(filepath.Dir(f.path)); err != nil {
		return fileKept, fmt.Errorf("%s was restored from %s, but its folder did not sync: %v", f.path, copyPath, err)
	}
	u.step("restored: %s from %s; the migrated file is kept as %s", f.name, filepath.Base(copyPath), filepath.Base(failedPath))
	return fileRestored, nil
}

// restoreRemoved handles a file that existed before the switch and that the
// new release removed: its stale -wal and -shm files go, and its copy, when
// there is one, is renamed back to <file>.
func (u *Updater) restoreRemoved(f planFile) (restoreResult, error) {
	copyPath := f.copyPath()
	for _, side := range []string{f.path + "-wal", f.path + "-shm"} {
		if err := os.Remove(side); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fileKept, fmt.Errorf("%s was removed by the new release; %v", f.path, err)
		}
	}
	if _, err := os.Lstat(copyPath); errors.Is(err, fs.ErrNotExist) {
		u.step("gone: %s was removed by the new release, which left no copy %s", f.name, filepath.Base(copyPath))
		return fileGone, nil
	}
	if err := os.Rename(copyPath, f.path); err != nil {
		return fileKept, fmt.Errorf("%s was removed by the new release and its copy %s was not renamed back: %v; rename it to %s before the unit starts",
			f.path, copyPath, err, f.path)
	}
	if err := syncDir(filepath.Dir(f.path)); err != nil {
		return fileKept, fmt.Errorf("%s was restored from %s, but its folder did not sync: %v", f.path, copyPath, err)
	}
	u.step("restored: %s from %s; the new release had removed %s", f.name, filepath.Base(copyPath), f.name)
	return fileBackFromCopy, nil
}
