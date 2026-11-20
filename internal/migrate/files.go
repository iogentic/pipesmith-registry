package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // the driver "sqlite"
)

// FileVersion reads the version of the file at path without writing it: 0
// for a missing file or a file without any table. A file another process
// holds open is read as well. A file without a -wal file is read as
// immutable, so the read leaves no file behind. A -wal or -shm file that is
// no regular file, such as a symlink, is refused before anything is opened.
func FileVersion(path string) (int, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	for _, side := range []string{path + "-wal", path + "-shm"} {
		if info, err := os.Lstat(side); err == nil && !info.Mode().IsRegular() {
			return 0, fmt.Errorf("%s is not a regular file (mode %v); the version of %s is not read", side, info.Mode(), path)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
	}
	params := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(10000)", "query_only(1)"}}
	if _, err := os.Lstat(path + "-wal"); errors.Is(err, fs.ErrNotExist) {
		// no connection holds the file in WAL mode, so none writes it: read
		// it as it is, without the -wal and -shm files a connection makes
		params.Set("immutable", "1")
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?"+params.Encode())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return version(db, path, "schema file")
}

// CopyRequest is the suffix of the file beside a data file by which the
// update asks for the copy of that file before its migration: the update
// checked the space for it and counts on the copy for its way back.
const CopyRequest = ".copy-required"

// CopyRule is the value for Options.CopyWhen of catalog.sqlite: the copy is
// written when <file>.copy-required is a regular file, whatever the free
// space; a copy that does not fit then fails and the file does not migrate.
// Without the request SpareCopy decides.
func CopyRule(free func(dir string) (uint64, error)) func(file string) bool {
	spare := SpareCopy(free)
	return func(file string) bool {
		if info, err := os.Lstat(file + CopyRequest); err == nil && info.Mode().IsRegular() {
			slog.Info(fmt.Sprintf("%s migrates after a copy: the update asked for it with %s", file, filepath.Base(file+CopyRequest)))
			return true
		}
		return spare(file)
	}
}

// SpareCopy is a value for Options.CopyWhen: a copy is written when the free
// space of the folder of the file, as free reports it, is at least twice the
// size of the file. It logs one line either way.
func SpareCopy(free func(dir string) (uint64, error)) func(file string) bool {
	return func(file string) bool {
		info, err := os.Stat(file)
		if err != nil {
			slog.Warn(fmt.Sprintf("%s migrates without a copy: its size is unknown: %v", file, err))
			return false
		}
		size := uint64(info.Size()) // #nosec G115 -- the size of a regular file is not negative.
		avail, err := free(filepath.Dir(file))
		if err != nil {
			slog.Warn(fmt.Sprintf("%s migrates without a copy: the free space is unknown: %v", file, err))
			return false
		}
		if avail/2 >= size {
			slog.Info(fmt.Sprintf("%s migrates after a copy: %d bytes free, %d bytes in the file", file, avail, size))
			return true
		}
		slog.Warn(fmt.Sprintf("%s migrates without a copy: %d bytes free, less than twice the %d bytes of the file", file, avail, size))
		return false
	}
}
