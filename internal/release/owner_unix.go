//go:build darwin || linux

package release

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// fileOwner is the uid that owns path.
func fileOwner(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no owner of %s", path)
	}
	return int(st.Uid), nil // #nosec G115 -- a uid of 32 bits fits an int on darwin and linux.
}

// lstatFile is the uid and the mode of path itself, a symlink not followed.
func lstatFile(path string) (fileStat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileStat{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStat{}, fmt.Errorf("no owner of %s", path)
	}
	return fileStat{uid: int(st.Uid), mode: info.Mode()}, nil // #nosec G115 -- a uid of 32 bits fits an int on darwin and linux.
}

// lockRoot takes an exclusive lock on the install root directory itself, so
// the lock adds no file below the root. It does not wait.
func lockRoot(root string) (func(), error) { return lockDir(root, ErrLocked) }

// lockDir takes an exclusive flock on the directory dir without waiting; a
// lock held by another process is the refusal busy.
func lockDir(dir string, busy error) (func(), error) {
	f, err := os.Open(dir) // #nosec G304 -- a directory of the install root.
	if err != nil {
		return nil, fmt.Errorf("%w: %v", busy, err)
	}
	fd := int(f.Fd()) // #nosec G115 -- a file descriptor fits an int.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w on %s; try again when it has ended", busy, dir)
		}
		return nil, fmt.Errorf("%w: lock of %s: %v", busy, dir, err)
	}
	return func() {
		syscall.Flock(fd, syscall.LOCK_UN)
		f.Close()
	}, nil
}

// textBusy reports a start of a file that another process still holds open
// for writing, such as a child forked while the copy was open.
func textBusy(err error) bool { return errors.Is(err, syscall.ETXTBSY) }

// openNonblock opens a FIFO without waiting for a writer.
const openNonblock = syscall.O_NONBLOCK
