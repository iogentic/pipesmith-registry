package cli

import "syscall"

const (
	ioctlReadTermios  = syscall.TIOCGETA
	ioctlWriteTermios = syscall.TIOCSETA
	// ioctlFlushTermios sets the attributes after it drops the input not
	// read yet, as tcsetattr with TCSAFLUSH
	ioctlFlushTermios = syscall.TIOCSETAF
)

// dropQueuedInput has nothing left to drop: a write to the master side of a
// pseudo terminal hands each byte to the line discipline before it returns
// (ptcwrite in bsd/kern/tty_dev.c of xnu, read 2026-11-18), so TIOCSETAF has
// dropped every key typed before the prompt.
func dropQueuedInput(uintptr) error { return nil }
