//go:build !darwin && !linux

package logs

import "os"

// lockFile is not built for this system: the log directory is not locked.
func lockFile(*os.File) error { return nil }
