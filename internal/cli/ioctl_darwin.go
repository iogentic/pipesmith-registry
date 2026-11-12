package cli

import "syscall"

const (
	ioctlReadTermios  = syscall.TIOCGETA
	ioctlWriteTermios = syscall.TIOCSETA
	// ioctlFlushTermios sets the attributes after it drops the input not
	// read yet, as tcsetattr with TCSAFLUSH
	ioctlFlushTermios = syscall.TIOCSETAF
)
