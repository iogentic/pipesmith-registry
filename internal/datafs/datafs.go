// Package datafs tells whether a directory lies on a file system where SQLite
// cannot share one database between processes: a Docker Desktop file share,
// FUSE, NFS or SMB. Measured on 2026-11-13 with a Docker Desktop bind mount
// (fakeowner): a scan and a server that started and stopped during it, two
// processes on one index, lost the tables of the scan; on ext4 they did not.
package datafs

import (
	"os"
	"path/filepath"
)

// kinds maps the f_type of statfs(2) to the name of the file system.
var kinds = map[int64]string{
	0x6a656a63: "fakeowner", // Docker Desktop file sharing of the host
	0x65735546: "fuse",
	0x6969:     "nfs",
	0x517b:     "smb",
	0xfe534d42: "smb2",
	0xff534d42: "cifs",
}

func kindOf(magic int64) string { return kinds[magic] }

// Shared is the name of the file system of dir when it is one of those, else
// "". A directory that does not exist yet is judged by its nearest existing
// parent. On other systems than Linux it is always "".
func Shared(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	magic, ok := statfsType(dir)
	if !ok {
		return ""
	}
	return kindOf(magic)
}
