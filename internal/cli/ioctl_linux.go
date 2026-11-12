package cli

import "syscall"

const (
	ioctlReadTermios  = syscall.TCGETS
	ioctlWriteTermios = syscall.TCSETS
	// ioctlFlushTermios is TCSETSF, which package syscall does not name: it
	// sets the attributes after it drops the input not read yet, as
	// tcsetattr with TCSAFLUSH. It is TCSETS plus 2 in the asm/ioctls.h of
	// every architecture of linux-libc-dev in the build image (0x5402 and
	// 0x5404 in asm-generic, read 2026-11-12).
	ioctlFlushTermios = syscall.TCSETS + 2
)
