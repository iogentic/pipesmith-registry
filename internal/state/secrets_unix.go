//go:build darwin || linux

package state

import (
	"errors"
	"os"
	"syscall"
)

// ownerOf is the user id of a file.
func ownerOf(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	return int(st.Uid), true
}

// openNoFollow opens a file for reading and refuses a symbolic link at the
// last part of the path, so the checks of ReadSecret see the opened file.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- a secret file of the data directory the operator configured.
}

func isLoop(err error) bool { return errors.Is(err, syscall.ELOOP) }
