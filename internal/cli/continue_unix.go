//go:build darwin || linux

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// catchContinue catches SIGCONT and drops it, until the returned function
// runs. os/signal blocks every signal it does not watch on a thread of its
// own, SIGCONT among them, and leaves SIGTERM open there. On macOS a SIGTERM
// that arrives while SIGSTOP holds the process waits on that thread; the
// default SIGCONT does not wake the thread, a caught one does.
func catchContinue() (stop func()) {
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGCONT)
	return func() { signal.Stop(caught) }
}
