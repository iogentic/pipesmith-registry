package channel

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// markPrefix starts the name of the file below releases/ that marks a
// version whose scheduled switch failed: releases/.switch-failed-<version>.
const markPrefix = ".switch-failed-"

// maxMark bounds the error text a mark holds.
const maxMark = 1000

func markPath(root, version string) (string, error) {
	v, err := release.ParseVersion(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(release.Layout{Root: root}.Releases(), markPrefix+v.String()), nil
}

// MarkSwitchFailed writes the mark of version with the time and cause.
func MarkSwitchFailed(root, version string, cause error) error {
	path, err := markPath(root, version)
	if err != nil {
		return err
	}
	text := cause.Error()
	if len(text) > maxMark {
		text = text[:maxMark]
	}
	return os.WriteFile(path, fmt.Appendf(nil, "%s %s\n", time.Now().UTC().Format(time.RFC3339), text), 0o600)
}

// SwitchFailed reports whether the mark of version exists and returns its
// path.
func SwitchFailed(root, version string) (bool, string, error) {
	path, err := markPath(root, version)
	if err != nil {
		return false, "", err
	}
	_, err = os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, path, nil
	case err != nil:
		return false, path, err
	}
	return true, path, nil
}

// clearMark removes the mark of version, as a fetch that stages it anew does.
func clearMark(root, version string) error {
	path, err := markPath(root, version)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// duePrefix starts the name of the file below releases/ that marks a version
// the scheduled run staged and is about to switch to:
// releases/.switch-due-<version>. It stays when the switch did not begin, as
// after a kill or ErrLocked, so the next scheduled run tries it again
// (finding R3-2).
const duePrefix = ".switch-due-"

func duePath(root, version string) (string, error) {
	v, err := release.ParseVersion(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(release.Layout{Root: root}.Releases(), duePrefix+v.String()), nil
}

// MarkSwitchDue writes the mark of a switch to version that the scheduled
// run is about to start.
func MarkSwitchDue(root, version string) error {
	path, err := duePath(root, version)
	if err != nil {
		return err
	}
	return os.WriteFile(path, fmt.Appendf(nil, "%s\n", time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// SwitchDue reports whether the mark of a due switch to version exists.
func SwitchDue(root, version string) (bool, error) {
	path, err := duePath(root, version)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// ClearSwitchDue removes the mark of a due switch to version.
func ClearSwitchDue(root, version string) error {
	path, err := duePath(root, version)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
