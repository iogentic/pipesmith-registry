package datafs

import "syscall"

func statfsType(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Type), true // #nosec G115 -- f_type is a magic number; int64 holds every value of it.
}
