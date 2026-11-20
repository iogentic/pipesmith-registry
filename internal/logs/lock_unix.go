//go:build darwin || linux

package logs

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes the exclusive lock of lock without waiting; errLocked when
// another open file holds it.
func lockFile(lock *os.File) error {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a file descriptor fits in an int.
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
