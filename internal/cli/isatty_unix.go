//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal is isatty() of the file: the terminal attributes can be read.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlReadTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
