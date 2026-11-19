//go:build darwin || linux

package migrate

import "syscall"

// FreeSpace is the space of the file system of dir that an unprivileged
// process may use, in bytes.
func FreeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // #nosec G115 -- block counts and sizes are not negative.
}
