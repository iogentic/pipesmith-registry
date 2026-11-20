package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// The backup of the state file (rulings KN6 and KN12 of leg 6-kinds): a
// consistent copy through VACUUM INTO on a connection of its own, while the
// server writes, into a new file of mode 0600 in a directory outside the
// data directory and the stores. Only state.sqlite is copied, no secret
// file. Every error is a fixed text that names no path.

// BackupName is the base name of a backup made at t.
func BackupName(t time.Time) string {
	return "state-" + t.UTC().Format("20060102T150405Z") + ".sqlite"
}

// Backup copies the state file of dataDir into the new file BackupName(now)
// in the directory location and gives its base name and size (ruling
// KN12). location must be an absolute, existing directory; neither it nor
// a directory above it may be the data directory or the root of a store of
// guarded, compared as files (os.SameFile), and none of them may be open
// to the writes of every user unless the sticky bit is set. The copy is
// made in a new directory of mode 0700 (os.MkdirTemp) inside location and
// linked into place, so an existing name is never written through.
func Backup(ctx context.Context, dataDir, location string, guarded []string, now time.Time) (string, int64, error) {
	if !filepath.IsAbs(location) {
		return "", 0, errors.New("the location of the backup is not an absolute directory")
	}
	if info, err := os.Stat(location); err != nil || !info.IsDir() {
		return "", 0, errors.New("the location of the backup is not an existing directory")
	}
	if forbiddenPlace(location, append([]string{dataDir}, guarded...)) {
		return "", 0, errors.New("the location of the backup lies in the data directory or in a store; it must lie outside both")
	}
	if openToOthers(location) {
		return "", 0, errors.New("other users may change the location of the backup or a directory above it")
	}
	name := BackupName(now)
	if _, err := os.Lstat(filepath.Join(location, name)); err == nil {
		return "", 0, fmt.Errorf("a file named %s exists in the location of the backup already", name)
	}
	work, err := os.MkdirTemp(location, ".state-backup-")
	if err != nil {
		return "", 0, errors.New("the location of the backup takes no new directory")
	}
	defer os.RemoveAll(work) // the work directory goes in every case; the backup is linked out of it
	copied := filepath.Join(work, FileName)
	if err := vacuumInto(ctx, Path(dataDir), copied); err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, errors.New("the state file is not copied")
	}
	if err := os.Chmod(copied, 0o600); err != nil {
		return "", 0, errors.New("the backup file keeps another mode")
	}
	final := filepath.Join(location, name)
	if err := os.Link(copied, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", 0, fmt.Errorf("a file named %s exists in the location of the backup already", name)
		}
		return "", 0, errors.New("the backup file cannot be placed in the location")
	}
	info, err := os.Lstat(final)
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, errors.New("the backup file changed while it was placed")
	}
	return name, info.Size(), nil
}

// forbiddenPlace reports a path that is, or lies below, one of the
// directories given: each directory above it, written as given and with its
// links resolved, is compared with them as a file.
func forbiddenPlace(path string, dirs []string) bool {
	var guarded []os.FileInfo
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil {
			guarded = append(guarded, info)
		}
	}
	candidates := []string{filepath.Clean(path)}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		candidates = append(candidates, real)
	}
	for _, p := range candidates {
		for {
			if info, err := os.Stat(p); err == nil {
				for _, g := range guarded {
					if os.SameFile(info, g) {
						return true
					}
				}
			}
			parent := filepath.Dir(p)
			if parent == p {
				break
			}
			p = parent
		}
	}
	return false
}

// openToOthers reports a directory, or one above it with its links
// resolved, that every user may write and that has no sticky bit.
func openToOthers(path string) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return true
	}
	for p := real; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || info.Mode().Perm()&0o002 != 0 && info.Mode()&os.ModeSticky == 0 {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// vacuumInto copies the database at src into the empty file dst.
func vacuumInto(ctx context.Context, src, dst string) error {
	source, err := dsn(src, url.Values{"mode": {"ro"}, "_pragma": {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)}})
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}
