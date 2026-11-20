package logs

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

// lockName is the lock file of the log directory. One server holds its
// exclusive lock while its files are open, so a log directory belongs to one
// server: rotation, compression and retention of two servers on one
// directory would lose lines (finding 7a-1).
const lockName = "logs.lock"

// errLocked is the lock held by another open file.
var errLocked = errors.New("the log directory is locked")

// lockDir takes the lock of the log directory. It is false when another
// server holds it: one warning with the directory goes to slog.Default,
// standard error in serve, and the files stay off. A lock file that cannot
// be opened or locked otherwise does not stop the files; their own opens
// fail and count the lines.
func (f *Files) lockDir() bool {
	lock, err := os.OpenFile(filepath.Join(f.dir, lockName), os.O_CREATE|os.O_RDWR|noFollow, 0o600) // #nosec G304 -- the lock file of this package in the log directory of the operator.
	if err != nil {
		return true
	}
	if err := lockFile(lock); err != nil {
		_ = lock.Close() // not locked; nothing to release
		if errors.Is(err, errLocked) {
			slog.Default().Warn("the log directory "+LineText(f.dir)+" is in use by another server; this server writes no log files",
				"logger", "pipesmith_registry.logs")
			return false
		}
		return true
	}
	f.lock = lock
	return true
}

// unlock releases the lock of the log directory.
func (f *Files) unlock() {
	if f.lock != nil {
		_ = f.lock.Close() // the close releases the lock
		f.lock = nil
	}
}
