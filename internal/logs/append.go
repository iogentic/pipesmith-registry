package logs

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// AppendAudit writes one line to audit.log of opts.Dir and closes the files,
// for a command. While a server holds the lock of the directory the line
// goes as one file into audit-pending of the directory, and that server
// moves it into audit.log on its next Tick (pending.go): a line appended
// beside the rotation of that server could be lost. The lock is never
// broken. An empty Dir writes nothing.
func AppendAudit(opts Options, e AuditEvent) error {
	if opts.Dir == "" {
		return nil
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return err
	}
	if held(opts.Dir) {
		return writePending(opts.Dir, e.line(now()), now()) // checked first, so Open does not warn of it
	}
	f := Open(opts)
	if f.off() {
		_ = f.Close() // nothing open; a server took the lock after the check
		return writePending(opts.Dir, e.line(now()), now())
	}
	err := f.Audit(e)
	return errors.Join(err, f.Close())
}

// AuditLine is the line of audit.log of e at now, with its line break.
func AuditLine(e AuditEvent, now time.Time) []byte { return e.line(now) }

// held reports whether another open file holds the lock of dir. The probe
// takes the lock when it is free and gives it back at once.
func held(dir string) bool {
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR|noFollow, 0o600) // #nosec G304 -- the lock file of this package in the log directory of the operator.
	if err != nil {
		return false
	}
	defer func() { _ = lock.Close() }() // the close releases a lock the probe took
	return errors.Is(lockFile(lock), errLocked)
}
